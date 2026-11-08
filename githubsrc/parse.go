// Package githubsrc fetches a public GitHub or GitLab repository's source tree
// without requiring git: it resolves a ref to a commit SHA through the REST API
// and downloads the archive from the host's archive endpoint.
//
// 安全边界：调用方给的地址只会被解析成 {provider, path, ref}，之后所有出站请求都用
// 常量模板重新拼接（见 Client 的方法）。客户端另做三层校验——scheme/host 白名单、
// 逐跳重定向校验、拨号期 IP 检查（client.go）。
package githubsrc

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	// ErrInvalidURL 表示输入不是受支持的仓库地址。
	ErrInvalidURL = errors.New("invalid repository url")
	// ErrRepoNotFound 表示仓库不存在。未认证请求下私有仓库同样返回 404，
	// 因此本错误语义上是「不存在或不可见」。
	ErrRepoNotFound = errors.New("repo not found")
	// ErrRefNotFound 表示仓库存在但指定的分支/标签/commit 不存在。
	ErrRefNotFound = errors.New("ref not found")
	// ErrRateLimited 表示代码托管接口限流。
	ErrRateLimited = errors.New("rate limited")
	// ErrEmptyRepo 表示仓库没有任何提交。
	ErrEmptyRepo = errors.New("empty repo")
	// ErrRepoBlocked 表示仓库被 GitHub 以合规原因屏蔽（HTTP 451）。
	// 实测：DMCA 下架的仓库对 /repos/{o}/{r} 返回 451，而不是 404 —— 它确实存在，
	// 只是拒绝提供内容，所以文案必须和「仓库不存在」区分开。
	ErrRepoBlocked = errors.New("repo blocked")
)

// Provider identifies the code host a Spec points at. The zero value is GitHub:
// every Spec built before GitLab support only carried owner/repo, and treating
// zero as GitHub keeps those call sites valid.
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
)

// Spec is a parsed repository reference. Path is the full project path
// ("owner/repo" on GitHub, "group/subgroup/project" on GitLab); Ref empty means
// "default branch".
type Spec struct {
	Provider Provider
	Path     string
	Ref      string
}

func (s Spec) String() string { return s.Path }

// Owner is the first path segment. GitHub always has exactly two segments; on
// GitLab this is the top-level namespace, which is only informational.
func (s Spec) Owner() string {
	if i := strings.IndexByte(s.Path, '/'); i > 0 {
		return s.Path[:i]
	}
	return ""
}

// Repo is the last path segment (the project slug).
func (s Spec) Repo() string {
	if i := strings.LastIndexByte(s.Path, '/'); i >= 0 && i+1 < len(s.Path) {
		return s.Path[i+1:]
	}
	return s.Path
}

func (s Spec) provider() Provider {
	if s.Provider == ProviderGitLab {
		return ProviderGitLab
	}
	return ProviderGitHub
}

// WebURL is the canonical browser URL of the repository, used for the task's
// source_url (so the UI links back to the right host).
func (s Spec) WebURL() string {
	host := "github.com"
	if s.provider() == ProviderGitLab {
		host = "gitlab.com"
	}
	return "https://" + host + "/" + s.Path
}

// ParseRepoURL accepts the URL shapes users actually paste:
//
//	https://github.com/{owner}/{repo}[.git][/]
//	https://github.com/{owner}/{repo}/tree/{ref}          (ref may contain "/")
//	https://gitlab.com/{group}[/{subgroup}...]/{project}[.git][/]
//	https://gitlab.com/{group}/.../{project}/-/tree/{ref}
//
// Everything else is rejected: non-https schemes, hosts that merely contain
// "github.com", userinfo, ports, query strings, fragments, and dot segments.
// Rejecting is deliberate — a permissive parser would hand the rest of this
// package an attacker-chosen host.
func ParseRepoURL(raw string) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Spec{}, ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return Spec{}, ErrInvalidURL
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return Spec{}, fmt.Errorf("%w: 只支持 https 地址", ErrInvalidURL)
	}
	if u.User != nil {
		return Spec{}, fmt.Errorf("%w: 地址不能包含用户名或凭据", ErrInvalidURL)
	}
	if u.Port() != "" {
		return Spec{}, fmt.Errorf("%w: 地址不能带端口", ErrInvalidURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Spec{}, fmt.Errorf("%w: 地址不能带查询参数或锚点", ErrInvalidURL)
	}
	// Hostname() 会去掉端口并做小写归一；用它比对而不是 Contains，否则
	// github.com.evil.com 会被当成合法主机。
	provider := Provider("")
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com":
		provider = ProviderGitHub
	case "gitlab.com", "www.gitlab.com":
		provider = ProviderGitLab
	default:
		return Spec{}, fmt.Errorf("%w: 只支持 github.com 或 gitlab.com 的仓库", ErrInvalidURL)
	}

	// u.Path 已解码，所以 %2e%2e 会还原成 ".."，直接在段上判断即可。
	segs := splitPath(u.Path)
	if provider == ProviderGitLab {
		return parseGitLabPath(segs)
	}
	return parseGitHubPath(segs)
}

func parseGitHubPath(segs []string) (Spec, error) {
	if len(segs) < 2 {
		return Spec{}, fmt.Errorf("%w: 地址应形如 https://github.com/owner/repo", ErrInvalidURL)
	}
	owner, repo := segs[0], segs[1]
	if err := validName(owner, "owner"); err != nil {
		return Spec{}, err
	}
	repo = strings.TrimSuffix(repo, ".git")
	if err := validName(repo, "repo"); err != nil {
		return Spec{}, err
	}

	spec := Spec{Provider: ProviderGitHub, Path: owner + "/" + repo}
	switch len(segs) {
	case 2:
		return spec, nil
	default:
		// 只接受 /tree/{ref}；/blob/、/releases/ 之类不是整仓语义。
		if segs[2] != "tree" {
			return Spec{}, fmt.Errorf("%w: 仅支持仓库根地址或 /tree/{分支}", ErrInvalidURL)
		}
		if len(segs) <= 3 {
			return Spec{}, fmt.Errorf("%w: /tree/ 后缺少分支名", ErrInvalidURL)
		}
		ref, err := joinRef(segs[3:])
		if err != nil {
			return Spec{}, err
		}
		spec.Ref = ref
		return spec, nil
	}
}

// parseGitLabPath 解析 GitLab 的项目地址。与 GitHub 的两点差异：
//   - 命名空间可以多级（group/subgroup/project），GitHub 固定 owner/repo；
//   - 子页面用 /-/ 分隔（/-/tree/main），GitHub 用 /tree/main。
//
// 多级路径是必须支持的：GitLab 上大量项目挂在子组下，只认两段会把它们全部拒掉。
func parseGitLabPath(segs []string) (Spec, error) {
	if len(segs) < 2 {
		return Spec{}, fmt.Errorf("%w: 地址应形如 https://gitlab.com/group/project", ErrInvalidURL)
	}
	project := segs
	var refSegs []string
	for i, s := range segs {
		if s != "-" {
			continue
		}
		project = segs[:i]
		tail := segs[i+1:]
		// 与 GitHub 同口径：只接受 /-/tree/{ref}，/-/blob/、/-/releases/ 不是整仓语义。
		if len(tail) == 0 || tail[0] != "tree" {
			return Spec{}, fmt.Errorf("%w: 仅支持仓库根地址或 /-/tree/{分支}", ErrInvalidURL)
		}
		refSegs = tail[1:]
		if len(refSegs) == 0 {
			return Spec{}, fmt.Errorf("%w: /-/tree/ 后缺少分支名", ErrInvalidURL)
		}
		break
	}
	if len(project) < 2 {
		return Spec{}, fmt.Errorf("%w: 地址应形如 https://gitlab.com/group/project", ErrInvalidURL)
	}
	if len(project) > 20 {
		return Spec{}, fmt.Errorf("%w: 项目路径层级过深", ErrInvalidURL)
	}
	clean := make([]string, len(project))
	for i, s := range project {
		if err := validName(s, "项目路径"); err != nil {
			return Spec{}, err
		}
		clean[i] = s
	}
	clean[len(clean)-1] = strings.TrimSuffix(clean[len(clean)-1], ".git")
	if err := validName(clean[len(clean)-1], "repo"); err != nil {
		return Spec{}, err
	}
	path := strings.Join(clean, "/")
	if len(path) > 200 {
		return Spec{}, fmt.Errorf("%w: 项目路径过长", ErrInvalidURL)
	}

	spec := Spec{Provider: ProviderGitLab, Path: path}
	if refSegs != nil {
		ref, err := joinRef(refSegs)
		if err != nil {
			return Spec{}, err
		}
		spec.Ref = ref
	}
	return spec, nil
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// validName accepts the characters GitHub and GitLab allow in user, namespace and
// repo names. It also rejects "." and ".." so a crafted URL cannot walk out of
// the URL template.
func validName(s, what string) error {
	if s == "" || len(s) > 100 {
		return fmt.Errorf("%w: %s 长度不合法", ErrInvalidURL, what)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("%w: %s 不能是 . 或 ..", ErrInvalidURL, what)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: %s 含非法字符 %q", ErrInvalidURL, what, r)
		}
	}
	return nil
}

func joinRef(segs []string) (string, error) {
	for _, s := range segs {
		if s == "." || s == ".." {
			return "", fmt.Errorf("%w: 分支名不能包含 . 或 .. 段", ErrInvalidURL)
		}
	}
	ref := strings.Join(segs, "/")
	if len(ref) > 200 {
		return "", fmt.Errorf("%w: 分支名过长", ErrInvalidURL)
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: 分支名含控制字符", ErrInvalidURL)
		}
	}
	return ref, nil
}

// NormalizeRef validates a branch/tag/commit typed into the UI. It rejects the
// same things ParseRepoURL rejects in a path, because the value ends up as one.
func NormalizeRef(ref string) (string, error) {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	if ref == "" {
		return "", nil
	}
	segs := strings.Split(ref, "/")
	for _, s := range segs {
		if s == "" {
			return "", fmt.Errorf("%w: 分支名不能有空的路径段", ErrInvalidURL)
		}
	}
	return joinRef(segs)
}

// IsCommitSHA reports whether s looks like a full 40-char commit SHA. A SHA needs
// no API call to resolve, which saves one slot of the unauthenticated quota.
func IsCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
