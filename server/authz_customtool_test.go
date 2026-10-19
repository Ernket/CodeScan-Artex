package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 回归测试：/api/tools/** 等高危管理端点必须管理员专属。
//
// 漏洞复现路径：requireAuth 只验证"登录了"，guardTaskAccess 只守 /api/tasks/{id}/
// 与对话接口。POST /api/tools/custom/test 直接调 runCommandTool → actool.NewBash，
// 因此任何已登录账号（包括只有部门访客角色的只读用户）都能在服务器上执行 shell。

func visitingUser() *db.User {
	return &db.User{
		ID:       9,
		Username: "visitor",
		Enabled:  true,
		IsAdmin:  false,
		// 只有部门访客角色：permissionsFor 会算出 IsVisitorOnly=true / CanChat=false
		Departments: []db.UserDepartment{{DepartmentID: 1, Role: db.DeptRoleVisitor}},
	}
}

func adminUser() *db.User {
	return &db.User{ID: 1, Username: "admin", Enabled: true, IsAdmin: true}
}

// callAs 直接调用 guard 包装后的 handler，绕过 requireAuth（在 context 里塞账号）。
func callAs(t *testing.T, u *db.User, guarded http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authCtxKey{}, u))
	w := httptest.NewRecorder()
	guarded(w, req)
	return w
}

// 每个可执行/可改配置的端点都必须对访客返回 403，而且要在触碰任何执行器之前返回。
func TestVisitorCannotReachDangerousAdminEndpoints(t *testing.T) {
	s := &Server{}
	// 这些 handler 本身不做权限判断，权限必须来自路由包装。
	// 断言"未授权"时不关心返回的是 400/500（说明已经进入 handler 了），
	// 只关心必须是 403。
	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{"自定义工具 dry-run 执行", s.pgTestCustomTool, "POST", "/api/tools/custom/test",
			`{"kind":"command","exec":{"command":"id"}}`},
		{"新建自定义工具", s.pgCreateCustomTool, "POST", "/api/tools/custom",
			`{"key":"x","kind":"command","exec":{"command":"id"}}`},
		{"修改自定义工具", s.pgUpdateCustomTool, "PUT", "/api/tools/custom/x",
			`{"kind":"command","exec":{"command":"id"}}`},
		{"删除自定义工具", s.pgDeleteCustomTool, "DELETE", "/api/tools/custom/x", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// adminOnly 是我们期望的包装；这里直接测目标包装函数本身，
			// 一旦 requireAdmin 被移除或换成别的弱校验，本测试会失败。
			w := callAs(t, visitingUser(), s.requireAdmin(tc.handler), tc.method, tc.path, tc.body)
			if w.Code != 403 {
				t.Fatalf("%s: 访客得到 %d（期望 403），body=%s —— 执行器已被触达",
					tc.name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "output") {
				t.Fatalf("%s: 访客拿到了执行输出: %s", tc.name, w.Body.String())
			}
		})
	}
}

func TestAdminPassesAdminGuard(t *testing.T) {
	s := &Server{}
	called := false
	h := s.requireAdmin(func(w http.ResponseWriter, r *http.Request) { called = true })
	w := callAs(t, adminUser(), h, "GET", "/api/users", "")
	if !called || w.Code == 403 {
		t.Fatalf("管理员应通过 requireAdmin, code=%d called=%v", w.Code, called)
	}
}

// 未登录（context 里没有账号）也必须拒绝，不能靠"u == nil 就放行"。
func TestAnonymousFailsAdminGuard(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("未认证请求不应进入 handler")
	})(w, httptest.NewRequest("GET", "/api/users", nil))
	if w.Code != 403 {
		t.Fatalf("anonymous: code=%d want 403", w.Code)
	}
}
