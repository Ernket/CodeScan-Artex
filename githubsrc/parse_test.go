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
		{"https://github.com/Ernket/CodeScan-Artex", Spec{"Ernket", "CodeScan-Artex", ""}},
		{"https://github.com/Ernket/CodeScan-Artex.git", Spec{"Ernket", "CodeScan-Artex", ""}},
		{"https://github.com/Ernket/CodeScan-Artex/", Spec{"Ernket", "CodeScan-Artex", ""}},
		{"  https://github.com/Ernket/CodeScan-Artex  ", Spec{"Ernket", "CodeScan-Artex", ""}},
		{"https://www.github.com/o/r", Spec{"o", "r", ""}},
		{"https://GitHub.com/O/R", Spec{"O", "R", ""}},
		{"https://github.com/o/r/tree/main", Spec{"o", "r", "main"}},
		{"https://github.com/o/r/tree/feat/a/b", Spec{"o", "r", "feat/a/b"}},
		{"https://github.com/o/r/tree/v1.2.3", Spec{"o", "r", "v1.2.3"}},
		{"https://github.com/o/r/tree/f32a86a0826e5323d633a2c2fb841b108c6174d3", Spec{"o", "r", "f32a86a0826e5323d633a2c2fb841b108c6174d3"}},
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
		"ftp://github.com/o/r",
		"https://github.com.evil.com/o/r",
		"https://notgithub.com/o/r",
		"https://evilgithub.com/o/r",
		"https://x@github.com/o/r",
		"https://github.com:8443/o/r",
		"https://github.com/o/r?x=1",
		"https://github.com/o/r#readme",
		"https://github.com/o",
		"https://github.com/",
		"https://github.com/o/../r",
		"https://github.com/o/%2e%2e/r",
		"https://github.com/o/r/blob/main/a.go",
		"https://github.com/o/r/tree",
		"https://github.com/o/r/tree/..",
		"https://127.0.0.1/o/r",
		"git@github.com:o/r.git",
		"https://gitlab.com/o/r",
		"https://github.com/o/r/",
	}
	for _, in := range reject {
		if in == "https://github.com/o/r/" {
			continue // 末尾斜杠是接受的，列在这里仅作对照
		}
		if _, err := ParseRepoURL(in); err == nil {
			t.Errorf("ParseRepoURL(%q) 应当拒绝，却通过了", in)
		}
	}
}

func TestParseRepoURLErrorsAreTyped(t *testing.T) {
	_, err := ParseRepoURL("https://gitlab.com/o/r")
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("期望 ErrInvalidURL，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "github.com") {
		t.Errorf("错误信息应说明只支持 github.com: %v", err)
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
