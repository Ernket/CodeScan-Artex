package githubsrc

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	accept := []struct {
		in   string
		want Spec
	}{
		// ---- GitHub ----
		{"https://github.com/Ernket/CodeScan-Artex", Spec{ProviderGitHub, "Ernket/CodeScan-Artex", ""}},
		{"https://github.com/Ernket/CodeScan-Artex.git", Spec{ProviderGitHub, "Ernket/CodeScan-Artex", ""}},
		{"https://github.com/Ernket/CodeScan-Artex/", Spec{ProviderGitHub, "Ernket/CodeScan-Artex", ""}},
		{"  https://github.com/Ernket/CodeScan-Artex  ", Spec{ProviderGitHub, "Ernket/CodeScan-Artex", ""}},
		{"https://www.github.com/o/r", Spec{ProviderGitHub, "o/r", ""}},
		{"https://GitHub.com/O/R", Spec{ProviderGitHub, "O/R", ""}},
		{"https://github.com/o/r/tree/main", Spec{ProviderGitHub, "o/r", "main"}},
		{"https://github.com/o/r/tree/feat/a/b", Spec{ProviderGitHub, "o/r", "feat/a/b"}},
		{"https://github.com/o/r/tree/v1.2.3", Spec{ProviderGitHub, "o/r", "v1.2.3"}},
		{"https://github.com/o/r/tree/f32a86a0826e5323d633a2c2fb841b108c6174d3", Spec{ProviderGitHub, "o/r", "f32a86a0826e5323d633a2c2fb841b108c6174d3"}},

		// ---- GitLab ----
		{"https://gitlab.com/gitlab-org/gitlab-runner", Spec{ProviderGitLab, "gitlab-org/gitlab-runner", ""}},
		{"https://gitlab.com/gitlab-org/gitlab-runner.git", Spec{ProviderGitLab, "gitlab-org/gitlab-runner", ""}},
		{"https://gitlab.com/gitlab-org/gitlab-runner/", Spec{ProviderGitLab, "gitlab-org/gitlab-runner", ""}},
		{"https://www.gitlab.com/pages/plain-html", Spec{ProviderGitLab, "pages/plain-html", ""}},
		{"https://GitLab.com/Group/Sub/Project", Spec{ProviderGitLab, "Group/Sub/Project", ""}},
		{"https://gitlab.com/pages/plain-html/-/tree/main", Spec{ProviderGitLab, "pages/plain-html", "main"}},
		{"https://gitlab.com/group/sub/project/-/tree/feat/a/b", Spec{ProviderGitLab, "group/sub/project", "feat/a/b"}},
		{"https://gitlab.com/group/sub/project/-/tree/v1.2.3", Spec{ProviderGitLab, "group/sub/project", "v1.2.3"}},
	}
	for _, c := range accept {
		got, err := ParseRepoURL(c.in)
		if err != nil {
			t.Errorf("ParseRepoURL(%q) 应当接受，却报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRepoURL(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}

	reject := []string{
		"",
		"   ",
		"http://github.com/o/r",
		"http://gitlab.com/g/p",
		"ftp://github.com/o/r",
		"https://github.com.evil.com/o/r",
		"https://gitlab.com.evil.com/g/p",
		"https://notgithub.com/o/r",
		"https://evilgithub.com/o/r",
		"https://x@github.com/o/r",
		"https://x@gitlab.com/g/p",
		"https://github.com:8443/o/r",
		"https://gitlab.com:8443/g/p",
		"https://github.com/o/r?x=1",
		"https://gitlab.com/g/p?x=1",
		"https://github.com/o/r#readme",
		"https://gitlab.com/g/p#readme",
		"https://github.com/o",
		"https://github.com/",
		"https://github.com/o/../r",
		"https://github.com/o/%2e%2e/r",
		"https://github.com/o/r/blob/main/a.go",
		"https://github.com/o/r/tree",
		"https://github.com/o/r/tree/..",
		"https://127.0.0.1/o/r",
		"git@github.com:o/r.git",
		"https://gitee.com/o/r",
		"https://bitbucket.org/o/r",
		// GitLab 特有的非法形态
		"https://gitlab.com/group",
		"https://gitlab.com/",
		"https://gitlab.com/-/tree/main",
		"https://gitlab.com/group/-/tree/main",
		"https://gitlab.com/group/project/-/blob/main/a.go",
		"https://gitlab.com/group/project/-/tree",
		"https://gitlab.com/group/project/-/tree/..",
		"https://gitlab.com/group/project/-/archive/main/x.zip",
		"git@gitlab.com:group/project.git",
	}
	for _, in := range reject {
		if _, err := ParseRepoURL(in); err == nil {
			t.Errorf("ParseRepoURL(%q) 应当拒绝，却通过了", in)
		}
	}
}

func TestParseRepoURLErrorsAreTyped(t *testing.T) {
	_, err := ParseRepoURL("https://gitee.com/o/r")
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("期望 ErrInvalidURL，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "github.com") || !strings.Contains(err.Error(), "gitlab.com") {
		t.Errorf("错误信息应说明支持的两个域名: %v", err)
	}
}

func TestSpecHelpers(t *testing.T) {
	gh := Spec{Provider: ProviderGitHub, Path: "owner/repo"}
	if gh.Owner() != "owner" || gh.Repo() != "repo" || gh.WebURL() != "https://github.com/owner/repo" {
		t.Errorf("GitHub Spec 辅助方法不对: %+v", gh)
	}
	gl := Spec{Provider: ProviderGitLab, Path: "group/sub/project"}
	if gl.Owner() != "group" || gl.Repo() != "project" || gl.WebURL() != "https://gitlab.com/group/sub/project" {
		t.Errorf("GitLab Spec 辅助方法不对: %+v", gl)
	}
	// 零值 Provider 按 GitHub 处理（历史调用只带 owner/repo）。
	zero := Spec{Path: "o/r"}
	if zero.WebURL() != "https://github.com/o/r" {
		t.Errorf("零值 Provider 应视为 GitHub，得到 %s", zero.WebURL())
	}
}

func TestIsCommitSHA(t *testing.T) {
	ok := "f32a86a0826e5323d633a2c2fb841b108c6174d3"
	if !IsCommitSHA(ok) {
		t.Fatal("40 位 SHA 应被识别")
	}
	for _, bad := range []string{"", "main", "f32a86a", strings.Repeat("z", 40), ok + "a"} {
		if IsCommitSHA(bad) {
			t.Errorf("%q 不应被识别为 SHA", bad)
		}
	}
}
