package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestTaskArchiveAPIQueueListAndLimits(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer m.Close()
	task, err := m.CreateTask("archive api", "verify 202", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskID)
		_ = m.pg.DeleteTask(taskID)
	}()

	// Start with an already-cancelled context so the background archive worker
	// exits before these route-contract assertions enqueue work.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	s := New(ctx, m, dataDir, dataDir, dataDir)
	s.archiveWG.Wait()
	token := testAdminToken(t, s)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	recorder := request(http.MethodPost, "/api/tasks/"+task.ID+"/archive", "{}")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("running archive status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := m.pg.SetPaused(taskID, true); err != nil {
		t.Fatal(err)
	}
	recorder = request(http.MethodPost, "/api/tasks/"+task.ID+"/archive", "{}")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("archive queue status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var queued struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.ID <= 0 || queued.State != "archive_queued" {
		t.Fatalf("unexpected queue response: %+v", queued)
	}

	recorder = request(http.MethodGet, "/api/task-archives?page=1&size=10&q="+task.ID, "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`"task_id":%d`, taskID)) {
		t.Fatalf("archive list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	ids := make([]string, 101)
	for index := range ids {
		ids[index] = strconv.Itoa(index + 1)
	}
	body, _ := json.Marshal(map[string]any{"task_ids": ids})
	recorder = request(http.MethodPost, "/api/tasks/archive/batch", string(body))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "100") {
		t.Fatalf("oversized batch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestTaskArchivesAreScopedToDepartment 盯住归档读取的部门边界。
//
// 归档里带任务名/描述/目标/统计，属于任务数据的快照；此前 list/get 两个接口
// 只过了 requireAuth，任何登录账号（含无部门账号）都能遍历到别的部门的归档。
func TestTaskArchivesAreScopedToDepartment(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer m.Close()

	deptA, err := m.pg.Departments().CreateDepartment("归档测试-甲", "")
	if err != nil {
		t.Fatal(err)
	}
	deptB, err := m.pg.Departments().CreateDepartment("归档测试-乙", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = m.pg.Departments().DeleteDepartment(deptA.ID)
		_ = m.pg.Departments().DeleteDepartment(deptB.ID)
	}()

	mkTask := func(dept *db.Department) *db.Task {
		t.Helper()
		task, err := m.pg.CreateTaskWithOptions("归档作用域", "goal", db.TaskCreateOptions{DepartmentID: &dept.ID})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	taskA, taskB := mkTask(deptA), mkTask(deptB)
	defer func() {
		_ = m.pg.DeleteTask(taskA.ID)
		_ = m.pg.DeleteTask(taskB.ID)
	}()

	owner, err := m.pg.Users().CreateUser("archive-owner", "", "x", false,
		[]db.DepartmentMembership{{DepartmentID: deptA.ID, Role: db.DeptRoleOwner}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.pg.Users().DeleteUser(owner.ID) }()

	insert := func(task *db.Task, name string) int64 {
		t.Helper()
		var id int64
		err := m.pg.QueryRow(`INSERT INTO task_archives(task_id,state,phase,task_name) VALUES ($1,'ready','done',$2) RETURNING id`,
			task.ID, name).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM task_archives WHERE id=$1`, id) })
		return id
	}
	aID := insert(taskA, "ARCH-SCOPE-A")
	bID := insert(taskB, "ARCH-SCOPE-B")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	s := New(ctx, m, dataDir, dataDir, dataDir)

	ownerTok, err := signJWT(s.jwtKey, owner)
	if err != nil {
		t.Fatal(err)
	}
	get := func(token, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 甲部门 owner：只看到本部门的归档。
	rec := get(ownerTok, "/api/task-archives?q=ARCH-SCOPE")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID     int64  `json:"id"`
			TaskID int64  `json:"task_id"`
			Name   string `json:"task_name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != "ARCH-SCOPE-A" {
		t.Fatalf("甲部门应只看到自己的归档，实际 total=%d items=%+v", page.Total, page.Items)
	}

	// 别人的归档：按不存在处理。
	if rec := get(ownerTok, "/api/task-archives/"+strconv.FormatInt(bID, 10)); rec.Code != http.StatusNotFound {
		t.Fatalf("跨部门归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(ownerTok, "/api/task-archives/"+strconv.FormatInt(aID, 10)); rec.Code != http.StatusOK {
		t.Fatalf("本部门归档应 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 管理员：两份都能看到。
	adminTok := testAdminToken(t, s)
	rec = get(adminTok, "/api/task-archives?q=ARCH-SCOPE")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("管理员应看到两份归档，实际 total=%d body=%s", page.Total, rec.Body.String())
	}

	// 无部门账号：一份都看不到。
	nodept, err := m.pg.Users().CreateUser("archive-nodept", "", "x", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.pg.Users().DeleteUser(nodept.ID) }()
	nodeptTok, err := signJWT(s.jwtKey, nodept)
	if err != nil {
		t.Fatal(err)
	}
	rec = get(nodeptTok, "/api/task-archives?q=ARCH-SCOPE")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("无部门账号不应看到任何归档，实际 total=%d", page.Total)
	}
	if rec := get(nodeptTok, "/api/task-archives/"+strconv.FormatInt(bID, 10)); rec.Code != http.StatusNotFound {
		t.Fatalf("无部门账号读归档应 404，实际 %d", rec.Code)
	}
}

// TestTaskArchiveWritesAreScopedToDepartment 盯住归档写接口（恢复/删除）的部门边界。
//
// 归档任务在归档完成时就从 manager 卸载，写接口只能由归档反查任务判权限：
//   - 跨部门/无部门的归档与不存在必须同为 404（403 会变成归档存在性探针）；
//   - 部门访客可见但只读，恢复/删除返回 403；
//   - 部门所有者与创建者必须能恢复/删除自己的归档（改造前在内存里查不到任务，
//     非管理员一律 403，等于归档只能进不能出）。
func TestTaskArchiveWritesAreScopedToDepartment(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer m.Close()

	deptA, err := m.pg.Departments().CreateDepartment("归档写测试-甲", "")
	if err != nil {
		t.Fatal(err)
	}
	deptB, err := m.pg.Departments().CreateDepartment("归档写测试-乙", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = m.pg.Departments().DeleteDepartment(deptA.ID)
		_ = m.pg.Departments().DeleteDepartment(deptB.ID)
	}()

	taskA, err := m.pg.CreateTaskWithOptions("归档写作用域", "goal", db.TaskCreateOptions{DepartmentID: &deptA.ID})
	if err != nil {
		t.Fatal(err)
	}
	taskA2, err := m.pg.CreateTaskWithOptions("归档写作用域", "goal", db.TaskCreateOptions{DepartmentID: &deptA.ID})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := m.pg.CreateTaskWithOptions("归档写作用域", "goal", db.TaskCreateOptions{DepartmentID: &deptB.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = m.pg.DeleteTask(taskA.ID)
		_ = m.pg.DeleteTask(taskA2.ID)
		_ = m.pg.DeleteTask(taskB.ID)
	}()

	owner, err := m.pg.Users().CreateUser("archive-write-owner", "", "x", false,
		[]db.DepartmentMembership{{DepartmentID: deptA.ID, Role: db.DeptRoleOwner}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.pg.Users().DeleteUser(owner.ID) }()
	visitor, err := m.pg.Users().CreateUser("archive-write-visitor", "", "x", false,
		[]db.DepartmentMembership{{DepartmentID: deptA.ID, Role: db.DeptRoleVisitor}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.pg.Users().DeleteUser(visitor.ID) }()
	outsider, err := m.pg.Users().CreateUser("archive-write-outsider", "", "x", false,
		[]db.DepartmentMembership{{DepartmentID: deptB.ID, Role: db.DeptRoleOwner}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.pg.Users().DeleteUser(outsider.ID) }()

	insert := func(task *db.Task, name string) int64 {
		t.Helper()
		var id int64
		err := m.pg.QueryRow(`INSERT INTO task_archives(task_id,state,phase,task_name) VALUES ($1,'ready','done',$2) RETURNING id`,
			task.ID, name).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM task_archives WHERE id=$1`, id) })
		return id
	}
	aID := insert(taskA, "ARCH-WRITE-A")
	a2ID := insert(taskA2, "ARCH-WRITE-A2")
	bID := insert(taskB, "ARCH-WRITE-B")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	s := New(ctx, m, dataDir, dataDir, dataDir)
	// 归档任务在归档完成时会被 manager 卸载（archiveTask -> forgetTask）。这里同样
	// 卸载，才能覆盖「内存里查不到，只能读库判权限」的那条分支。
	m.forgetTask(strconv.FormatInt(taskA.ID, 10), taskA.ID)
	m.forgetTask(strconv.FormatInt(taskB.ID, 10), taskB.ID)

	call := func(u *db.User, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		token, err := signJWT(s.jwtKey, u)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	restorePath := func(id int64) string { return "/api/task-archives/" + strconv.FormatInt(id, 10) + "/restore" }
	deletePath := func(id int64) string { return "/api/task-archives/" + strconv.FormatInt(id, 10) }

	// 部门访客：看得见，但不能恢复/删除。
	if rec := call(visitor, http.MethodPost, restorePath(aID), "{}"); rec.Code != http.StatusForbidden {
		t.Fatalf("部门访客恢复归档应 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(visitor, http.MethodDelete, deletePath(aID), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("部门访客删除归档应 403，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 跨部门：与不存在同为 404，不能靠 403/404 区分「存在但无权」。
	if rec := call(outsider, http.MethodPost, restorePath(aID), "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("跨部门恢复归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(outsider, http.MethodDelete, deletePath(aID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("跨部门删除归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(outsider, http.MethodPost, "/api/task-archives/restore/batch",
		`{"archive_ids":[`+strconv.FormatInt(aID, 10)+`]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("跨部门批量恢复应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 批量归档的越权面在 body 的 task_ids：别的部门的历史任务不在内存里，也必须挡住。
	if rec := call(outsider, http.MethodPost, "/api/tasks/archive/batch",
		`{"task_ids":["`+strconv.FormatInt(taskA.ID, 10)+`"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("跨部门批量归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(owner, http.MethodPost, "/api/tasks/archive/batch",
		`{"task_ids":["`+strconv.FormatInt(taskB.ID, 10)+`"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("把别的部门任务塞进批量归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 本部门所有者：可以恢复自己的归档（归档任务不在内存里也照样判得出来）。
	if rec := call(owner, http.MethodPost, restorePath(aID), "{}"); rec.Code != http.StatusAccepted {
		t.Fatalf("部门所有者恢复本部门归档应 202，实际 %d %s", rec.Code, rec.Body.String())
	}
	var state string
	if err := m.pg.QueryRow(`SELECT state FROM task_archives WHERE id=$1`, aID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.RestoreQueued {
		t.Fatalf("归档状态应为 %s，实际 %s", db.RestoreQueued, state)
	}
	if rec := call(owner, http.MethodDelete, deletePath(bID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("部门所有者删除别的部门归档应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(owner, http.MethodDelete, deletePath(a2ID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("部门所有者删除本部门归档应 202，实际 %d %s", rec.Code, rec.Body.String())
	}
}
