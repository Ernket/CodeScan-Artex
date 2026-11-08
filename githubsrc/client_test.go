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
	c := newClient("", srv.URL, srv.URL, true, true)
	return c, srv
}

func TestCheckURL(t *testing.T) {
	c := newClient("", defaultAPIBase, defaultCodeloadBase, false, false)

	ok := []string{
		"https://api.github.com/repos/o/r",
		"https://codeload.github.com/o/r/zip/abc",
		"https://objects.githubusercontent.com/x",
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
	_, err := c.Repo(context.Background(), Spec{Owner: "o", Repo: "r"})
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
		_, err := c.Repo(context.Background(), Spec{Owner: "o", Repo: "r"})
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
		info, err := c.Repo(context.Background(), Spec{Owner: "o", Repo: "r"})
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

	got, err := c.ResolveCommit(context.Background(), Spec{Owner: "o", Repo: "r", Ref: "feat/a/b"})
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if got != sha {
		t.Errorf("SHA 应统一成小写，得到 %s", got)
	}
	if !strings.Contains(gotPath, "/commits/feat/a/b") {
		t.Errorf("含斜杠的分支名应保留层级，实际路径 %s", gotPath)
	}

	if _, err := c.ResolveCommit(context.Background(), Spec{Owner: "o", Repo: "r", Ref: "nope"}); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("不存在的分支应为 ErrRefNotFound，得到 %v", err)
	}
	// 422 同样要归到 ErrRefNotFound：分支名打错时 GitHub 给的是 422。
	if _, err := c.ResolveCommit(context.Background(), Spec{Owner: "o", Repo: "r", Ref: "unprocessable"}); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("422 应为 ErrRefNotFound，得到 %v", err)
	}
	// 默认分支（Ref 为空）拿到 404 时不能谎报成分支不存在。
	if _, err := c.ResolveCommit(context.Background(), Spec{Owner: "o", Repo: "nope"}); !errors.Is(err, ErrRepoNotFound) {
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
	rc, size, err := c.OpenArchive(context.Background(), Spec{Owner: "o", Repo: "r"}, strings.Repeat("a", 40))
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
