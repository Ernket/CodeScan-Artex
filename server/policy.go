package server

import (
	"net/http"
	"strings"
)

// =====================================================================
// 平台级能力守卫（管理员专属功能区 + 管理员专属写操作）
//
// 背景：本项目的部门权限模型原本只覆盖「任务」这一条线（见 authz.go 的
// guardTaskAccess）。系统区（LLM / Agent / Skill / 工具 / MCP / 通知 / 日志 /
// 系统配置 / 拦截审批 / 更新）、工作空间、资产同步、流量、LLM 录制这些面在 API
// 层只过了 requireAuth —— 任何登录账号（含无部门的部门访客）都能读写。
//
// 这里用「路径前缀 → 仅管理员」的显式清单把它们一次性收口。清单是唯一事实
// 来源：以后在这些功能区下新增路由会自动被覆盖，不会因为「忘了加守卫」而重新
// 变成公开接口。
//
// 两类清单：
//   - adminOnlyAreas：整个功能区（读 + 写）都仅管理员。
//   - adminOnlyWrites：读保持共享（其他页面/筛选要用），仅写操作限管理员。
//
// 匹配规则是「路径 == 前缀」或「路径以 前缀 + "/" 开头」，所以 "/api/llm" 命中
// "/api/llm" 与 "/api/llm/profiles"，但不会命中 "/api/llmx"。
// =====================================================================

// adminOnlyAreas 是整个功能区仅管理员可访问的 /api 路径前缀。
var adminOnlyAreas = []string{
	// 系统配置与平台运维
	"/api/settings", // 全局开关、代理、web-search key、并发上限…
	"/api/update",   // 自更新：apply/rollback 会替换二进制并重启
	"/api/gc",
	"/api/logs",     // 日志含文件路径、代理地址、内部报错
	"/api/commands", // 工具调用明细与统计

	// LLM 配置与录制。录制（/api/llm/records）含原始请求/响应体：源码 + 提示词。
	"/api/llm",

	// Agent / Skill / 工具 / MCP / 资源可见性
	"/api/agents",
	"/api/triggers",
	"/api/skills", // 文件读写：skill 目录是 agent 的指令来源
	"/api/tools",
	"/api/mcp",
	"/api/visibility",

	// 通知推送（渠道配置、投递记录含漏洞标题/摘要）
	"/api/notify",

	// 拦截审批：危险工具调用的人工闸门，绝不能由普通用户关闭或代批
	"/api/intercept",
	"/api/asset-intercept",

	// 工作空间：所有任务源码与 agent 产物的共享工作目录
	"/api/workspace",

	// 资产同步（ScopeSentry 数据源与导入）
	"/api/sync",

	// 流量：抓包记录含真实目标凭据
	"/api/traffic",

	// 全局活动任务切换（前端未使用；它会改变所有人的默认视图与引擎目标）
	"/api/active",
}

// adminOnlyWrites 是「读保持共享、仅写操作限管理员」的 /api 路径前缀。
//
// 这些是跨部门共享的配置/资产数据：普通用户需要读（例如任务列表用分类做筛选、
// 资产页看测绘结果），但改动会影响所有人，因此只放读。
var adminOnlyWrites = []string{
	"/api/task-categories",
	"/api/task-templates",
	"/api/assets",
	"/api/companies",
}

// matchPrefix reports whether p equals prefix or lives under it.
func matchPrefix(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// inPrefixList reports whether p falls under any of the given prefixes.
func inPrefixList(p string, prefixes []string) bool {
	for _, pre := range prefixes {
		if matchPrefix(p, pre) {
			return true
		}
	}
	return false
}

// adminRequired reports whether the request must be made by an administrator.
func adminRequired(r *http.Request) bool {
	if inPrefixList(r.URL.Path, adminOnlyAreas) {
		return true
	}
	return isWriteMethod(r.Method) && inPrefixList(r.URL.Path, adminOnlyWrites)
}

// guardAdminAreas rejects non-admins on platform-management surfaces. It runs after
// requireAuth, so the account is always available. Unauthenticated paths (login,
// status, init, health) have no account and are not in any list, so they pass
// through untouched.
func (s *Server) guardAdminAreas(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !adminRequired(r) {
			next.ServeHTTP(w, r)
			return
		}
		u := currentUser(r)
		if u == nil || !u.IsAdmin {
			writeErr(w, 403, "需要管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	})
}
