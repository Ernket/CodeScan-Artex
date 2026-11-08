package githubsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// allowedHosts 是唯一允许出站的域名集合。
//
// objects.githubusercontent.com 是 GitHub 归档的实际对象存储：codeload 有时会把
// 请求 302 到那里。除此之外不接受任何主机——白名单越短，SSRF 面越小。
var allowedHosts = map[string]bool{
	"github.com":                    true,
	"www.github.com":                true,
	"api.github.com":                true,
	"codeload.github.com":           true,
	"objects.githubusercontent.com": true,
}

const (
	defaultAPIBase      = "https://api.github.com"
	defaultCodeloadBase = "https://codeload.github.com"
	// 仓库元数据的缓存时长。一次导入最多查两次（首次估算 + 确认后重发），
	// 缓存让第二次不再消耗 60/h 的配额。
	repoCacheTTL = 5 * time.Minute
	// 单次 API 响应体上限：元数据只有几百字节，超过这个数一定不是正常响应。
	apiBodyLimit = 1 << 20
)

// Client talks to the GitHub API and codeload. It is safe for concurrent use.
type Client struct {
	HC           *http.Client
	APIBase      string
	CodeloadBase string

	// allowInsecure 允许 http 的 APIBase（仅测试用，生产恒为 false）。
	allowInsecure bool
	// allowLoopback 允许拨号到环回/内网地址（仅测试用）。
	allowLoopback bool
	// extraAllowedHost 是白名单之外的额外主机（仅测试用，指向 httptest 服务）。
	extraAllowedHost string
	// viaProxy 表示确实配置了代理。此时拨号目标是代理本身（通常是内网地址），
	// IP 级闸门无法生效，只剩下域名白名单 —— 这一取舍在 NewClient 的注释里说明。
	viaProxy bool

	mu    sync.Mutex
	cache map[string]repoCacheEntry
}

type repoCacheEntry struct {
	info RepoInfo
	exp  time.Time
}

// NewClient builds the shared client. proxy is an optional explicit proxy URL;
// empty means direct. The platform's global proxy is deliberately NOT reused:
// that one carries offensive traffic to targets and is often an unstable
// tunnel, while this client only fetches source from github.com.
func NewClient(proxy string) *Client {
	if v := strings.TrimSpace(os.Getenv("ARTEX_GITHUB_ALLOW_LOOPBACK")); v == "1" || strings.EqualFold(v, "true") {
		// 只给集成测试用，生产环境不应设置。
		return newClient(proxy, defaultAPIBase, defaultCodeloadBase, true, true)
	}
	return newClient(proxy, defaultAPIBase, defaultCodeloadBase, false, false)
}

func newClient(proxy, apiBase, codeloadBase string, allowInsecure, allowLoopback bool) *Client {
	c := &Client{
		APIBase:       strings.TrimRight(apiBase, "/"),
		CodeloadBase:  strings.TrimRight(codeloadBase, "/"),
		allowInsecure: allowInsecure,
		allowLoopback: allowLoopback,
		cache:         map[string]repoCacheEntry{},
	}
	if allowInsecure {
		if u, err := url.Parse(apiBase); err == nil {
			c.extraAllowedHost = u.Hostname()
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   c.dialControl,
	}).DialContext
	tr.ResponseHeaderTimeout = 30 * time.Second
	// 明确忽略环境变量里的 HTTP(S)_PROXY：本功能只出站到 GitHub，是否经由代理
	// 必须显式配置。继承环境变量会让「拨号守卫」把代理自己当成内网地址拒绝掉，
	// 而且运维不知道流量走了哪里。
	tr.Proxy = nil
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
			c.viaProxy = true
		}
	}
	c.HC = &http.Client{
		Transport:     tr,
		CheckRedirect: c.checkRedirect,
	}
	return c
}

// RepoInfo is the subset of GET /repos/{owner}/{repo} this package needs.
type RepoInfo struct {
	// Name is GitHub's canonical repo name (its casing may differ from the URL).
	// The zip archive's wrapper directory is "{Name}-{ref}", so the exact string
	// matters when stripping that wrapper.
	Name string `json:"name"`
	// SizeKB is the repository size in kilobytes, including history. It is an
	// estimate only: measured 39% larger than the generated zip on a real repo.
	SizeKB int64 `json:"size"`
}

// Repo fetches repository metadata. Results are cached for repoCacheTTL.
func (c *Client) Repo(ctx context.Context, sp Spec) (*RepoInfo, error) {
	key := strings.ToLower(sp.String())
	if info := c.cacheGet(key); info != nil {
		return info, nil
	}
	u := fmt.Sprintf("%s/repos/%s/%s", c.APIBase, url.PathEscape(sp.Owner), url.PathEscape(sp.Repo))
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := classify(resp); err != nil {
		return nil, err
	}
	var info RepoInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, apiBodyLimit)).Decode(&info); err != nil {
		return nil, fmt.Errorf("解析仓库信息失败: %w", err)
	}
	if info.Name == "" {
		info.Name = sp.Repo
	}
	c.cachePut(key, info)
	return &info, nil
}

// ResolveCommit turns a branch/tag name (or empty = default branch) into a full
// 40-char commit SHA, so the import is reproducible: the same URL yields the same
// tree tomorrow.
func (c *Client) ResolveCommit(ctx context.Context, sp Spec) (string, error) {
	ref := sp.Ref
	if ref == "" {
		ref = "HEAD"
	} else {
		// 分支名可以含 "/"，逐段转义后再拼，否则会把路径结构改掉。
		parts := strings.Split(ref, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		ref = strings.Join(parts, "/")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.APIBase,
		url.PathEscape(sp.Owner), url.PathEscape(sp.Repo), ref)
	resp, err := c.get(ctx, u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// GitHub 对空仓库返回 409 "Git Repository is empty."
		return "", ErrEmptyRepo
	}
	if err := classify(resp); err != nil {
		// 分支/标签不存在时 GitHub 的返回码有两种：404（ref 形状合法但对不上）
		// 和 422（ref 或 sha 无法解析，实测 /commits/.hidden、/commits/000...0 都是
		// 422）。两者都必须落到 ErrRefNotFound —— 否则用户把分支名打错会看到
		// 「拉取失败，请稍后重试」，而真正的原因（分支不存在）被吞掉。
		if sp.Ref != "" && (errors.Is(err, ErrRepoNotFound) || isUnprocessable(resp.StatusCode)) {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, apiBodyLimit)).Decode(&body); err != nil {
		return "", fmt.Errorf("解析 commit 失败: %w", err)
	}
	if !IsCommitSHA(body.SHA) {
		return "", fmt.Errorf("GitHub 返回的 commit 不合法")
	}
	return strings.ToLower(body.SHA), nil
}

// OpenArchive streams the zipball for one commit. The caller MUST wrap the body
// in an io.LimitReader: codeload sends chunked responses with no Content-Length
// (measured), so the declared size is -1 and cannot be trusted for limits.
func (c *Client) OpenArchive(ctx context.Context, sp Spec, commit string) (io.ReadCloser, int64, error) {
	u := fmt.Sprintf("%s/%s/%s/zip/%s", c.CodeloadBase,
		url.PathEscape(sp.Owner), url.PathEscape(sp.Repo), url.PathEscape(commit))
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, 0, err
	}
	if err := classify(resp); err != nil {
		resp.Body.Close()
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("构造请求地址失败")
	}
	if err := c.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败")
	}
	req.Header.Set("User-Agent", "artex-github-import")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HC.Do(req)
	if err != nil {
		// 不把原始错误抛给调用方：*url.Error 里带完整 URL（私库 token 场景下
		// 会泄露凭据），这里只保留底层原因。
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			return nil, fmt.Errorf("请求 GitHub 失败: %w", uerr.Err)
		}
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	return resp, nil
}

// checkURL enforces the scheme + host allowlist. It runs before every request and
// on every redirect hop.
func (c *Client) checkURL(u *url.URL) error {
	if u == nil {
		return errors.New("请求地址为空")
	}
	https := strings.EqualFold(u.Scheme, "https")
	if !https && !(c.allowInsecure && strings.EqualFold(u.Scheme, "http")) {
		return fmt.Errorf("拒绝非 https 请求（%s）", u.Scheme)
	}
	if u.User != nil {
		return errors.New("请求地址不能包含凭据")
	}
	host := strings.ToLower(u.Hostname())
	if host != "" && host == strings.ToLower(c.extraAllowedHost) {
		return nil
	}
	if !allowedHosts[host] {
		return fmt.Errorf("拒绝访问非白名单域名 %q", host)
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("拒绝访问非标准端口 %s", p)
	}
	return nil
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("重定向次数过多")
	}
	return c.checkURL(req.URL)
}

// dialControl rejects connections to loopback / private / link-local addresses at
// connect time. This is the layer that survives DNS rebinding: the name resolved
// to a public IP when we validated it, and to 169.254.169.254 when we connect.
func (c *Client) dialControl(_, address string, _ syscall.RawConn) error {
	// 走代理时目标地址是代理本身，IP 检查没有意义（代理通常在私网）。
	// 这种情况下 SSRF 防护退化为「域名白名单 + 逐跳重定向校验」，其余两层仍在。
	if c.allowLoopback || c.viaProxy {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("无法解析目标地址 %q", host)
	}
	if blockedIP(ip) {
		return fmt.Errorf("拒绝连接内网地址 %s", ip)
	}
	return nil
}

func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// CGNAT 100.64.0.0/10：IsPrivate 不覆盖，但云上常用作内网段。
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// isUnprocessable reports GitHub's 422 "cannot resolve this ref/sha" answer.
func isUnprocessable(code int) bool {
	return code == http.StatusUnprocessableEntity
}

// classify maps an unsuccessful response onto a typed error so the HTTP layer can
// show a readable Chinese message instead of a status code.
func classify(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrRepoNotFound
	case http.StatusConflict:
		return ErrEmptyRepo
	// 451 Unavailable For Legal Reasons：DMCA 下架的仓库。它存在，只是内容被屏蔽，
	// 报成「仓库不存在」会让用户反复确认自己是不是把名字打错了。
	case http.StatusUnavailableForLegalReasons:
		return ErrRepoBlocked
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusForbidden, http.StatusUnauthorized:
		if strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" {
			return ErrRateLimited
		}
		return fmt.Errorf("GitHub 拒绝访问（HTTP %d）", resp.StatusCode)
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("GitHub 服务异常（HTTP %d）", resp.StatusCode)
		}
		return fmt.Errorf("GitHub 返回异常状态（HTTP %d）", resp.StatusCode)
	}
}

func (c *Client) cacheGet(key string) *RepoInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Now().After(e.exp) {
		return nil
	}
	info := e.info
	return &info
}

func (c *Client) cachePut(key string, info RepoInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]repoCacheEntry{}
	}
	c.cache[key] = repoCacheEntry{info: info, exp: time.Now().Add(repoCacheTTL)}
}
