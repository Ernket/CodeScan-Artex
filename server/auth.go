package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	// authPassKey / authUserKey 是旧版「单账号（ARTEX）+ 单一密码」的遗留设置项。
	// 现在只用于首次启动时把这些凭据迁移成第一个管理员账号（见 migrateLegacyAdmin）。
	authPassKey = "auth.password_hash"
	authUserKey = "auth.username"
	jwtTTL      = 7 * 24 * time.Hour
	keyChars    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 下限与 setup 页的前端校验一致——校验只放在前端等于没放，直接打 API 就能
	// 绕过。上限是 bcrypt 的硬限制：超过 72 字节 GenerateFromPassword 会返回
	// ErrPasswordTooLong，提前挡掉好过让用户收到一句含义不明的「密码加密失败」。
	minPasswordRunes    = 8
	maxPasswordBytes    = 72
	maxUsernameRunes    = 64
	defaultAdminName    = "admin"
	defaultAdminDisplay = "管理员"
)

// errDataSourceUnavailable 是密码相关读操作失败时统一的回复。这些 handler 绝不能
// 把"读不到"当成"没有设置"：authInit 曾因此在数据库报错时放行，让未认证请求覆盖
// 掉已有的管理员密码。
const errDataSourceUnavailable = "数据源暂时不可用，请稍后重试"

// validatePassword 返回空串表示通过，否则返回可直接展示给用户的中文原因。
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("密码长度至少 %d 位", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("密码长度不能超过 %d 字节", maxPasswordBytes)
	}
	return ""
}

// validateUsername 校验账号名（字母/数字/._- 且长度受限），返回中文原因。
func validateUsername(name string) string {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 2 {
		return "用户名长度至少 2 位"
	}
	if n > maxUsernameRunes {
		return fmt.Sprintf("用户名长度不能超过 %d 位", maxUsernameRunes)
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "用户名不能包含空白字符"
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// one-time migration out of the old in-workspace location.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT key 已从 %s 迁移到 %s（移出可浏览工作区）", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 新 JWT key 已写入 %s", path)
	return buf, nil
}

// authClaims is the JWT payload: the account id plus the role flags used by the
// permission layer. Username/is_admin are carried for convenience only — every
// request re-reads the account row, so a deleted/disabled/demoted account loses
// access immediately without waiting for the token to expire.
type authClaims struct {
	UID      int64  `json:"uid"`
	Username string `json:"usr"`
	Admin    bool   `json:"adm"`
	jwt.RegisteredClaims
}

// signJWT issues a 7-day HS256 token for a user account.
func signJWT(key []byte, u *db.User) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{
		UID:      u.ID,
		Username: u.Username,
		Admin:    u.IsAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.Username,
			ExpiresAt: jwt.NewNumericDate(now.Add(jwtTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}).SignedString(key)
}

// parseJWT validates tokenStr and returns its claims.
func parseJWT(tokenStr string, key []byte) (*authClaims, error) {
	claims := &authClaims{}
	t, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	})
	if err != nil || !t.Valid || claims.UID <= 0 {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

// extractToken reads the JWT from Authorization: Bearer header,
// artex_token cookie, or ?token= query param (for SSE connections).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// authCtxKey carries the authenticated account through the request context.
type authCtxKey struct{}

// currentUser returns the authenticated account, or nil outside the auth chain.
func currentUser(r *http.Request) *db.User {
	if r == nil {
		return nil
	}
	u, _ := r.Context().Value(authCtxKey{}).(*db.User)
	return u
}

// requireAuth wraps h with JWT validation + account resolution.
// 只有「登录前必须能打」的三条路径免鉴权：状态查询、首次初始化、登录。其余 /api/auth/*
// （me / logout / change-password）都要带 token —— 它们要拿到当前账号才能工作。
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/status", "/api/auth/init", "/api/auth/login", "/api/health":
			h.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		if tok == "" {
			writeErr(w, 401, "未授权")
			return
		}
		claims, err := parseJWT(tok, s.jwtKey)
		if err != nil {
			writeErr(w, 401, "token 无效或已过期")
			return
		}
		u, err := s.m.pg.Users().GetUserByID(claims.UID)
		if err != nil {
			if errors.Is(err, db.ErrUserNotFound) {
				writeErr(w, 401, "账号不存在或已被删除")
				return
			}
			writeErr(w, 503, errDataSourceUnavailable)
			return
		}
		if !u.Enabled {
			writeErr(w, 403, "账号已停用，请联系管理员")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authCtxKey{}, u)))
	})
}

// requireAdmin guards platform-management endpoints (账号与部门管理).
func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil || !u.IsAdmin {
			writeErr(w, 403, "需要管理员权限")
			return
		}
		h(w, r)
	}
}

// ---- permission model ----

// userPermissions is the capability set reported to the frontend so it can hide
// actions a user may not perform. The backend enforces the same rules regardless.
type userPermissions struct {
	CanManageUsers       bool `json:"can_manage_users"`
	CanManageDepartments bool `json:"can_manage_departments"`
	CanCreateTask        bool `json:"can_create_task"`
	CanChat              bool `json:"can_chat"`
	IsVisitorOnly        bool `json:"is_visitor_only"`
}

// permissionsFor computes the capability set from the account's memberships.
func (s *Server) permissionsFor(u *db.User) userPermissions {
	p := userPermissions{}
	if u == nil {
		return p
	}
	if u.IsAdmin {
		return userPermissions{
			CanManageUsers: true, CanManageDepartments: true,
			CanCreateTask: true, CanChat: true,
		}
	}
	owned, visitor := 0, 0
	for _, m := range u.Departments {
		if m.Role == db.DeptRoleOwner {
			owned++
		} else {
			visitor++
		}
	}
	p.CanCreateTask = owned > 0
	p.CanChat = owned > 0
	p.IsVisitorOnly = owned == 0 && visitor > 0
	return p
}

// canCreateTask reports whether u may create a task (管理员或至少一个部门的拥有者).
func (s *Server) canCreateTask(u *db.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	if u.IsAdmin {
		return true, nil
	}
	owned, err := s.m.pg.Users().OwnedDepartmentIDs(u.ID)
	if err != nil {
		return false, err
	}
	return len(owned) > 0, nil
}

// taskAccess is the resolved permission pair for one task.
type taskAccess struct {
	View  bool
	Write bool
}

// taskAccessFor resolves what u may do with a task:
//   - 管理员：全部任务可读写；
//   - 部门成员：可读本部门任务；部门所有者（owner）可写，访客（visitor）只读；
//   - 创建者：自己的任务（含无部门的历史任务）可读写。
func (s *Server) taskAccessFor(u *db.User, t *Task) taskAccess {
	if t == nil {
		return taskAccess{}
	}
	return accessForOwner(u, t.DepartmentID, t.CreatedBy)
}

// accessForOwner 是任务权限判定的单一实现：入参只要归属信息（部门 + 创建者）。
// 内存里的 Task、数据库里的 Task、以及只剩元数据的冷归档都能复用同一条规则，
// 避免「按任务对象判」和「按归档反查判」两套口径漂移。
func accessForOwner(u *db.User, departmentID, createdBy *int64) taskAccess {
	if u == nil {
		return taskAccess{}
	}
	if u.IsAdmin {
		return taskAccess{View: true, Write: true}
	}
	acc := taskAccess{}
	if createdBy != nil && *createdBy == u.ID {
		acc.View, acc.Write = true, true
	}
	if departmentID != nil {
		for _, m := range u.Departments {
			if m.DepartmentID == *departmentID {
				acc.View = true
				if m.Role == db.DeptRoleOwner {
					acc.Write = true
				}
				break
			}
		}
	}
	return acc
}

// visibleTask reports whether u may see one task.
func (s *Server) visibleTask(u *db.User, t *Task) bool {
	return s.taskAccessFor(u, t).View
}

// taskWriteAllowed reports whether u may mutate/chat-with one task.
func (s *Server) taskWriteAllowed(u *db.User, t *Task) bool {
	return s.taskAccessFor(u, t).Write
}

// ---- auth handlers ----

// GET /api/auth/status — reports whether the platform has been initialised.
// 读失败必须回 503 而不是 initialized:false：前端在 initialized:false 时会把用户
// 送到 /setup 去设置账号，把数据库故障包装成 200 等于把用户往覆盖已有密码的路上推。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	users := s.m.pg.Users()
	count, err := users.CountUsers()
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	admins, err := users.CountAdmins()
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{
		"initialized": count > 0,
		"user_count":  count,
		"admin_count": admins,
		"service":     "SCA Audit",
	})
}

// POST /api/auth/init — creates the FIRST administrator; rejected once any account
// exists. Body: {username, password, display_name?}.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	users := s.m.pg.Users()
	count, err := users.CountUsers()
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if count > 0 {
		writeErr(w, 403, "管理员账号已存在")
		return
	}
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		req.Username = defaultAdminName
	}
	if msg := validateUsername(req.Username); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	display := strings.TrimSpace(req.DisplayName)
	if display == "" {
		display = defaultAdminDisplay
	}
	u, err := users.CreateUser(req.Username, display, string(hash), true, nil)
	if err != nil {
		if errors.Is(err, db.ErrUsernameTaken) {
			writeErr(w, 403, "管理员账号已存在")
			return
		}
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	tok, err := signJWT(s.jwtKey, u)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	log.Printf("[auth] 已创建管理员账号 %s", u.Username)
	writeJSON(w, 200, map[string]any{"token": tok, "user": u})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	users := s.m.pg.Users()
	auth, err := users.GetUserAuth(req.Username)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			writeErr(w, 401, "用户名或密码错误")
			return
		}
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(auth.PasswordHash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	if !auth.Enabled {
		writeErr(w, 403, "账号已停用，请联系管理员")
		return
	}
	u, err := users.GetUserByID(auth.ID)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	tok, err := signJWT(s.jwtKey, u)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	_ = users.TouchLastLogin(u.ID)
	writeJSON(w, 200, map[string]any{"token": tok, "user": u, "permissions": s.permissionsFor(u)})
}

// POST /api/auth/logout — stateless JWT: the client drops the token.
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/auth/me — the current account, its departments (with roles) and the
// capability set used by the frontend to render/hide actions.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeErr(w, 401, "未授权")
		return
	}
	writeJSON(w, 200, map[string]any{
		"user":        u,
		"permissions": s.permissionsFor(u),
	})
}

// POST /api/auth/change-password — changes the CURRENT account's password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "新密码不能为空")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	me := currentUser(r)
	if me == nil {
		writeErr(w, 401, "未授权")
		return
	}
	users := s.m.pg.Users()
	auth, err := users.GetUserAuth(me.Username)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			writeErr(w, 401, "未授权")
			return
		}
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(auth.PasswordHash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "当前密码错误")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	if err := users.SetUserPassword(auth.ID, string(newHash)); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// migrateLegacyAdmin turns a legacy single-password install into the first admin
// account, so existing deployments keep working after the multi-user upgrade. It is
// a no-op once any account exists.
func (s *Server) migrateLegacyAdmin() {
	pg := s.m.pg
	if pg == nil {
		return
	}
	users := pg.Users()
	if count, err := users.CountUsers(); err != nil || count > 0 {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil || strings.TrimSpace(hash) == "" {
		return
	}
	username, _, _ := pg.GetSetting(authUserKey)
	username = strings.TrimSpace(username)
	if username == "" || username == "ARTEX" {
		username = defaultAdminName
	}
	created, err := users.MigrateLegacyAdmin(username, hash)
	if err != nil {
		log.Printf("[auth] 旧单账号迁移失败: %v", err)
		return
	}
	if created {
		log.Printf("[auth] 已把旧版单账号迁移为管理员账号 %q（密码不变）", username)
	}
}
