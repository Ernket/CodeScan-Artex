package server

import (
	"net/http"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 回归测试：不可见的任务，无论读还是写都必须返回 404。
//
// 守卫原本先判 「write && !access.Write」 再判 「!write && !access.View」，
// 导致跨部门账号用写方法探测时拿到 403、用读方法拿到 404 —— 状态码本身
// 就泄漏了「这个任务 id 存在，只是我无权写」。可见性必须先判。
func TestTaskGuardHidesInvisibleTaskOnWrite(t *testing.T) {
	depA, depB := int64(1), int64(2)
	task := &Task{ID: "7", DepartmentID: &depA}
	s := &Server{m: &Manager{tasks: map[string]*Task{"7": task}}}
	h := s.guardTaskAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	hf := http.HandlerFunc(h.ServeHTTP)

	foreign := &db.User{
		ID: 3, Username: "dave", Enabled: true,
		Departments: []db.UserDepartment{{DepartmentID: depB, Role: db.DeptRoleOwner}},
	}
	if w := callAs(t, foreign, hf, http.MethodGet, "/api/tasks/7", ""); w.Code != http.StatusNotFound {
		t.Errorf("跨部门读取应返回 404，实得 %d（%s）", w.Code, w.Body.String())
	}
	if w := callAs(t, foreign, hf, http.MethodPost, "/api/tasks/7/start", ""); w.Code != http.StatusNotFound {
		t.Errorf("跨部门写入应返回 404 而不是 403（403 泄漏任务存在），实得 %d（%s）", w.Code, w.Body.String())
	}

	// 同部门访客：能看（200），不能写（403）——这是真正的只读语义，必须保留。
	visitor := &db.User{
		ID: 4, Username: "bob", Enabled: true,
		Departments: []db.UserDepartment{{DepartmentID: depA, Role: db.DeptRoleVisitor}},
	}
	if w := callAs(t, visitor, hf, http.MethodGet, "/api/tasks/7", ""); w.Code != http.StatusOK {
		t.Errorf("同部门访客应可读，实得 %d（%s）", w.Code, w.Body.String())
	}
	if w := callAs(t, visitor, hf, http.MethodPost, "/api/tasks/7/start", ""); w.Code != http.StatusForbidden {
		t.Errorf("同部门访客写操作应返回 403，实得 %d（%s）", w.Code, w.Body.String())
	}
}

// 回归测试：带 ?task= 的对话入口同样不能泄漏任务存在性。
//
// 守卫原本对「别人的任务」返回 403、对「不存在的任务」返回 404，状态码即可
// 区分二者。改为与任务守卫一致：不可见一律 404，可见但只读才 403。
func TestChatGuardHidesInvisibleTask(t *testing.T) {
	depA, depB := int64(1), int64(2)
	task := &Task{ID: "7", DepartmentID: &depA}
	s := &Server{m: &Manager{tasks: map[string]*Task{"7": task}}}
	h := s.guardTaskAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	hf := http.HandlerFunc(h.ServeHTTP)

	foreign := &db.User{
		ID: 3, Username: "dave", Enabled: true,
		Departments: []db.UserDepartment{{DepartmentID: depB, Role: db.DeptRoleOwner}},
	}
	if w := callAs(t, foreign, hf, http.MethodPost, "/api/chat?task=7", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("跨部门对话应返回 404（403 会泄漏任务存在），实得 %d（%s）", w.Code, w.Body.String())
	}
	if w := callAs(t, foreign, hf, http.MethodPost, "/api/chat?task=999", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("不存在的任务应返回 404，实得 %d（%s）", w.Code, w.Body.String())
	}

	visitor := &db.User{
		ID: 4, Username: "bob", Enabled: true,
		Departments: []db.UserDepartment{{DepartmentID: depA, Role: db.DeptRoleVisitor}},
	}
	if w := callAs(t, visitor, hf, http.MethodPost, "/api/chat?task=7", "{}"); w.Code != http.StatusForbidden {
		t.Errorf("同部门访客发起对话应返回 403，实得 %d（%s）", w.Code, w.Body.String())
	}

	owner := &db.User{
		ID: 5, Username: "alice", Enabled: true,
		Departments: []db.UserDepartment{{DepartmentID: depA, Role: db.DeptRoleOwner}},
	}
	if w := callAs(t, owner, hf, http.MethodPost, "/api/chat?task=7", "{}"); w.Code != http.StatusOK {
		t.Errorf("同部门 owner 应放行到 handler，实得 %d（%s）", w.Code, w.Body.String())
	}
}
