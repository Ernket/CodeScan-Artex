package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// newGithubTestServer 造一个只用于路由/校验断言的 Server。
//
// ctx 先取消：这几个用例不碰网络，也不该让 janitor / worker 去动真实库。
func newGithubTestServer(t *testing.T) *Server {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, dataDir, dataDir, dataDir)
	return s
}

// testUploadID 造一个不与库里已有行冲突的 staging id。
func testUploadID(tag string) string {
	return "test-" + tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// stageTestSource 造一个 ready 的 staging 上传（含一个占位源码文件），返回 upload_id。
//
// 建任务现在对**所有账号**都强制要求源码，凡是走真实 HTTP /api/tasks 的用例都必须
// 先经过这一步，否则拿到的是 400——这正是本意，不要在测试里绕开后端。
func stageTestSource(t *testing.T, s *Server, tag string) string {
	t.Helper()
	id := testUploadID(tag)
	dir := filepath.Join(s.m.dir, uploadDirName, id, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 staging 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("写占位源码失败: %v", err)
	}
	up, err := s.m.pg.TaskUploads().CreateTaskUpload(db.NewTaskUpload{
		ID:         id,
		Filename:   tag + ".zip",
		SourcePath: dir,
		State:      db.UploadStateReady,
		Origin:     "upload",
	})
	if err != nil {
		t.Fatalf("建 staging 行失败: %v", err)
	}
	t.Cleanup(func() {
		_ = s.m.pg.TaskUploads().DeleteTaskUpload(up.ID)
		_ = os.RemoveAll(filepath.Join(s.m.dir, uploadDirName, id))
	})
	return up.ID
}

// TestHandlerRegistersUploadRoutes 是路由表的回归测试。
//
// Go 1.22+ 的 ServeMux 遇到二义模式（例如 /api/tasks/uploads/{id} 与
// /api/tasks/{id}/coverage）会在**注册时 panic**，而注册发生在 Handler() 里。
// 因此「能不能构造出 Handler」本身就是断言：新路由若写成 /api/tasks/uploads/{id}，
// 这里会直接 panic。
func TestHandlerRegistersUploadRoutes(t *testing.T) {
	s := newGithubTestServer(t)
	h := s.Handler() // 二义模式会在这里 panic
	if h == nil {
		t.Fatal("Handler() 返回 nil")
	}
	token := testAdminToken(t, s)
	// 三个新接口都必须真的接上：不存在的路由会拿到 404 page not found，
	// 而非法入参应该是 400。
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/tasks/import/github", `{"repo_url":"not-a-url"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/tasks/import/github", `{"repo_url":"https://gitlab.com/a/b"}`, http.StatusBadRequest},
		// 未登录 → 401（鉴权中间件在业务校验之前）。
		{http.MethodGet, "/api/task-uploads/abc", ``, http.StatusUnauthorized},
		{http.MethodPost, "/api/task-uploads/abc/confirm", `{"accept":true}`, http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if tc.want != http.StatusUnauthorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: 期望 %d 实得 %d（%s）", tc.method, tc.path, tc.want, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "page not found") {
			t.Errorf("%s %s 没有注册到路由表", tc.method, tc.path)
		}
	}
}

// TestAttachUploadStateGate 守住「源码是否就绪」这道唯一闸门。
//
// 前端不可信：用 fetching / awaiting_confirm / failed 的 id 建任务，搬过去的
// 要么是不存在的目录，要么是半成品。
func TestAttachUploadStateGate(t *testing.T) {
	s := newGithubTestServer(t)
	store := s.m.pg.TaskUploads()

	cases := []struct {
		state string
		want  string
	}{
		{db.UploadStateFetching, "源码尚未就绪"},
		{db.UploadStateAwaitingConfirm, "请先在页面确认"},
		{db.UploadStateFailed, "源码导入失败"},
	}
	var ids []string
	for _, tc := range cases {
		up, err := store.CreateTaskUpload(db.NewTaskUpload{
			ID:         testUploadID("gate-" + tc.state),
			Filename:   "t/gate-" + tc.state,
			SourcePath: filepath.Join(s.m.dir, uploadDirName, "gate-"+tc.state, "source"),
			State:      tc.state,
			Origin:     "github",
		})
		if err != nil {
			t.Fatalf("建行失败(%s): %v", tc.state, err)
		}
		ids = append(ids, up.ID)
		err = s.attachUploadToTask(nil, &Task{ID: "1"}, up.ID)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("state=%s 应被拒并提示 %q，实际 err=%v", tc.state, tc.want, err)
		}
	}
	// 不存在的 id 也不能当成「没有源码」放过。
	if err := s.attachUploadToTask(nil, &Task{ID: "1"}, "no-such-upload"); err == nil {
		t.Error("不存在的 upload_id 应报错")
	}

	// staging 是个人资源：别人（非管理员）拿着 upload_id 也不能把源码搬进自己任务。
	// created_by 有外键，先建一个真实账号。
	ownerUser, err := s.m.pg.Users().CreateUser(testUploadID("owner"), "", "x", false, nil)
	if err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.Users().DeleteUser(ownerUser.ID) })
	owner := ownerUser.ID
	other, err := store.CreateTaskUpload(db.NewTaskUpload{
		ID:         testUploadID("other-owner"),
		Filename:   "t/other-owner",
		SourcePath: filepath.Join(s.m.dir, uploadDirName, "other-owner", "source"),
		State:      db.UploadStateReady,
		Origin:     "github",
		CreatedBy:  &owner,
	})
	if err != nil {
		t.Fatalf("建行失败(other): %v", err)
	}
	ids = append(ids, other.ID)
	if err := s.attachUploadToTask(&db.User{ID: ownerUser.ID + 1000}, &Task{ID: "1"}, other.ID); err == nil ||
		!strings.Contains(err.Error(), "无权使用他人的上传") {
		t.Errorf("冒用他人 upload_id 应被拒，实际 err=%v", err)
	}
	// 管理员可以用（这里只验证它过了归属校验，后续因目录不存在而失败是预期的）。
	if err := s.attachUploadToTask(&db.User{ID: ownerUser.ID + 1000, IsAdmin: true}, &Task{ID: "1"}, other.ID); err != nil &&
		strings.Contains(err.Error(), "无权使用他人的上传") {
		t.Errorf("管理员不应被归属校验拦住，实际 err=%v", err)
	}
	for _, id := range ids {
		_ = store.DeleteTaskUpload(id)
	}
}

// TestUploadJanitorReclaimsExpiredStaging 覆盖四种行的回收与两种必须保留的行。
func TestUploadJanitorReclaimsExpiredStaging(t *testing.T) {
	s := newGithubTestServer(t)
	store := s.m.pg.TaskUploads()

	type row struct {
		id    string
		state string
		age   time.Duration
		dir   string
	}
	mk := func(state string, age time.Duration) row {
		up, err := store.CreateTaskUpload(db.NewTaskUpload{
			ID:         testUploadID("janitor-" + state + age.String()),
			Filename:   "t/janitor-" + state,
			SourcePath: filepath.Join(s.m.dir, uploadDirName, "janitor-"+state+age.String(), "source"),
			State:      state,
			Origin:     "github",
		})
		if err != nil {
			t.Fatalf("建行失败(%s): %v", state, err)
		}
		// created_at 由 now() 写入，回收判定看的就是它，只能回填。
		if _, err := s.m.pg.Exec(`UPDATE task_uploads SET created_at = now() - $2::interval WHERE id=$1`,
			up.ID, age.String()); err != nil {
			t.Fatalf("回填 created_at 失败: %v", err)
		}
		dir := filepath.Join(s.m.dir, uploadDirName, up.ID)
		if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
			t.Fatal(err)
		}
		return row{up.ID, state, age, dir}
	}

	oldReady := mk(db.UploadStateReady, 25*time.Hour)
	freshReady := mk(db.UploadStateReady, time.Hour)
	oldFailed := mk(db.UploadStateFailed, 2*time.Hour)
	oldAwait := mk(db.UploadStateAwaitingConfirm, 2*time.Hour)
	oldFetching := mk(db.UploadStateFetching, 25*time.Hour)

	s.runUploadJanitor()

	for _, r := range []row{oldReady, oldFailed, oldAwait} {
		if _, err := store.GetTaskUpload(r.id); err == nil {
			t.Errorf("state=%s 超过 TTL 应被删除", r.state)
		}
		if _, err := os.Stat(r.dir); !os.IsNotExist(err) {
			t.Errorf("state=%s 的目录应被删除", r.state)
		}
	}
	for _, r := range []row{freshReady, oldFetching} {
		if _, err := store.GetTaskUpload(r.id); err != nil {
			t.Errorf("state=%s 不该被回收: %v", r.state, err)
		}
		if _, err := os.Stat(r.dir); err != nil {
			t.Errorf("state=%s 的目录不该被删: %v", r.state, err)
		}
	}

	// 孤儿目录：有目录没行，且 mtime 超过宽限期。
	orphan := filepath.Join(s.m.dir, uploadDirName, "orphan-dir")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	s.runUploadJanitor()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("过期孤儿目录应被清理")
	}

	fresh := filepath.Join(s.m.dir, uploadDirName, "fresh-orphan")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	s.runUploadJanitor()
	if _, err := os.Stat(fresh); err != nil {
		t.Error("宽限期内的目录不该被清理（可能正在导入）")
	}

	for _, r := range []row{freshReady, oldFetching} {
		_ = store.DeleteTaskUpload(r.id)
	}
}

func TestImportGithubMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errCancelled, "已取消"},
		{errConfirmTimeout, "确认超时"},
		{errTooLargeHard, "超过"},
		{errNotZip, "zip"},
	}
	for _, tc := range cases {
		if got := importGithubMessage(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("%v 的文案应包含 %q，实得 %q", tc.err, tc.want, got)
		}
	}
}

// TestCreateTaskRequiresSource 盯住「源码是建任务的必填项，对管理员也一样」。
//
// 以前管理员可以不带 upload_id 建任务，但建完之后【没有任何接口能补源码】
// （attachUploadToTask 只在创建时调用）——任务永远拿不到代码，开启后 planner
// 还按提示词假定 source/ 存在，worker 读不到任何文件，只会空转烧 token。
func TestCreateTaskRequiresSource(t *testing.T) {
	s := newGithubTestServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	post := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 管理员也不例外。
	if rec := post(map[string]any{"description": "无源码", "goal": "应被拒绝"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("无源码建任务应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 带上 ready 的 staging 上传就能建，且源码真的落进 tasks/<id>/source。
	rec := post(map[string]any{
		"description": "有源码", "goal": "应成功",
		"upload_id": stageTestSource(t, s, "create"),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("带源码建任务应返回 201，实际 %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID         string `json:"id"`
		SourcePath string `json:"source_path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("创建响应异常: %s (%v)", rec.Body.String(), err)
	}
	t.Cleanup(func() {
		if id, err := strconv.ParseInt(created.ID, 10, 64); err == nil {
			_ = s.m.pg.DeleteTask(id)
		}
	})
	if created.SourcePath == "" {
		t.Fatalf("创建后 source_path 为空: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, filepath.FromSlash(created.SourcePath))); err != nil {
		t.Fatalf("源码目录未落地: %v", err)
	}
}

// TestStartTaskRejectsSourceLessTask 盯住「没有源码就不许开启审计」——管理员也不例外。
//
// 以前管理员开启无源码任务只记一条日志就放行，engine 起来后 planner 按提示词
// 假定 source/ 存在，worker 读不到任何文件，整轮审计空转。
func TestStartTaskRejectsSourceLessTask(t *testing.T) {
	s := newGithubTestServer(t)
	h := s.Handler()
	tok := testAdminToken(t, s)

	body, _ := json.Marshal(map[string]any{
		"description": "无源码可开启性", "goal": "验证开启被拒",
		"upload_id": stageTestSource(t, s, "nostart"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建任务失败: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("创建响应异常: %s (%v)", rec.Body.String(), err)
	}
	taskID, _ := strconv.ParseInt(created.ID, 10, 64)
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(taskID) })

	// 模拟存量无源码任务：库里和内存里都把 source_path 清掉。
	if err := s.m.pg.SetTaskSourcePath(taskID, ""); err != nil {
		t.Fatalf("清空 source_path 失败: %v", err)
	}
	s.m.SetTaskSourcePathInMemory(created.ID, "")

	req = httptest.NewRequest(http.MethodPost, "/api/tasks/"+created.ID+"/start", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无源码开启应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "没有源码包") {
		t.Errorf("提示文案应说明缺少源码，实得 %s", rec.Body.String())
	}
}
