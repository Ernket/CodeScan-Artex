package githubsrc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := newClient("", srv.URL, srv.URL, srv.URL+"/api/v4", true, true)
	return c, srv
}

func TestCheckURL(t *testing.T) {
	c := newClient("", defaultAPIBase, defaultCodeloadBase, defaultGitLabAPIBase, false, false)

	ok := []string{
		"https://api.github.com/repos/o/r",
		"https://codeload.github.com/o/r/zip/abc",
		"https://objects.githubusercontent.com/x",
		"https://gitlab.com/api/v4/projects/g%2Fp",
		"https://www.gitlab.com/g/p",
	}
	for _, raw := range ok {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err != nil {
			t.Errorf("%s 应当放行: %v", raw, err)
		}
	}

	bad := []string{
		"http://api.github.com/repos/o/r",
		"https://github.com.evil.com/o/r",
		"https://raw.githubusercontent.com/o/r/main/x",
		"https://github.com:8443/o/r",
		"https://user:pass@github.com/o/r",
		"https://127.0.0.1/o/r",
	}
	for _, raw := range bad {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err == nil {
			t.Errorf("%s 应当被拒绝", raw)
		}
	}
}

func TestRedirectToInternalHostIsRejected(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 指向云元数据端点：即使 scheme 是 https，也不在白名单里。
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	_, err := c.Repo(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r"})
	if err == nil {
		t.Fatal("重定向到元数据端点必须失败")
	}
	if !strings.Contains(err.Error(), "白名单") && !strings.Contains(err.Error(), "拒绝") {
		t.Errorf("错误信息应说明被拒绝: %v", err)
	}
}

func TestDialControlBlocksInternalIPs(t *testing.T) {
	c := &Client{}
	blocked := []string{"127.0.0.1:443", "10.0.0.5:443", "172.16.3.4:443", "192.168.1.1:443",
		"169.254.169.254:80", "[::1]:443", "100.64.0.1:443", "0.0.0.0:443"}
	for _, addr := range blocked {
		if err := c.dialControl("tcp", addr, nil); err == nil {
			t.Errorf("拨号到 %s 应当被拒绝", addr)
		}
	}
	if err := c.dialControl("tcp", "140.82.112.3:443", nil); err != nil {
		t.Errorf("公网地址不应被拒绝: %v", err)
	}

	c.allowLoopback = true
	if err := c.dialControl("tcp", "127.0.0.1:443", nil); err != nil {
		t.Errorf("allowLoopback 时应放行: %v", err)
	}
}

func TestRepoStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		header map[string]string
		want   error
	}{
		{http.StatusNotFound, nil, ErrRepoNotFound},
		{http.StatusTooManyRequests, nil, ErrRateLimited},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, ErrRateLimited},
		// 实测 DMCA 下架的仓库返回 451（Ernket/EKPV12）。
		{http.StatusUnavailableForLegalReasons, nil, ErrRepoBlocked},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"message":"x"}`)
		}))
		_, err := c.Repo(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r"})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: 期望 %v，得到 %v", tc.status, tc.want, err)
		}
	}
}

func TestRepoCache(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"name":"Real-Name","size":10866}`)
	}))
	for i := 0; i < 3; i++ {
		info, err := c.Repo(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r"})
		if err != nil {
			t.Fatalf("Repo: %v", err)
		}
		if info.Name != "Real-Name" || info.SizeKB != 10866 {
			t.Fatalf("解析结果不对: %+v", info)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("三次调用应只打一次上游，实际 %d 次", got)
	}
}

func TestResolveCommit(t *testing.T) {
	const sha = "f32a86a0826e5323d633a2c2fb841b108c6174d3"
	var gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "nope") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "unprocessable") {
			// 实测：/repos/{o}/{r}/commits/.hidden 与全 0 的 sha 都返回 422，
			// 不是 404。不映射就会退化成「拉取失败，请稍后重试」。
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"No commit found for SHA"}`)
			return
		}
		_, _ = io.WriteString(w, `{"sha":"`+strings.ToUpper(sha)+`"}`)
	}))

	got, err := c.ResolveCommit(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r", Ref: "feat/a/b"})
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if got != sha {
		t.Errorf("SHA 应统一成小写，得到 %s", got)
	}
	if !strings.Contains(gotPath, "/commits/feat/a/b") {
		t.Errorf("含斜杠的分支名应保留层级，实际路径 %s", gotPath)
	}

	if _, err := c.ResolveCommit(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r", Ref: "nope"}); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("不存在的分支应为 ErrRefNotFound，得到 %v", err)
	}
	// 422 同样要归到 ErrRefNotFound：分支名打错时 GitHub 给的是 422。
	if _, err := c.ResolveCommit(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r", Ref: "unprocessable"}); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("422 应为 ErrRefNotFound，得到 %v", err)
	}
	// 默认分支（Ref 为空）拿到 404 时不能谎报成分支不存在。
	if _, err := c.ResolveCommit(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/nope"}); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("未指定 ref 的 404 应保持 ErrRepoNotFound，得到 %v", err)
	}
}

func TestOpenArchiveStreamsBody(t *testing.T) {
	payload := []byte("PK\x03\x04fake-zip")
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/o/r/zip/") {
			t.Errorf("归档路径不对: %s", r.URL.Path)
		}
		_, _ = w.Write(payload)
	}))
	rc, size, err := c.OpenArchive(context.Background(), Spec{Provider: ProviderGitHub, Path: "o/r"}, strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != string(payload) {
		t.Errorf("内容不一致: %q", body)
	}
	if size != int64(len(payload)) {
		t.Logf("Content-Length = %d（codeload 真实环境为 -1，测试服务端会给出长度）", size)
	}
}

// TestGitLabRepoCommitArchive 盯住 GitLab 的三条 URL 模板与响应字段。
//
// GitLab 与 GitHub 的差异不只是域名：项目路径要多级、且必须以 %2F 形式整体
// 编码进 /projects/{id}；commit 用 id 而不是 sha；归档走 /repository/archive.zip。
// 任何一处拼错都会在真实环境变成 404，所以这里逐条断言请求形态。
func TestGitLabRepoCommitArchive(t *testing.T) {
	const sha = "87e8c32b8918a836c09475e0f668ea7c436d3758"
	payload := []byte("PK\x03\x04gitlab-zip")
	var metaPath, commitPath, archiveURI string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/archive.zip"):
			archiveURI = r.URL.RequestURI()
			_, _ = w.Write(payload)
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			commitPath = r.URL.EscapedPath()
			_, _ = io.WriteString(w, `{"id":"`+strings.ToUpper(sha)+`"}`)
		default:
			metaPath = r.URL.EscapedPath()
			// GitLab 元数据里没有 size/statistics（匿名请求），name 是显示名、
			// path 才是归档包裹目录用的 slug。
			_, _ = io.WriteString(w, `{"path":"project","name":"My Project"}`)
		}
	}))

	sp := Spec{Provider: ProviderGitLab, Path: "group/sub/project"}
	info, err := c.Repo(context.Background(), sp)
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	if info.Name != "project" {
		t.Errorf("包裹目录前缀应取 path 而不是显示名，得到 %q", info.Name)
	}
	if info.SizeKnown || info.SizeKB != 0 {
		t.Errorf("匿名 GitLab 元数据不应有体积估算，得到 size=%d known=%v", info.SizeKB, info.SizeKnown)
	}
	if !strings.HasPrefix(metaPath, "/api/v4/projects/group%2Fsub%2Fproject") {
		t.Errorf("项目路径应在 /api/v4 下并整体编码成 %%2F 形式，实际 %s", metaPath)
	}

	got, err := c.ResolveCommit(context.Background(), sp)
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if got != sha {
		t.Errorf("应读取 GitLab 的 id 并统一小写，得到 %s", got)
	}
	if !strings.HasSuffix(commitPath, "/repository/commits/HEAD") {
		t.Errorf("默认分支应解析为 HEAD，实际 %s", commitPath)
	}

	rc, _, err := c.OpenArchive(context.Background(), sp, sha)
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != string(payload) {
		t.Errorf("归档内容不一致: %q", body)
	}
	// 归档接口同样在 /api/v4 下：少了这个前缀会落到 GitLab 网页路由，实测返回 403。
	if !strings.HasPrefix(archiveURI, "/api/v4/projects/group%2Fsub%2Fproject/repository/archive.zip") ||
		!strings.Contains(archiveURI, "sha="+sha) {
		t.Errorf("归档地址不对: %s", archiveURI)
	}
}

// TestResolveProviderPrefersGitLab 盯住自定义 host 的探测顺序：GitLab 先于
// GitHub Enterprise，且命中后按 origin 缓存，不会每次导入都重复探测。
func TestResolveProviderPrefersGitLab(t *testing.T) {
	var glHits, ghHits int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
			atomic.AddInt32(&glHits, 1)
			_, _ = io.WriteString(w, `{"path":"r","name":"R"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/"):
			atomic.AddInt32(&ghHits, 1)
			_, _ = io.WriteString(w, `{"name":"R","size":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	c.setExtraHosts([]string{"127.0.0.1"})
	c.allowPrivateNetwork = true

	sp := Spec{Provider: ProviderAuto, Path: "o/r", Origin: srv.URL}
	got, err := c.ResolveProvider(context.Background(), sp)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got.Provider != ProviderGitLab || got.Origin != srv.URL {
		t.Errorf("应识别为 GitLab 并保留 origin，得到 %+v", got)
	}
	if glHits != 1 || ghHits != 0 {
		t.Errorf("应先探测 GitLab 且不碰 GHES，实际 gl=%d gh=%d", glHits, ghHits)
	}

	// 第二次：平台缓存 + 元数据缓存，都不应再打上游。
	if _, err := c.ResolveProvider(context.Background(), sp); err != nil {
		t.Fatalf("ResolveProvider 二次: %v", err)
	}
	if glHits != 1 || ghHits != 0 {
		t.Errorf("缓存失效，实际 gl=%d gh=%d", glHits, ghHits)
	}
}

// TestResolveProviderFallsBackToGitHubEnterprise 盯住 GitLab 探测失败后的降级，
// 以及 GHES 的 /api/v3 + /zipball 模板。
func TestResolveProviderFallsBackToGitHubEnterprise(t *testing.T) {
	const sha = "87e8c32b8918a836c09475e0f668ea7c436d3758"
	payload := []byte("PK\x03\x04ghes-zip")
	var metaHits int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v4/"):
			http.NotFound(w, r)
		case r.URL.Path == "/api/v3/repos/o/r":
			atomic.AddInt32(&metaHits, 1)
			_, _ = io.WriteString(w, `{"name":"r","size":2}`)
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/commits/"):
			_, _ = io.WriteString(w, `{"sha":"`+strings.ToUpper(sha)+`"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/zipball/"):
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	c.setExtraHosts([]string{"127.0.0.1"})
	c.allowPrivateNetwork = true

	sp := Spec{Provider: ProviderAuto, Path: "o/r", Origin: srv.URL}
	got, err := c.ResolveProvider(context.Background(), sp)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got.Provider != ProviderGitHub {
		t.Fatalf("应降级为 GitHub Enterprise，得到 %+v", got)
	}
	if _, err := c.Repo(context.Background(), got); err != nil {
		t.Fatalf("Repo: %v", err)
	}
	if metaHits != 1 {
		t.Errorf("探测结果应被元数据缓存复用，实际请求 %d 次", metaHits)
	}
	commit, err := c.ResolveCommit(context.Background(), got)
	if err != nil || commit != sha {
		t.Fatalf("ResolveCommit = %q, %v", commit, err)
	}
	rc, _, err := c.OpenArchive(context.Background(), got, commit)
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != string(payload) {
		t.Errorf("归档内容不一致: %q", body)
	}
}

func TestResolveProviderUnknownPlatform(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	sp := Spec{Provider: ProviderAuto, Path: "o/r", Origin: srv.URL}
	if _, err := c.ResolveProvider(context.Background(), sp); !errors.Is(err, ErrPlatformUnknown) {
		t.Fatalf("期望 ErrPlatformUnknown，得到 %v", err)
	}
}

// TestResolveProviderDeepPathIsGitLab 盯住「多级路径只可能是 GitLab」这一捷径，
// 避免为自建 GitLab 多打一次探测请求。
func TestResolveProviderDeepPathIsGitLab(t *testing.T) {
	var hits int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"path":"project","name":"P"}`)
	}))
	sp := Spec{Provider: ProviderAuto, Path: "group/sub/project", Origin: srv.URL}
	got, err := c.ResolveProvider(context.Background(), sp)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got.Provider != ProviderGitLab {
		t.Errorf("三段的路径应直接判定为 GitLab，得到 %+v", got)
	}
	if hits != 0 {
		t.Errorf("不应发起探测请求，实际 %d 次", hits)
	}
}

func TestCheckURLExtraHostsAllowHTTPAndAnyPort(t *testing.T) {
	c := newClient("", defaultAPIBase, defaultCodeloadBase, defaultGitLabAPIBase, false, false)
	c.setExtraHosts([]string{"gitlab.corp.local", "192.168.1.50"})

	ok := []string{
		"http://gitlab.corp.local:8080/api/v4/projects/g%2Fp",
		"https://gitlab.corp.local/x",
		"http://192.168.1.50:3000/o/r",
	}
	for _, raw := range ok {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err != nil {
			t.Errorf("%s 应当放行: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://other.corp.local/x", "http://github.com/o/r"} {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err == nil {
			t.Errorf("%s 应当被拒绝", raw)
		}
	}
}

// TestDialControlAllowPrivateNetwork 盯住「放行内网但仍拦元数据」这条边界。
func TestDialControlAllowPrivateNetwork(t *testing.T) {
	c := &Client{allowPrivateNetwork: true}
	for _, addr := range []string{"10.0.0.5:443", "192.168.1.50:8080", "127.0.0.1:3000", "[::1]:443"} {
		if err := c.dialControl("tcp", addr, nil); err != nil {
			t.Errorf("%s 在允许内网时应放行: %v", addr, err)
		}
	}
	for _, addr := range []string{"169.254.169.254:80", "[fe80::1]:443", "[fd00:ec2::254]:80"} {
		if err := c.dialControl("tcp", addr, nil); err == nil {
			t.Errorf("%s 是元数据地址，必须始终拦截", addr)
		}
	}
}

// TestResolveProviderKeepsRateLimit 盯住探测阶段的错误保真：真实 GitLab 被限流
// 时不能报成「平台无法识别」。
func TestResolveProviderKeepsRateLimit(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/projects/") {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	sp := Spec{Provider: ProviderAuto, Path: "o/r", Origin: srv.URL}
	if _, err := c.ResolveProvider(context.Background(), sp); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，得到 %v", err)
	}
}

// TestResolveProviderLegacyGitLabURL 盯住 GitLab 旧链接：/group/sub/tree/main
// 与 GHES 网页形态相同，必须靠探测识别成 GitLab，而不是直接拒绝。
func TestResolveProviderLegacyGitLabURL(t *testing.T) {
	var glHits int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/projects/") {
			atomic.AddInt32(&glHits, 1)
			_, _ = io.WriteString(w, `{"path":"sub","name":"Sub"}`)
			return
		}
		http.NotFound(w, r)
	}))
	sp, err := ParseRepoURLWithHosts("http://127.0.0.1/group/sub/tree/main", []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("ParseRepoURLWithHosts: %v", err)
	}
	sp.Origin = srv.URL
	got, err := c.ResolveProvider(context.Background(), sp)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got.Provider != ProviderGitLab || got.Ref != "main" || glHits != 1 {
		t.Errorf("旧链接应识别为 GitLab 并保留 ref，得到 %+v / hits=%d", got, glHits)
	}
}
