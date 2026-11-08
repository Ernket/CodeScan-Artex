// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import type { ChatMention } from "@/lib/chat-mentions";
import { MOCK } from "@/lib/mock/enabled";
import { mockHandle } from "@/lib/mock/handler";
import type {
  ActiveFindingRetest,
  Activity,
  Agent,
  AgentDetail,
  AgentTrigger,
  ArchiveBatchItem,
  Asset,
  AssetInterceptRule,
  AssetInterceptRuleInput,
  Audit,
  BatchCategoryItem,
  BatchControlItem,
  ChatAttachment,
  CommandRecord,
  Company,
  CompanyScopeMutation,
  CompanyScopeRule,
  Conversation,
  ConvTokenSummary,
  CoverageAssetRefs,
  CoverageGraphData,
  DailyTokenBucket,
  DeleteTaskOptions,
  DeleteTaskResult,
  Edge,
  EvidenceBodyPreview,
  ExplorationNodePage,
  ExplorationNodeQuery,
  Finding,
  FindingAssetTree,
  FindingDeepenResponse,
  FindingGroupsPage,
  FindingPackets,
  FindingQuery,
  FindingRawPacket,
  FindingRetest,
  FindingStats,
  FindingStatus,
  FindingsPage,
  FindingTraffic,
  FindingTrafficDetail,
  IntentAsset,
  InterceptApprovalFilter,
  InterceptApprovalRow,
  InterceptDetail,
  InterceptPending,
  InterceptRule,
  JudgeConfig,
  JudgeUsage,
  LLMPoolStatus,
  LLMProfile,
  LLMRecordDetail,
  LLMRecordItem,
  LLMRetryOverride,
  LLMRetryPolicy,
  LLMTask,
  MCPServer,
  MCPTool,
  MissingSkill,
  ModelTokenStat,
  NotificationChannel,
  NotificationDelivery,
  NotificationFilter,
  NotificationMeta,
  PromptVar,
  PromptVersion,
  SessionTokenUsage,
  Settings,
  Severity,
  SkillCall,
  SkillItem,
  SSProject,
  SSTask,
  Stats,
  Task,
  TaskArchive,
  TaskArchivePage,
  TaskAssetMutation,
  TaskAssetScopeMutation,
  TaskCategory,
  TaskConstraint,
  TaskGoal,
  TaskLLMResolutions,
  TaskNode,
  TaskScopeRow,
  TaskTemplate,
  TokenTotal,
  TokenUsage,
  Tool,
  ToolStat,
  TrafficDetail,
  TrafficEvidenceRef,
  TrafficEvidenceRole,
  TrafficHost,
  TrafficResp,
  UpdateCheck,
  UsageStats,
  UserAccount,
  Department,
  DepartmentMember,
  DepartmentMembershipInput,
  Permissions,
  TaskUpload,
  ImportGithubResult,
  TaskDependency,
  TaskDependencyList,
  TaskDepsImportResult,
  DepMaterializeResult,
  WorkspaceFile,
  WorkspaceListing,
} from "@/lib/types";

function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem("artex_token");
}

export async function http<T>(path: string, init?: RequestInit): Promise<T> {
  if (MOCK) return mockHandle<T>(init?.method ?? "GET", path, init?.body ?? null);
  const token = getToken();
  const r = await fetch(`/api${path}`, {
    ...init,
    headers: {
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init?.headers as Record<string, string> | undefined),
    },
  });
  if (r.status === 401) {
    if (typeof window !== "undefined") {
      localStorage.removeItem("artex_token");
      document.cookie = "artex_token=; path=/; max-age=0";
      window.location.href = "/login";
    }
    throw new Error("未授权");
  }
  if (!r.ok) {
    const fallback = `${init?.method ?? "GET"} ${path}: ${r.status}`;
    let message = fallback;
    try {
      const payload = (await r.json()) as { error?: unknown };
      if (typeof payload.error === "string" && payload.error.trim()) {
        message = payload.error.trim();
      }
    } catch {
      // Keep the status-based fallback for empty or non-JSON error responses.
    }
    throw new Error(message);
  }
  if (r.status === 204) return undefined as T;
  return r.json();
}

// sseUrl builds a URL for Server-Sent Events streams.
//
// Production (static export / Docker): the Go backend serves the web UI *and* the
// SSE streams on the same port, so we default to a same-origin (relative) URL.
// This is what makes reverse-proxy deploys work: a page loaded from
// https://<domain>/ connects to https://<domain>/api/..., which the proxy forwards
// to the backend — no need to expose :7878 publicly. Hardcoding :7878 here used to
// break exactly that (https://<domain>:7878/... is unreachable when only 443 is open).
//
// Dev (next dev): SSE must NOT go through the Next.js `/api` rewrite — that proxy
// buffers the streamed response, so event frames never reach the browser (the
// EventSource opens but receives 0 messages). So in dev only we connect straight
// to the Go backend on :7878, whose CORS is open.
//
// Override either default with NEXT_PUBLIC_SSE_BASE (set it to "" to force same-origin).
// Token is appended as ?token= because SSE can't carry cookies cross-origin.
// mockReport returns a canned Markdown report for the demo.
function mockReport(_task?: string): string {
  return `# SCA Audit 代码审计报告 — Acme Corp

## 概览
- 范围：acme.com（含 www / admin / api / shop / vpn 子域）
- 已确认发现：6 项（高危 3 · 中危 3 · 低危 2）
- 引擎模式：exploring

## 关键发现
1. **[高] 后台默认口令** admin.acme.com admin/admin123 → 可完全接管后台。
2. **[高] SQL 注入** www.acme.com/search?q= → 可读取 acme_prod 库。
3. **[高] IDOR** api.acme.com/v1/orders?id= → 可越权读取他人订单（含手机号/地址）。
4. **[中] 反射型 XSS**、**暴露 .git 源码**、**登录无速率限制**。

## 建议
- 后台强制改密 + 启用 MFA、封禁默认口令。
- search 接口参数化查询、输出编码。
- API 增加对象级授权校验（IDOR）、更换强 JWT 密钥。

> （demo）本报告由 mock 数据生成，仅用于界面演示。`;
}

export function sseUrl(path: string): string {
  const base =
    process.env.NEXT_PUBLIC_SSE_BASE ??
    (process.env.NODE_ENV !== "production" && typeof window !== "undefined"
      ? `${window.location.protocol}//${window.location.hostname}:7878`
      : "");
  const token = getToken();
  const sep = path.includes("?") ? "&" : "?";
  return token ? `${base}${path}${sep}token=${encodeURIComponent(token)}` : `${base}${path}`;
}

const get = <T>(p: string) => http<T>(p);
const post = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "POST", body: body ? JSON.stringify(body) : undefined });
const put = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PUT", body: body ? JSON.stringify(body) : undefined });
const patch = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PATCH", body: body ? JSON.stringify(body) : undefined });
const del = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "DELETE", body: body ? JSON.stringify(body) : undefined });

// Go serializes nil slices as JSON null — coerce to [].
const arr = <T>(x: T[] | null | undefined): T[] => x ?? [];
const tq = (task?: string, sep: "?" | "&" = "?") => (task ? `${sep}task=${encodeURIComponent(task)}` : "");

// findingFilterParams 把发现页的筛选条件序列化成 query string。列表 / 分组 /
// 资产树 / 导出共用同一份,新增筛选项只改这里(后端也只解析这一份)。
function findingFilterParams(q: Omit<FindingQuery, "page" | "pageSize">): URLSearchParams {
  const p = new URLSearchParams();
  if (q.severity && q.severity !== "all") p.set("severity", q.severity);
  if (q.status && q.status !== "all") p.set("status", q.status);
  if (q.vulnclass && q.vulnclass !== "all") p.set("vulnclass", q.vulnclass);
  if (q.task && q.task !== "all") p.set("task_id", q.task);
  if (q.query?.trim()) p.set("q", q.query.trim());
  if (q.sort) p.set("sort", q.sort);
  if (q.assetScope) p.set("asset_scope", q.assetScope);
  return p;
}

function interceptPageQuery(page: number, size: number, filter: InterceptApprovalFilter) {
  const query = new URLSearchParams({ page: String(page), size: String(size) });
  if (filter.status) query.set("status", filter.status);
  if (filter.decision_source) query.set("decision_source", filter.decision_source);
  return query.toString();
}

export const api = {
  // 后端应用版本号（release 时由 ldflags 注入，默认 "dev"）。
  health: () => get<{ ok: boolean; service: string; version: string }>("/health"),

  // ---- auth ----
  authStatus: () => get<{ initialized: boolean; user_count?: number; admin_count?: number }>("/auth/status"),
  login: (username: string, password: string) =>
    post<{ token: string; user: UserAccount; permissions: Permissions }>("/auth/login", { username, password }),
  initAdmin: (username: string, password: string, displayName?: string) =>
    post<{ token: string; user: UserAccount }>("/auth/init", {
      username,
      password,
      display_name: displayName ?? "",
    }),
  logout: () => post<{ ok: boolean }>("/auth/logout"),
  // me 返回当前账号 + 其部门角色 + 权限集合（前端据此隐藏无权操作的入口）。
  me: () => get<{ user: UserAccount; permissions: Permissions }>("/auth/me"),
  changePassword: (oldPassword: string, newPassword: string) =>
    post<{ ok: boolean }>("/auth/change-password", { old_password: oldPassword, new_password: newPassword }),

  // ---- users（账号管理，仅管理员）----
  users: () => get<{ users: UserAccount[] }>("/users").then((r) => arr(r.users)),
  createUser: (input: {
    username: string;
    password: string;
    displayName?: string;
    isAdmin?: boolean;
    departments?: DepartmentMembershipInput[];
  }) =>
    post<UserAccount>("/users", {
      username: input.username,
      password: input.password,
      display_name: input.displayName ?? "",
      is_admin: input.isAdmin ?? false,
      departments: input.departments ?? [],
    }),
  updateUser: (
    id: number,
    // 注意：这里不能叫 patch —— 会遮蔽上面的 patch() 请求助手，导致调用不可达。
    input: { displayName?: string; isAdmin?: boolean; enabled?: boolean; password?: string },
  ) =>
    patch<UserAccount>(`/users/${id}`, {
      display_name: input.displayName,
      is_admin: input.isAdmin,
      enabled: input.enabled,
      password: input.password ?? "",
    }),
  deleteUser: (id: number) => del<{ deleted: number }>(`/users/${id}`),
  setUserDepartments: (id: number, departments: DepartmentMembershipInput[]) =>
    put<UserAccount>(`/users/${id}/departments`, { departments }),

  // ---- departments（部门管理）----
  departments: () => get<{ departments: Department[] }>("/departments").then((r) => arr(r.departments)),
  createDepartment: (name: string, description: string) =>
    post<Department>("/departments", { name, description }),
  updateDepartment: (id: number, name: string, description: string) =>
    patch<Department>(`/departments/${id}`, { name, description }),
  deleteDepartment: (id: number) => del<{ deleted: number }>(`/departments/${id}`),
  departmentMembers: (id: number) =>
    get<{ members: DepartmentMember[] }>(`/departments/${id}/members`).then((r) => arr(r.members)),

  // ---- tasks ----
  tasks: () =>
    get<{ tasks: Task[]; active: string }>("/tasks").then((r) => ({ tasks: arr(r.tasks), active: r.active ?? "" })),
  task: (id: string) => get<Task>(`/tasks/${encodeURIComponent(id)}`),
  // uploadTaskSource 上传源码压缩包：后端自动解压到 staging 目录，返回 upload_id。
  // 创建任务时带上 upload_id，后端把源码搬进该任务目录（data/tasks/<id>/source）。
  uploadTaskSource: async (file: File, onProgress?: (percent: number) => void): Promise<TaskUpload> => {
    if (MOCK) return { upload_id: "mock", filename: file.name, file_count: 1, total_bytes: file.size, entries: [] };
    // 用 XHR 而不是 fetch：上传源码包可能很大，需要真实的进度反馈。
    return new Promise<TaskUpload>((resolve, reject) => {
      const fd = new FormData();
      fd.append("file", file);
      const token = getToken();
      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/tasks/upload");
      if (token) xhr.setRequestHeader("Authorization", `Bearer ${token}`);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable && onProgress) onProgress(Math.round((e.loaded / e.total) * 100));
      };
      xhr.onload = () => {
        let body: Record<string, unknown> = {};
        try {
          body = JSON.parse(xhr.responseText || "{}");
        } catch {
          body = {};
        }
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve(body as unknown as TaskUpload);
          return;
        }
        if (xhr.status === 401) {
          localStorage.removeItem("artex_token");
          document.cookie = "artex_token=; path=/; max-age=0";
          window.location.href = "/login";
          reject(new Error("未授权"));
          return;
        }
        reject(new Error(typeof body.error === "string" && body.error ? body.error : `上传失败(${xhr.status})`));
      };
      xhr.onerror = () => reject(new Error("上传失败：网络错误"));
      xhr.send(fd);
    });
  },
  // 未消费的上传（刷新页面后可以继续创建任务；放弃时删除）。
  taskUploads: () => get<{ uploads: TaskUpload[] }>("/tasks/uploads").then((r) => arr(r.uploads)),
  deleteTaskUpload: (uploadId: string) => del<{ deleted: string }>(`/tasks/uploads/${encodeURIComponent(uploadId)}`),

  // ---- GitHub 仓库导入（见 docs/github-source-import.md）----
  // importFromGithub 第一次调用可能返回 needs_confirm（仓库体积超过软上限）：
  // 此时后端没有建行也没有下载，带 confirmLarge=true 重发即可。
  importFromGithub: (repoUrl: string, ref?: string, confirmLarge?: boolean, signal?: AbortSignal) =>
    http<ImportGithubResult>("/tasks/import/github", {
      method: "POST",
      body: JSON.stringify({ repo_url: repoUrl, ref: ref ?? "", confirm_large: confirmLarge ?? false }),
      signal,
    }),
  // taskUpload 轮询一次导入状态。注意路径是 task-uploads 而不是 tasks/uploads：
  // 后者与 /tasks/{id}/coverage 是二义路由，后端注册会 panic。
  taskUpload: (id: string) => get<TaskUpload>(`/task-uploads/${encodeURIComponent(id)}`),
  confirmTaskUpload: (id: string, accept: boolean) =>
    post<{ state: string }>(`/task-uploads/${encodeURIComponent(id)}/confirm`, { accept }),
  // 手动开启任务：新建的任务不会自动开跑，用户进任务详情后点「开启任务」。
  startTask: (id: string) => post<Task>(`/tasks/${encodeURIComponent(id)}/start`),
  // 管理员调整任务归属部门（null = 移出部门）。
  setTaskDepartment: (id: string, departmentId: number | null) =>
    post<Task>(`/tasks/${encodeURIComponent(id)}/department`, { department_id: departmentId }),
  createTask: (input: {
    name?: string;
    categoryId?: number;
    description: string;
    goal: string;
    llmProfileIds?: number[];
    sourceTaskIds?: string[];
    companyIds?: number[];
    timeoutSeconds?: number;
    seedFirstIntent?: boolean;
    planHeartbeatSeconds?: number;
    coverageEnabled?: boolean;
    interceptRules?: AssetInterceptRuleInput[];
    /** 任务归属部门：普通用户必须是自己拥有的部门；管理员可省略。 */
    departmentId?: number | null;
    /** 上传源码包得到的 upload_id（web 建任务的必经步骤）。 */
    uploadId?: string;
    /** 创建后是否立即开跑；默认 false = 等用户在任务详情里手动开启。 */
    autoStart?: boolean;
  }) =>
    post<Task>("/tasks", {
      name: input.name ?? "",
      category_id: input.categoryId ?? null,
      description: input.description,
      goal: input.goal,
      llm_profile_ids: input.llmProfileIds ?? [],
      source_task_ids: input.sourceTaskIds ?? [],
      company_ids: input.companyIds ?? [],
      timeout_seconds: input.timeoutSeconds ?? 0,
      seed_first_intent: input.seedFirstIntent ?? false,
      plan_heartbeat_seconds: input.planHeartbeatSeconds ?? 0, // 0 = 后端归一到默认 600(10min)
      coverage_enabled: input.coverageEnabled ?? true, // 默认开;false=关闭资产覆盖度功能
      intercept_rules: input.interceptRules ?? [], // 任务级资产拦截规则
      department_id: input.departmentId ?? null,
      upload_id: input.uploadId ?? "",
      auto_start: input.autoStart ?? false,
    }),
  taskCategories: () => get<{ categories: TaskCategory[] }>("/task-categories").then((r) => arr(r.categories)),
  updateTask: (id: string, input: { name?: string; pinned?: boolean }) => patch<Task>(`/tasks/${id}`, input),
  renameTask: (id: string, name: string) => patch<Task>(`/tasks/${id}`, { name }),
  pinTask: (id: string, pinned: boolean) => patch<Task>(`/tasks/${id}`, { pinned }),
  createTaskCategory: (name: string) => post<TaskCategory>("/task-categories", { name }),
  renameTaskCategory: (id: number, name: string) => patch<TaskCategory>(`/task-categories/${id}`, { name }),
  deleteTaskCategory: (id: number) => del<{ deleted: number }>(`/task-categories/${id}`),
  updateTaskCategory: (taskId: string, categoryId?: number) =>
    patch<Task>(`/tasks/${taskId}/category`, { category_id: categoryId ?? null }),
  // categoryId 省略/undefined = 移出分类（后端收到 null）
  updateTasksCategory: (taskIds: string[], categoryId?: number) =>
    post<{ items: BatchCategoryItem[]; category: TaskCategory | null }>("/tasks/category/batch", {
      task_ids: taskIds,
      category_id: categoryId ?? null,
    }),
  taskTemplates: () => get<{ templates: TaskTemplate[] }>("/task-templates").then((r) => arr(r.templates)),
  createTaskTemplate: (
    input: Pick<TaskTemplate, "name" | "description" | "goal" | "category_id" | "intercept_rules">,
  ) => post<TaskTemplate>("/task-templates", input),
  updateTaskTemplate: (
    id: number,
    input: Partial<Pick<TaskTemplate, "name" | "description" | "goal" | "category_id" | "intercept_rules">>,
  ) => patch<TaskTemplate>(`/task-templates/${id}`, input),
  deleteTaskTemplate: (id: number) => del<{ deleted: number }>(`/task-templates/${id}`),
  updateTaskLLMProfiles: (id: string, llmProfileIds: number[], activeLLMProfileId?: number) =>
    put<{
      id: string;
      llm_profile_ids: number[];
      active_llm_profile_id?: number;
      llm_failover_state: string;
      reopened_intents: number;
      switch_event?: Activity;
    }>(`/tasks/${id}/llm`, {
      llm_profile_ids: llmProfileIds,
      active_llm_profile_id: activeLLMProfileId ?? null,
    }),
  deleteTask: (id: string, options: DeleteTaskOptions) => del<DeleteTaskResult>(`/tasks/${id}`, options),
  controlTask: (id: string, action: "pause" | "resume") =>
    post<{ id: string; paused: boolean; queued: boolean; status: string }>(`/tasks/${id}/control`, { action }),
  controlTasksBatch: (taskIds: string[], action: "pause" | "resume") =>
    post<{ items: BatchControlItem[] }>("/tasks/control/batch", { task_ids: taskIds, action }),
  taskArchives: (input?: { page?: number; size?: number; q?: string; state?: string }) => {
    const query = new URLSearchParams();
    query.set("page", String(input?.page ?? 1));
    query.set("size", String(input?.size ?? 20));
    if (input?.q) query.set("q", input.q);
    if (input?.state) query.set("state", input.state);
    return get<TaskArchivePage>(`/task-archives?${query.toString()}`).then((response) => ({
      ...response,
      items: arr(response.items),
    }));
  },
  taskArchive: (id: number) => get<TaskArchive>(`/task-archives/${id}`),
  archiveTask: (id: string) => post<TaskArchive>(`/tasks/${id}/archive`),
  archiveTasks: (taskIds: string[]) =>
    post<{ items: ArchiveBatchItem[] }>("/tasks/archive/batch", { task_ids: taskIds }),
  restoreTaskArchive: (id: number) => post<TaskArchive>(`/task-archives/${id}/restore`),
  restoreTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/restore/batch", { archive_ids: archiveIds }),
  deleteTaskArchive: (id: number) => del<TaskArchive>(`/task-archives/${id}`),
  deleteTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/delete/batch", { archive_ids: archiveIds }),
  controlIntent: (
    taskId: string,
    intentId: string,
    action: "pause" | "resume" | "cancel",
    reason?: string,
    // cancel 专用:soft(默认,假删除,意图置 deleted + 记删除原因)| hard(真删除,级联移除独占子孙)。
    mode?: "soft" | "hard",
  ) =>
    post<{
      id: number;
      state: "paused" | "open" | "deleted" | "";
      deleted?: { intents: number; facts: number; findings: number; activities: number };
    }>(`/tasks/${taskId}/intents/${intentId}/control`, { action, reason: reason ?? "", mode: mode ?? "soft" }),
  sendWorkerMessage: (taskId: string, intentId: string, message: string, requestId: string) =>
    post<{
      id: number;
      state: "running";
      accepted: true;
      request_id: string;
    }>(`/tasks/${taskId}/intents/${intentId}/messages`, { message, request_id: requestId }),
  taskLLMResolution: (id: string) => get<TaskLLMResolutions>(`/tasks/${id}/llm/resolution`),
  // 重跑一条没跑成功的意图(blocked/exhausted/stopped)：置回 open，worker 会重新认领、从头再跑。
  rerunIntent: (taskId: string, intentId: string) =>
    post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/${intentId}/rerun`),
  // 批量重跑本任务全部 blocked 意图（一次网络/LLM 断连导致多条 blocked 时一键全部重试）。
  rerunBlocked: (taskId: string) => post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/rerun-blocked`),
  setActive: (id: string) => post<{ active: string }>("/active", { id }),
  // ---- stats ----
  stats: (task?: string) => get<Stats>(`/stats${tq(task)}`),
  // 资产测试覆盖度(粗估，供参考)：范围内资产被 fact 碰过的占比 + 按类型的 总数/已测。
  taskCoverage: (id: string) =>
    get<{
      enabled: boolean; // 资产覆盖度功能是否开启；false 时其余字段为零值
      scope_rows: number;
      denominator: number;
      tested: number;
      pct: number | null;
      by_type: { type: string; total: number; tested: number }[];
    }>(`/tasks/${id}/coverage`),
  // 资产覆盖图：范围内全部资产 + 连接用的根域名/公司节点，含 tested/in_scope。
  taskCoverageGraph: (id: string) => get<CoverageGraphData>(`/tasks/${id}/coverage-graph`),

  // ---- 第三方依赖（Java jar / .m2）----
  // 清单：坐标/类数/大小/源码状态 + 当前反编译能力。后端只读库，不触发反编译。
  taskDeps: (id: string) => get<TaskDependencyList>(`/tasks/${encodeURIComponent(id)}/deps`),
  // 上传依赖包（内含 jar 的 zip）：后端只解压 + 建类名索引，**不反编译**。
  uploadTaskDeps: async (
    id: string,
    file: File,
    onProgress?: (percent: number) => void,
  ): Promise<{ jars: number; dependencies: number; indexed_class: number; engine: string }> => {
    if (MOCK) return { jars: 1, dependencies: 1, indexed_class: 1, engine: "mock" };
    return new Promise((resolve, reject) => {
      const fd = new FormData();
      fd.append("file", file);
      const token = getToken();
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/tasks/${encodeURIComponent(id)}/deps`);
      if (token) xhr.setRequestHeader("Authorization", `Bearer ${token}`);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable && onProgress) onProgress(Math.round((e.loaded / e.total) * 100));
      };
      xhr.onload = () => {
        let body: Record<string, unknown> = {};
        try {
          body = JSON.parse(xhr.responseText || "{}");
        } catch {
          body = {};
        }
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve(body as { jars: number; dependencies: number; indexed_class: number; engine: string });
          return;
        }
        reject(new Error(typeof body.error === "string" && body.error ? body.error : `上传失败(${xhr.status})`));
      };
      xhr.onerror = () => reject(new Error("上传失败：网络错误"));
      xhr.send(fd);
    });
  },
  // 从服务器本地 Maven 仓库（~/.m2/repository）按坐标导入 jar：不需要上传依赖包。
  // scan_source=true 时后端还会扫本任务源码里的 pom.xml，把直接依赖一并导入。
  // 走 post()/http()，MOCK 模式下与同区的 rescan/clear/materialize 一样由 mockHandle 兜底。
  importTaskDeps: (id: string, body: { gavs?: string[]; scan_source?: boolean }) =>
    post<TaskDepsImportResult>(`/tasks/${encodeURIComponent(id)}/deps/import`, body),
  // 重新扫描已上传的 jar（只重建清单与索引，不反编译）。
  rescanTaskDeps: (id: string) =>
    post<{ jars: number; dependencies: number; indexed_class: number; engine: string }>(
      `/tasks/${encodeURIComponent(id)}/deps/rescan`,
    ),
  // 清空本任务的依赖（DB 行 + deps 目录）。
  clearTaskDeps: (id: string) => post<{ ok: boolean }>(`/tasks/${encodeURIComponent(id)}/deps/clear`),
  // 按需把【一个】依赖变成可读源码。可能耗时几十秒（有 -sources.jar 时是秒级解压）。
  materializeTaskDep: (id: string, depId: number) =>
    post<DepMaterializeResult>(`/tasks/${encodeURIComponent(id)}/deps/${depId}/materialize`),
  // 全局 llm_usage 聚合（仪表盘新版 token 视图）：按 profile 总量 + 按天分桶。
  usageStats: (days = 365) => get<UsageStats>(`/tokens/usage?days=${days}`),
  // ---- 目标管理（总览）----
  // 本任务全部目标（text/vulnclass/state）。
  taskGoals: (id: string) =>
    get<{ goals: TaskGoal[] | null }>(`/tasks/${id}/goals`).then((response) => ({ goals: arr(response.goals) })),
  // 人工新增目标：写入图谱并通知 planner、复活任务。
  addGoal: (id: string, text: string, vulnclass?: string) =>
    post<TaskGoal>(`/tasks/${id}/goals`, { text, vulnclass: vulnclass ?? "" }),
  // 人工修改目标文本（及 vulnclass）：通知 planner「由 old 变为 new」、复活任务。
  updateGoal: (id: string, goalId: string, text: string, vulnclass?: string) =>
    patch<TaskGoal>(`/tasks/${id}/goals/${goalId}`, { text, vulnclass: vulnclass ?? "" }),
  // 人工删除目标（硬删除）：通知 planner，删除不复活任务。
  deleteGoal: (id: string, goalId: string) => del<{ ok: boolean }>(`/tasks/${id}/goals/${goalId}`),

  // ---- 操作约束管理（总览）----
  // 本任务全部操作约束（allow/deny）。
  taskConstraints: (id: string) =>
    get<{ constraints: TaskConstraint[] | null }>(`/tasks/${id}/constraints`).then((response) => ({
      constraints: arr(response.constraints),
    })),
  // 新增约束（不通知 planner，下一轮规划自然读到）。
  addConstraint: (id: string, text: string, kind: TaskConstraint["kind"]) =>
    post<TaskConstraint>(`/tasks/${id}/constraints`, { text, kind }),
  // 修改约束（文本 + allow/deny）。
  updateConstraint: (id: string, constraintId: string, text: string, kind: TaskConstraint["kind"]) =>
    patch<TaskConstraint>(`/tasks/${id}/constraints/${constraintId}`, { text, kind }),
  // 删除约束。
  deleteConstraint: (id: string, constraintId: string) =>
    del<{ ok: boolean }>(`/tasks/${id}/constraints/${constraintId}`),

  // ---- 任务级资产拦截/允许规则（总览）----
  taskInterceptRules: (id: string) =>
    get<{ rules: AssetInterceptRule[] | null }>(`/tasks/${id}/intercept-rules`).then((r) => arr(r.rules)),
  createTaskInterceptRule: (id: string, rule: AssetInterceptRuleInput) =>
    post<AssetInterceptRule>(`/tasks/${id}/intercept-rules`, rule),
  updateTaskInterceptRule: (id: string, ruleId: number, rule: AssetInterceptRuleInput) =>
    put<AssetInterceptRule>(`/tasks/${id}/intercept-rules/${ruleId}`, rule),
  deleteTaskInterceptRule: (id: string, ruleId: number) =>
    del<{ deleted: boolean }>(`/tasks/${id}/intercept-rules/${ruleId}`),
  toggleTaskInterceptRule: (id: string, ruleId: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/tasks/${id}/intercept-rules/${ruleId}/toggle`, { enabled }),

  // 本任务测试范围列表（含继承自来源任务的范围）。
  taskScope: (id: string) => get<{ scope: TaskScopeRow[] }>(`/tasks/${id}/scope`),
  // 手动新增一条测试范围（kind=company/root_domain/subdomain/ip/cidr）。
  addTaskScope: (id: string, kind: string, value: string, reason?: string) =>
    post<TaskScopeRow>(`/tasks/${id}/scope`, { kind, value, reason }),
  // 删除本任务的一条测试范围。
  deleteTaskScope: (id: string, scopeId: number) => del<{ ok: boolean }>(`/tasks/${id}/scope/${scopeId}`),
  // 某资产在本任务里关联的意图 / 事实 / 发现（覆盖图节点抽屉用）。
  taskAssetRefs: (id: string, assetId: number) => get<CoverageAssetRefs>(`/tasks/${id}/asset-refs?asset_id=${assetId}`),

  // ---- workspace file manager (workDir) ----
  workspaceList: (path = "") => get<WorkspaceListing>(`/workspace/list?path=${encodeURIComponent(path)}`),
  workspaceRead: (path: string) => get<WorkspaceFile>(`/workspace/read?path=${encodeURIComponent(path)}`),
  workspaceWrite: (path: string, content: string) =>
    post<{ ok: boolean; path: string }>(`/workspace/write`, { path, content }),
  workspaceMkdir: (path: string) => post<{ ok: boolean; path: string }>(`/workspace/mkdir`, { path }),
  workspaceDelete: (path: string) => del<{ ok: boolean }>(`/workspace/delete?path=${encodeURIComponent(path)}`),
  workspaceUpload: async (dir: string, files: File[]) => {
    if (MOCK) return { uploaded: files.length };
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    const token = getToken();
    const r = await fetch(`/api/workspace/upload?path=${encodeURIComponent(dir)}`, {
      method: "POST",
      headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: fd,
    });
    if (!r.ok) throw new Error(`上传失败: ${r.status}`);
    return r.json() as Promise<{ uploaded: number }>;
  },
  workspaceDownload: async (path: string) => {
    let blob: Blob;
    if (MOCK) {
      blob = new Blob([`（demo）${path} 的下载内容示例。`], { type: "text/plain" });
    } else {
      const token = getToken();
      const r = await fetch(`/api/workspace/download?path=${encodeURIComponent(path)}`, {
        headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      });
      if (!r.ok) throw new Error(`下载失败: ${r.status}`);
      blob = await r.blob();
    }
    const objUrl = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = objUrl;
    a.download = path.split("/").pop() || "download";
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(objUrl);
  },

  // ---- assets ----
  // Server-side paginated: pass limit/offset, get back the page + full match total.
  assets: (type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(`/assets?type=${type}&limit=${limit}&offset=${offset}`).then(
      (r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 }),
    ),
  searchAssets: (dsl: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?dsl=${encodeURIComponent(dsl)}${type ? `&type=${encodeURIComponent(type)}` : ""}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  assetCounts: (taskId = "") =>
    get<Record<string, number>>(`/assets/counts${taskId ? `?task_id=${encodeURIComponent(taskId)}` : ""}`),
  deleteAssets: (ids: number[]) =>
    http<{ deleted: number }>("/assets", { method: "DELETE", body: JSON.stringify({ ids }) }),
  // 登记资产（接口用 type=endpoint + method/url/permission）。task_id 会把资产挂到该任务上，
  // 已存在的接口按 (url, method) 合并：权限只在本次给了非空值时覆盖，task_ids 取并集。
  createAssets: (
    taskId: string,
    assets: {
      type: string;
      domain?: string;
      ip?: string;
      url?: string;
      method?: string;
      params?: Record<string, unknown>[];
      permission?: string;
      service_ip?: string;
    }[],
  ) =>
    post<{ results: { index: number; id: number; type: string }[]; errors: { index: number; error: string }[] }>(
      "/assets",
      { task_id: Number(taskId), assets },
    ),
  // task-scoped view of the same endpoint — server-side paginated like `assets`
  taskAssets: (taskId: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?task_id=${encodeURIComponent(taskId)}&type=${encodeURIComponent(type)}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  // DSL search scoped to a task — the backend forces the task_id filter, so it
  // always stays within that task's assets (same DSL grammar as `searchAssets`).
  searchTaskAssets: (taskId: string, dsl: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?task_id=${encodeURIComponent(taskId)}&dsl=${encodeURIComponent(dsl)}&type=${encodeURIComponent(type)}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  attachTaskAssets: (taskId: string, assetIds: number[], sourceSummary: string) =>
    post<TaskAssetMutation>(`/tasks/${taskId}/assets`, {
      asset_ids: assetIds,
      source_summary: sourceSummary,
    }),
  registerTaskAssetScopes: (taskId: string, scope: CompanyScopeRule[]) =>
    post<TaskAssetScopeMutation>(`/tasks/${taskId}/assets`, { scope }),
  detachTaskAsset: (taskId: string, assetId: number) => del<{ detached: number }>(`/tasks/${taskId}/assets/${assetId}`),
  taskIntentAssets: (taskId: string) =>
    get<{ assets: IntentAsset[] }>(`/tasks/${taskId}/intent-assets`).then((r) => arr(r.assets)),

  // ---- companies (企业 + 资产范围；归属唯一来源) ----
  companies: () => get<Company[]>("/companies").then(arr),
  createCompany: (name: string, scope: CompanyScopeRule[]) =>
    post<{ id: number; created: boolean; scope_added?: number; scope_invalid?: number; scope_errors?: string[] }>(
      "/companies",
      {
        name,
        scope,
      },
    ),
  addCompanyScope: (id: number, scope: CompanyScopeRule[], reason = "") =>
    post<CompanyScopeMutation>(`/companies/${id}/scope`, {
      scope,
      reason,
    }),
  updateCompanyScope: (id: number, scope: CompanyScopeRule[], reason = "") =>
    post<CompanyScopeMutation>(`/companies/${id}/scope`, {
      scope,
      reason,
      reset: true,
    }),
  deleteCompany: (id: number, deleteAssets = false) =>
    http<{ deleted: number; assets_deleted: number }>(`/companies/${id}`, {
      method: "DELETE",
      body: JSON.stringify({ delete_assets: deleteAssets }),
    }),

  // ---- exploration (per task) ----
  frontier: (task?: string) => get<TaskNode[]>(`/exploration/frontier${tq(task)}`).then(arr),
  findings: (task?: string) => get<Finding[]>(`/exploration/findings${tq(task)}`).then(arr),
  findingsPage: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingsPage>(`/exploration/findings?${p.toString()}`);
  },
  findingGroups: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingGroupsPage>(`/exploration/findings/groups?${p.toString()}`);
  },
  // findingAssetTree 取「按资产」视图的左侧树:只含有发现的资产及其祖先,
  // 节点带子树聚合计数。不分页——树是导航结构,一次取完。
  findingAssetTree: (q: Omit<FindingQuery, "page" | "pageSize">) =>
    get<FindingAssetTree>(`/exploration/findings/asset-tree?${findingFilterParams(q).toString()}`),
  findingStats: () => get<FindingStats>("/exploration/findings/stats"),
  // exportFindings 触发发现页导出并下载文件。scope=selected 时传 ids(finding_id 列表);
  // scope=filtered 时传当前筛选(沿用 FindingQuery 的筛选字段);scope=all 忽略筛选。
  exportFindings: async (opts: {
    format: "md-single" | "md-zip" | "csv" | "json";
    scope: "filtered" | "all" | "selected";
    filters?: Omit<FindingQuery, "page" | "pageSize">;
    ids?: string[];
  }) => {
    const p = new URLSearchParams({ format: opts.format, scope: opts.scope });
    if (opts.scope === "selected") {
      p.set("ids", (opts.ids ?? []).join(","));
    } else if (opts.scope === "filtered" && opts.filters) {
      for (const [k, v] of findingFilterParams(opts.filters)) p.set(k, v);
    }
    const token = getToken();
    const r = await fetch(`/api/exploration/findings/export?${p.toString()}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(`export: ${r.status}`);
    const blob = await r.blob();
    // 文件名优先取后端 Content-Disposition,取不到则用默认名。
    const disp = r.headers.get("Content-Disposition") ?? "";
    const m = disp.match(/filename="?([^"]+)"?/);
    const filename = m?.[1] ?? `findings-export`;
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  },
  getFinding: (id: string, contextTaskId?: string) =>
    get<Finding>(
      `/exploration/findings/${id}${contextTaskId ? `?context_task=${encodeURIComponent(contextTaskId)}` : ""}`,
    ),
  findingTraffic: (id: string, contextTask?: string) =>
    get<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  bindFindingTraffic: (id: string, traffic_refs: TrafficEvidenceRef[], contextTask?: string) =>
    post<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { traffic_refs },
    ),
  editFindingTraffic: (
    id: string,
    bindingId: string,
    version: number,
    fields: { role: TrafficEvidenceRole; note: string },
    contextTask?: string,
  ) =>
    patch<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version, ...fields },
    ),
  removeFindingTraffic: (id: string, bindingId: string, version: number, contextTask?: string) =>
    del<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version },
    ),
  orderFindingTraffic: (id: string, binding_ids: string[], version: number, contextTask?: string) =>
    http<FindingTraffic>(
      `/exploration/findings/${id}/traffic/order${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { method: "PUT", body: JSON.stringify({ binding_ids, version }) },
    ),
  findingTrafficDetail: (id: string, bindingId: string, contextTask?: string) =>
    get<FindingTrafficDetail>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  findingTrafficBody: (
    id: string,
    bindingId: string,
    side: "request" | "response",
    offset: number,
    contextTask?: string,
  ) =>
    get<EvidenceBodyPreview>(
      `/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&offset=${offset}${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  downloadFindingTrafficBody: async (
    id: string,
    bindingId: string,
    side: "request" | "response",
    contextTask?: string,
  ) => {
    const token = getToken();
    const response = await fetch(
      `/api/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&download=1${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} },
    );
    if (!response.ok) {
      const error = await response.json().catch(() => ({ error: "下载失败" }));
      throw new Error(error.error ?? "下载失败");
    }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `evidence-${bindingId}-${side}.bin`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
  // 原始报文：head + 空行 + 正文，可直接复制粘贴到 Repeater/curl 复测。
  findingTrafficRaw: (id: string, bindingId: string, side: "request" | "response", contextTask?: string) =>
    get<FindingRawPacket>(
      `/exploration/findings/${id}/traffic/${bindingId}/raw?side=${side}${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  downloadFindingTrafficRaw: async (
    id: string,
    bindingId: string,
    side: "request" | "response",
    contextTask?: string,
  ) => {
    const token = getToken();
    const response = await fetch(
      `/api/exploration/findings/${id}/traffic/${bindingId}/raw?side=${side}&download=1${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} },
    );
    if (!response.ok) {
      const error = await response.json().catch(() => ({ error: "下载失败" }));
      throw new Error(error.error ?? "下载失败");
    }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `finding-${id}-${side}.http`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
  // 推演复测报文（模型依据源码生成，非抓包证据）：读 / 人工修订 / 清除。
  // 写入入口是 agent 的 report_finding.retest_packets，这里只提供人工纠偏。
  // 报文按接口一份：列表接口同时返回该漏洞锚定的全部接口槽位（含尚未生成报文的），
  // 这样页面能把「哪个接口还没有报文」直接摆出来。
  findingPackets: (id: string, contextTask?: string) =>
    get<FindingPackets>(
      `/exploration/findings/${id}/packets${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ).then((r) => ({ packets: r.packets ?? [], interfaces: r.interfaces ?? [] })),
  // assetID 传 "0" 表示「未指定接口」那一份。
  saveFindingPacket: (
    id: string,
    assetID: string,
    fields: { raw_request?: string; raw_response?: string; grounded_refs?: string[]; assumptions?: string[] },
    contextTask?: string,
  ) =>
    patch<FindingPackets>(
      `/exploration/findings/${id}/packets/${assetID}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      fields,
    ).then((r) => ({ packets: r.packets ?? [], interfaces: r.interfaces ?? [] })),
  deleteFindingPacket: (id: string, assetID: string, contextTask?: string) =>
    del<{ deleted: boolean }>(
      `/exploration/findings/${id}/packets/${assetID}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  // 推演报文是纯文本产物，浏览器侧直接落盘即可，不需要后端导出接口。
  downloadFindingPacket: (id: string, side: "request" | "response", raw: string) => {
    const blob = new Blob([raw], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `finding-${id}-${side}.llm.http`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
  // 漏洞链路:该漏洞节点回溯到任务初始节点的子图(节点 + 关系)。
  findingLineage: (id: string) => get<{ nodes: TaskNode[]; edges: Edge[] }>(`/exploration/findings/${id}/lineage`),
  setFindingStatus: (id: string, status: FindingStatus) => patch<Finding>(`/exploration/findings/${id}`, { status }),
  setFindingSeverity: (id: string, severity: Severity) => patch<Finding>(`/exploration/findings/${id}`, { severity }),
  // 一次保存漏洞的名称/类别/严重等级(发现列表行内编辑用),只传出现的字段。
  updateFinding: (
    id: string,
    fields: { name?: string; vulnclass?: string; severity?: Severity; status?: FindingStatus },
  ) => patch<Finding>(`/exploration/findings/${id}`, fields),
  // 删除漏洞:移除 findings 记录 + 来源探索节点(从发现列表/任务发现 Tab/探索图一并消失)。
  deleteFinding: (id: string) => del<{ deleted: boolean; id: number }>(`/exploration/findings/${id}`),
  findingRetests: (id: string) =>
    get<{ retests: FindingRetest[] }>(`/exploration/findings/${encodeURIComponent(id)}/retests`).then((r) =>
      arr(r.retests),
    ),
  activeFindingRetests: () =>
    get<{ retests: ActiveFindingRetest[] }>("/exploration/findings/retests/active").then((r) => arr(r.retests)),
  startFindingRetest: (id: string, notes: string) =>
    post<{ retest: FindingRetest; created: boolean }>(`/exploration/findings/${encodeURIComponent(id)}/retests`, {
      notes,
    }),
  deepenFinding: (id: string, description: string) =>
    post<FindingDeepenResponse>(`/exploration/findings/${id}/deepen`, { description }),
  intents: (task?: string) => get<TaskNode[]>(`/exploration/intents${tq(task)}`).then(arr),
  tokenStats: (task?: string) =>
    get<{ workers: TokenUsage[]; sessions: SessionTokenUsage[]; total: TokenTotal }>(
      `/exploration/tokens${tq(task)}`,
    ).then((r) => ({
      workers: arr(r.workers),
      sessions: arr(r.sessions),
      total: r.total,
    })),
  tokenDaily: (days = 30) => get<DailyTokenBucket[]>(`/tokens/daily?days=${days}`).then(arr),
  conversationTokens: () =>
    get<ConvTokenSummary[]>("/tokens/conversations")
      .then(arr)
      .catch(() => [] as ConvTokenSummary[]),
  explorationGraph: (task?: string) => get<{ nodes: TaskNode[]; edges: Edge[] }>(`/exploration/graph${tq(task)}`),
  // 播报板:服务端按创建顺序分页的探索节点(默认最新在前)。
  explorationNodes: (task: string, query: ExplorationNodeQuery = {}) => {
    const q = new URLSearchParams();
    if (task) q.set("task", task);
    q.set("page", String(query.page ?? 1));
    q.set("size", String(query.size ?? 20));
    if (query.kinds?.length) q.set("kind", query.kinds.join(","));
    if (query.states?.length) q.set("state", query.states.join(","));
    if (query.q?.trim()) q.set("q", query.q.trim());
    if (query.order === "asc") q.set("order", "asc");
    return get<ExplorationNodePage>(`/exploration/nodes?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      total: r.total ?? 0,
      page: r.page ?? 1,
      size: r.size ?? query.size ?? 20,
      edges: arr(r.edges),
      refs: r.refs ?? {},
      assets: r.assets ?? {},
    }));
  },
  activity: (task?: string, opts?: { intent?: string; since?: number; limit?: number }) => {
    const q = new URLSearchParams();
    if (task) q.set("task", task);
    if (opts?.intent) q.set("intent", opts.intent);
    if (opts?.since) q.set("since", String(opts.since));
    if (opts?.limit) q.set("limit", String(opts.limit));
    return get<{ items: Activity[]; cursor: number }>(`/exploration/activity?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      cursor: r.cursor ?? 0,
    }));
  },
  activityDetail: (id: number, task?: string) => get<{ detail: string }>(`/exploration/activity/${id}${tq(task)}`),
  // 跨任务的最新事件（仪表盘活动流）。不要用 activity(undefined) 代替：那个接口会解析到
  // 当前活动任务，并从 sinceID 起做「最旧优先」扫描，返回的是最老的一批事件。
  recentActivity: (limit = 30) =>
    get<{ items: Activity[] }>(`/exploration/activity/recent?limit=${limit}`).then((r) => ({
      items: arr(r.items),
    })),
  // Reverse-paginated session history. session = "main" | "plan" | "intent:<id>".
  // before=0 → latest page; before=<id> → the older page ending before that id.
  // snapshotCursor is the TASK-level max id at query time — open the task SSE at
  // since=snapshotCursor so history (id≤cursor) and the live tail (id>cursor) meet
  // gap-free. hasMore = still-older steps exist (drives scroll-up loading).
  activityHistory: (task: string, session: string, before = 0, limit = 200) => {
    const q = new URLSearchParams({ session, limit: String(limit) });
    if (task) q.set("task", task);
    if (before > 0) q.set("before", String(before));
    return get<{ items: Activity[]; snapshot_cursor: number; earliest_cursor: number; has_more: boolean }>(
      `/exploration/activity/history?${q.toString()}`,
    ).then((r) => ({
      items: arr(r.items),
      snapshotCursor: r.snapshot_cursor ?? 0,
      earliestCursor: r.earliest_cursor ?? 0,
      hasMore: !!r.has_more,
    }));
  },
  // Paged worker (intent) session list — reaches past the legacy fixed 300 cap.
  // before=0 → newest page; before=<id> → older page. has_more = older intents exist.
  intentsPage: (task: string, before = 0, limit = 300) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (task) q.set("task", task);
    q.set("before", String(before > 0 ? before : 0));
    q.set("page", "1"); // marker so the backend returns the paged {items,has_more} shape
    return get<{ items: TaskNode[]; has_more: boolean }>(`/exploration/intents?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      hasMore: !!r.has_more,
    }));
  },

  // Main-agent conversation segments of a task. Each segment is a resettable session
  // (clean transcript/context) over the same task; `current` is the writable one.
  mainSessions: (task: string) =>
    get<{ sessions: { seq: number; created_at: string }[]; current: number }>(
      `/exploration/main-sessions${tq(task)}`,
    ).then((r) => ({ sessions: arr(r.sessions), current: r.current ?? 0 })),
  // Start a fresh main-agent session segment (does not touch the task's graph/assets/goal).
  newMainSession: (task: string) =>
    post<{ seq: number; created_at: string; current: number }>(`/exploration/main-session/new${tq(task)}`),

  // ---- traffic / audit / report / chat ----
  audit: (task?: string) => get<Audit>(`/audit${tq(task)}`),
  traffic: (
    page = 0,
    size = 100,
    host = "",
    method = "",
    q = "",
    opts: {
      body?: string;
      path?: string;
      status?: string;
      respMin?: string;
      respMax?: string;
      sort?: string;
      order?: string;
    } = {},
  ) =>
    get<TrafficResp>(
      `/traffic?page=${page}&size=${size}` +
        (host ? `&host=${encodeURIComponent(host)}` : "") +
        (method && method !== "all" ? `&method=${encodeURIComponent(method)}` : "") +
        (q ? `&q=${encodeURIComponent(q)}` : "") +
        (opts.body ? `&body=${encodeURIComponent(opts.body)}` : "") +
        (opts.path ? `&path=${encodeURIComponent(opts.path)}` : "") +
        (opts.status && opts.status !== "all" ? `&status=${encodeURIComponent(opts.status)}` : "") +
        (opts.respMin ? `&resp_min=${encodeURIComponent(opts.respMin)}` : "") +
        (opts.respMax ? `&resp_max=${encodeURIComponent(opts.respMax)}` : "") +
        (opts.sort ? `&sort=${encodeURIComponent(opts.sort)}` : "") +
        (opts.order ? `&order=${encodeURIComponent(opts.order)}` : ""),
    ),
  trafficExchange: (id: string) => get<TrafficDetail>(`/traffic/exchange?id=${encodeURIComponent(id)}`),
  trafficHosts: () => get<{ hosts: TrafficHost[] }>(`/traffic/hosts`),
  trafficDeleteHost: (host: string) => del<{ deleted: number }>(`/traffic?host=${encodeURIComponent(host)}`),
  trafficDeleteHosts: (hosts: string[]) => del<{ deleted: number }>(`/traffic/hosts`, { hosts }),
  // Purges every exchange and compacts the index; `reclaimed` is the bytes of
  // index handed back to the filesystem. Evidence bound to findings is kept.
  trafficDeleteAll: () => del<{ deleted: number; reclaimed: number }>(`/traffic/all`),

  // ---- app settings (runtime toggles) ----
  settings: () => get<Settings>(`/settings`),
  setSettings: (patch: Partial<Settings>) => put<Settings>(`/settings`, patch),
  // Run a real "test" search with the given (or saved) config to verify it works.
  testWebSearch: (patch: {
    web_search_backend?: string;
    web_search_proxy?: string;
    brave_search_api_key?: string;
    tavily_search_api_key?: string;
  }) => post<{ ok: boolean; error?: string; count?: number; backend?: string }>(`/settings/web-search/test`, patch),

  // ---- 漏洞 IM 推送 ----
  // 渠道是多实例资源（同一类型可配多个机器人、各有过滤规则），因此独立成组，
  // 不塞进扁平的 settings 键值里。
  notifyMeta: () => get<NotificationMeta>(`/notify/meta`),
  notifyChannels: () =>
    get<{ channels: NotificationChannel[] }>(`/notify/channels`).then((r) => arr(r.channels)),
  notifyCreateChannel: (payload: {
    name: string;
    kind: string;
    enabled?: boolean;
    mode?: string;
    config: Record<string, unknown>;
    filter?: NotificationFilter;
    rate_per_min?: number;
  }) => post<{ id: number }>(`/notify/channels`, payload),
  // PATCH 语义：只提交要改的字段。config 里的掩码值原样回传即表示「保持原值」。
  notifyUpdateChannel: (
    id: number,
    payload: {
      name?: string;
      kind?: string;
      enabled?: boolean;
      mode?: string;
      config?: Record<string, unknown>;
      filter?: NotificationFilter;
      rate_per_min?: number;
    },
  ) => patch<{ id: number }>(`/notify/channels/${id}`, payload),
  notifyDeleteChannel: (id: number) => del<{ ok: boolean }>(`/notify/channels/${id}`),
  // 同步发一条测试消息；失败时后端会把渠道的原始错误回传，供排查配置。
  notifyTestChannel: (id: number) => post<{ ok: boolean; latency_ms: number }>(`/notify/channels/${id}/test`),
  notifyDeliveries: (q: { channelId?: number; state?: string; page?: number; pageSize?: number } = {}) => {
    const p = new URLSearchParams();
    if (q.channelId) p.set("channel_id", String(q.channelId));
    if (q.state) p.set("state", q.state);
    p.set("page", String(q.page ?? 1));
    p.set("page_size", String(q.pageSize ?? 50));
    return get<{ deliveries: NotificationDelivery[]; total: number; page: number; page_size: number }>(
      `/notify/deliveries?${p.toString()}`,
    ).then((r) => ({ ...r, deliveries: arr(r.deliveries) }));
  },
  notifyRetryDelivery: (id: number) => post<{ ok: boolean }>(`/notify/deliveries/${id}/retry`),
  report: async (task?: string) => {
    if (MOCK) return mockReport(task);
    const token = getToken();
    const r = await fetch(`/api/report${tq(task)}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(`report: ${r.status}`);
    return r.text();
  },
  chatMentions: (kind: string, query: string, signal?: AbortSignal, cursor = "") =>
    http<{ items: ChatMention[]; next_cursor?: string }>(
      `/chat/mentions?${new URLSearchParams({ kind, q: query, cursor })}`,
      { signal },
    ),
  chat: (message: string, task?: string, attachments?: ChatAttachment[], seg?: number) =>
    post<{ reply: string; mode: string }>(`/chat${tq(task)}`, { message, attachments, seg }),
  chatStatus: (taskId: string) => get<{ running: boolean }>(`/tasks/${taskId}/chat/status`),
  // 方式1 文件上传:落到会话/任务工作目录 uploads/，返回可供 agent Read 的相对路径。
  chatUpload: async (scope: "task" | "session" | "staging", id: string, files: File[]) => {
    if (MOCK)
      return {
        attachments: files.map((f) => ({
          name: f.name,
          path: `uploads/${f.name}`,
          size: f.size,
          abs: `/mock/${f.name}`,
        })),
      };
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    const token = getToken();
    const r = await fetch(`/api/chat/upload?scope=${scope}&id=${encodeURIComponent(id)}`, {
      method: "POST",
      headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: fd,
    });
    if (!r.ok) throw new Error(`上传失败: ${r.status} ${await r.text()}`);
    return r.json() as Promise<{ attachments: ChatAttachment[] }>;
  },
  stopChat: (taskId: string) => post<{ status: string }>(`/tasks/${taskId}/chat/stop`, {}),
  gc: (ttl = 86400) => post<{ removed: number }>(`/gc?ttl=${ttl}`, {}),

  // ---- LLM ----
  getLLM: () =>
    get<{
      configured: boolean;
      provider: string;
      model: string;
      base_url: string;
      proxy?: string;
      key_set: boolean;
      rate_per_second?: number;
      rate_per_minute?: number;
      context_window_k?: number;
      thinking_type?: string;
      reasoning_effort?: string;
    }>("/llm"),
  setLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    rate_per_second = 0,
    rate_per_minute = 0,
    proxy = "",
    context_window_k = 0,
  ) => post("/llm", { provider, model, base_url, proxy, api_key, rate_per_second, rate_per_minute, context_window_k }),
  testLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    proxy = "",
    thinking_type = "",
    reasoning_effort = "",
    profile_id?: number,
    streaming = true, // 用该配置真实的收发模式来测，别让"流式能通、非流式不通"漏到会话里
    session_header_key = "", // 非空=测试请求也带该自定义会话头（值为一次性 session id）
  ) =>
    // reply = 模型实际回复(已截断);一个字都不回的配置后端直接判失败
    post<{ ok: boolean; error?: string; latency_ms?: number; model?: string; reply?: string }>("/llm/test", {
      provider,
      model,
      base_url,
      proxy,
      api_key,
      thinking_type,
      reasoning_effort,
      profile_id,
      streaming,
      session_header_key,
    }),
  llmProfiles: () => get<{ profiles: LLMProfile[] }>("/llm/profiles").then((r) => arr(r.profiles)),
  saveLLMProfile: (p: {
    id?: number; // omit/0 = create; set = update that profile
    name: string;
    format: string;
    model: string;
    base_url?: string;
    proxy?: string;
    api_key?: string; // blank on update keeps the existing key
    rate_per_second?: number;
    rate_per_minute?: number;
    context_window_k?: number;
    thinking_type?: string; // ""(不发送)|"disabled"|"enabled"
    reasoning_effort?: string; // ""(不发送)|"low"|"medium"|"high"|"xhigh"|"max"
    priority?: number; // 轮询顺位，越大越先用
    pool_exclude?: boolean; // true=不作为故障转移目标
    streaming?: boolean; // true(默认)=流式 | false=非流式
    max_tokens?: number; // 单次回复输出上限；0=不发送，由服务端默认值决定
    max_tokens_field?: string; // ""=max_tokens(默认) | "max_completion_tokens"（仅 openai 格式）
    session_header_key?: string; // 非空=每次请求带该 HTTP 头，头值=当前会话 session id；""=不发送
    retry?: LLMRetryOverride; // 本配置的重试覆盖；各项留 0 = 跟随全局重试策略
  }) => post<{ id: number }>("/llm/profiles", p),
  deleteLLMProfile: (id: string) => del<{ deleted: number }>(`/llm/profiles/${id}`),
  activateLLMProfile: (id: string) => post<{ ok: boolean }>("/llm/profiles/active", { id: Number(id) }),
  // 轮询链的实际顺序 + 各配置的熔断状态。
  llmPool: () => get<LLMPoolStatus>("/llm/pool"),
  // 清除熔断，让下一次调用立刻重试该配置；不传 id = 全部清除。
  resetLLMPool: (id?: string) => post<LLMPoolStatus>("/llm/pool/reset", { id: id ? Number(id) : 0 }),
  // 全局重试策略（五层各自的次数+间隔）。全 0 = 全部走内置默认。
  llmRetryPolicy: () => get<LLMRetryPolicy>("/llm/retry-policy"),
  saveLLMRetryPolicy: (p: LLMRetryPolicy) => post<LLMRetryPolicy>("/llm/retry-policy", p),
  fetchLLMModels: (provider: string, base_url: string, api_key: string, proxy = "", profile_id?: number) =>
    post<{ ok: boolean; error?: string; models?: string[] }>("/llm/models", {
      provider,
      base_url,
      api_key,
      proxy,
      profile_id,
    }),

  // ---- agents ----
  agents: () => get<{ agents: Agent[] }>("/agents").then((r) => arr(r.agents)),
  getAgent: (key: string) => get<AgentDetail>(`/agents/${key}`),
  createAgent: (key: string, name: string, description = "") => post<Agent>("/agents", { key, name, description }),
  updateAgent: (key: string, name: string, description = "") =>
    patch<{ ok: boolean }>(`/agents/${key}`, { name, description }),
  deleteAgent: (key: string) => del<{ deleted: string }>(`/agents/${key}`),

  // ---- conversations (chat page) ----
  conversations: () => get<{ conversations: Conversation[] }>("/conversations").then((r) => arr(r.conversations)),
  createConversation: (agent_key: string, title = "", llm_profile_id?: number | null) =>
    post<Conversation>("/conversations", { agent_key, title, llm_profile_id: llm_profile_id ?? null }),
  updateConversation: (id: number, input: { title?: string; pinned?: boolean }) =>
    patch<Conversation>(`/conversations/${id}`, input),
  renameConversation: (id: number, title: string) => patch<Conversation>(`/conversations/${id}`, { title }),
  pinConversation: (id: number, pinned: boolean) => patch<Conversation>(`/conversations/${id}`, { pinned }),
  updateConversationProfile: (id: number, llm_profile_id: number | null) =>
    patch<{ ok: boolean }>(`/conversations/${id}/profile`, { llm_profile_id }),
  deleteConversation: (id: number) => del<{ deleted: number }>(`/conversations/${id}`),
  deleteConversations: (ids: number[]) =>
    post<{ items: { id: number; ok: boolean; error?: string }[] }>("/conversations/delete/batch", { ids }),
  // Incremental tail: steps after `since` (id ASC) — live poll + post-send fetch.
  conversationMessages: (id: number, since = 0) =>
    get<{ items: Activity[]; cursor: number; running: boolean }>(`/conversations/${id}/messages?since=${since}`).then(
      (r) => ({ items: arr(r.items), cursor: r.cursor ?? 0, running: !!r.running }),
    ),
  // Reverse pagination: the latest `limit` steps (before=0) or the page of older
  // steps ending before id `before`. hasMore = still-older steps exist.
  conversationHistory: (id: number, before = 0, limit = 200) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (before > 0) q.set("before", String(before));
    return get<{ items: Activity[]; cursor: number; running: boolean; hasMore: boolean }>(
      `/conversations/${id}/messages?${q.toString()}`,
    ).then((r) => ({ items: arr(r.items), cursor: r.cursor ?? 0, running: !!r.running, hasMore: !!r.hasMore }));
  },
  conversationMsgDetail: (id: number, seq: number) =>
    get<{ detail: string }>(`/conversations/${id}/messages/${seq}`).then((r) => r.detail ?? ""),
  sendConversationMessage: (id: number, message: string, attachments?: ChatAttachment[]) =>
    post<{ status: string }>(`/conversations/${id}/messages`, { message, attachments }),
  stopConversation: (id: number) => post<{ status: string }>(`/conversations/${id}/stop`, {}),
  saveAgentPrompt: (key: string, template: string, note = "") =>
    put<{ version: number }>(`/agents/${key}/prompt`, { template, note }),
  resetAgentPrompt: (key: string) => post<{ version: number }>(`/agents/${key}/prompt/reset`, {}),
  // 收尾提示词(超时/步数耗尽的 settlement 提示);prompt 空串=清除覆盖、用内置默认;
  // max_turns 省略则不动、传 0=用内置默认轮数
  saveAgentWrapup: (key: string, prompt: string, maxTurns?: number) =>
    put<{ ok: boolean }>(`/agents/${key}/wrapup`, { prompt, max_turns: maxTurns }),
  resetAgentWrapup: (key: string) =>
    post<{ ok: boolean; wrapup_default: string; wrapup_max_turns_default: number }>(`/agents/${key}/wrapup/reset`, {}),
  // 任务级超时收尾词(仅 worker/planner);prompt 空=清除、用内置默认
  saveAgentTaskTimeoutWrapup: (key: string, prompt: string, maxTurns?: number) =>
    put<{ ok: boolean }>(`/agents/${key}/wrapup/task-timeout`, { prompt, max_turns: maxTurns }),
  resetAgentTaskTimeoutWrapup: (key: string) =>
    post<{ ok: boolean; task_timeout_wrapup_default: string; task_timeout_wrapup_max_turns_default: number }>(
      `/agents/${key}/wrapup/task-timeout/reset`,
      {},
    ),
  // P3 triggers (仅自定义 agent)
  agentTriggers: (key: string) =>
    get<{ triggers: AgentTrigger[] }>(`/agents/${key}/triggers`).then((r) => arr(r.triggers)),
  createTrigger: (key: string, t: Omit<AgentTrigger, "id" | "agent_key" | "last_fire">) =>
    post<AgentTrigger>(`/agents/${key}/triggers`, t),
  updateTrigger: (id: number, t: Omit<AgentTrigger, "id" | "agent_key" | "last_fire">) =>
    patch<{ ok: boolean }>(`/triggers/${id}`, t),
  deleteTrigger: (id: number) => del<{ deleted: number }>(`/triggers/${id}`),
  saveAgentConfig: (
    key: string,
    patch: {
      llm_profile_id?: number | null; // number=绑定；null=解绑(跟随任务/全局)；缺省=不动
      max_turns?: number;
      run_seconds?: number;
      web_search?: boolean;
      interactive_shell?: boolean;
      trigger_run_mode?: "serial" | "parallel";
      trigger_merge_mode?: "by_task" | "all" | "none";
      trigger_max_parallel?: number;
    },
  ) => put<{ ok: boolean }>(`/agents/${key}/config`, patch),
  agentPromptVersions: (key: string) =>
    get<{ versions: PromptVersion[] }>(`/agents/${key}/prompts`).then((r) => arr(r.versions)),
  agentVariables: (key: string) =>
    get<{ variables: PromptVar[] }>(`/agents/${key}/variables`).then((r) => arr(r.variables)),
  previewAgentPrompt: (key: string, template: string, sample?: Record<string, string>) =>
    post<{ rendered: string; error?: string }>(`/agents/${key}/prompt/preview`, { template, sample }),
  getAgentVisibility: (key: string) => get<{ mcp: number[]; skill: string[] }>(`/agents/${key}/visibility`),
  setAgentVisibility: (key: string, mcp: number[], skill: string[]) =>
    put<{ ok: boolean }>(`/agents/${key}/visibility`, { mcp, skill }),

  // ---- tools (内置工具目录) ----
  tools: () => get<{ tools: Tool[] }>("/tools").then((r) => arr(r.tools)),
  saveTool: (key: string, patch: Pick<Tool, "description" | "schema" | "agents" | "enabled">) =>
    put<{ ok: boolean }>(`/tools/${key}`, patch),
  resetTool: (key: string) => post<{ ok: boolean }>(`/tools/${key}/reset`, {}),
  // custom tools (自定义工具)
  createCustomTool: (
    t: Pick<Tool, "key" | "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => post<{ key: string }>("/tools/custom", t),
  updateCustomTool: (
    key: string,
    t: Pick<Tool, "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => put<{ ok: boolean }>(`/tools/custom/${key}`, t),
  deleteCustomTool: (key: string) => del<{ deleted: string }>(`/tools/custom/${key}`),
  testCustomTool: (body: { kind: string; exec: Record<string, unknown>; params: Record<string, unknown> }) =>
    post<{ output: string; is_error: boolean }>("/tools/custom/test", body),
  detectPython: () => post<{ python_interpreter: string }>("/settings/python/detect", {}),

  // ---- mcp ----
  mcpServers: () => get<{ servers: MCPServer[] }>("/mcp").then((r) => arr(r.servers)),
  saveMcpServer: (m: Partial<MCPServer>) => post<{ id: number }>("/mcp", m),
  deleteMcpServer: (id: number) => del<{ deleted: number }>(`/mcp/${id}`),
  mcpTools: (id: number) => get<{ tools: MCPTool[] }>(`/mcp/${id}/tools`).then((r) => arr(r.tools)),
  refreshMcpServer: (id: number) => post<{ tools: MCPTool[] }>(`/mcp/${id}/refresh`, {}).then((r) => arr(r.tools)),

  // ---- 资产同步 (ScopeSentry 数据源) ----
  ssStatus: () =>
    get<{ exists: boolean; configured: boolean; enabled: boolean; reachable: boolean; url?: string; tools: string[] }>(
      "/sync/scopesentry/status",
    ),
  ssDatasource: (body: { url?: string; api_key?: string }) =>
    post<{ id: number; enabled: boolean }>("/sync/scopesentry/datasource", body),
  ssProjects: (page = 1, size = 50, search = "") =>
    get<{ projects: SSProject[]; tag: Record<string, number> }>(
      `/sync/scopesentry/projects?page=${page}&size=${size}${search ? `&search=${encodeURIComponent(search)}` : ""}`,
    ).then((r) => ({ projects: arr(r.projects), tag: r.tag ?? {} })),
  ssTasks: (page = 1, size = 50, search = "") =>
    get<{ tasks: SSTask[] }>(
      `/sync/scopesentry/tasks?page=${page}&size=${size}${search ? `&search=${encodeURIComponent(search)}` : ""}`,
    ).then((r) => arr(r.tasks)),
  ssSync: (body: {
    dimension: "project" | "task";
    targets: string[];
    asset_types: string[];
    create_company?: boolean;
    page_size?: number;
  }) =>
    post<{
      synced: Record<string, number>;
      companies: string[] | null;
      warnings: string[] | null;
      errors: string[] | null;
    }>("/sync/scopesentry/sync", body),

  // ---- skills (文件系统) ----
  skills: () => get<{ skills: SkillItem[] }>("/skills").then((r) => arr(r.skills)),
  createSkill: (s: {
    name: string;
    description: string;
    license?: string;
    compatibility?: string;
    mcps?: string[];
    instructions?: string;
  }) => post<{ name: string }>("/skills", s),
  // uploadSkill installs a skill from a .zip (multipart). Surfaces the backend
  // error text (e.g. 已存在 / 缺少 SKILL.md) so the UI can show a precise message.
  uploadSkill: async (file: File, overwrite = false): Promise<{ name: string; files: number }> => {
    if (MOCK) return { name: file.name.replace(/\.zip$/i, ""), files: 1 };
    const fd = new FormData();
    fd.append("file", file);
    const token = getToken();
    const r = await fetch(`/api/skills/upload${overwrite ? "?overwrite=true" : ""}`, {
      method: "POST",
      body: fd,
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body?.error || `上传失败(${r.status})`);
    return body;
  },
  deleteSkill: (name: string) => del<{ deleted: string }>(`/skills/${name}`),
  updateSkillMeta: (
    name: string,
    meta: { mcps?: string[]; description?: string; license?: string; compatibility?: string },
  ) => put<{ ok: boolean }>(`/skills/${name}/meta`, meta),
  createSkillDir: (skill: string, path: string) => post<{ dir: string }>(`/skills/${skill}/dirs`, { path }),
  skillFiles: (name: string) => get<{ files: string[] }>(`/skills/${name}/files`).then((r) => r.files),
  readSkillFile: (name: string, file: string) =>
    get<{ content: string; file: string }>(`/skills/${name}/files/${file}`).then((r) => r.content),
  writeSkillFile: (name: string, file: string, content: string) =>
    put<{ ok: boolean }>(`/skills/${name}/files/${file}`, { content }),
  deleteSkillPath: (skill: string, path: string) => del<{ deleted: string }>(`/skills/${skill}/files/${path}`),
  skillUsage: (name: string, limit = 50) =>
    get<{ calls: SkillCall[] }>(`/skills/${name}/usage?limit=${limit}`).then((r) => arr(r.calls)),
  missingSkills: (limit = 20) =>
    get<{ missing: MissingSkill[] }>(`/skills/missing?limit=${limit}`).then((r) => arr(r.missing)),

  // ---- visibility (MCP resource side) ---- (agent ids are strings per spec)
  resourceVisibility: (kind: string, id: number) =>
    get<{ agents: string[] }>(`/visibility/${kind}/${id}`).then((r) => arr(r.agents)),
  toggleVisibility: (agentId: string, kind: string, resourceId: number, visible: boolean) =>
    post<{ ok: boolean }>("/visibility/toggle", { agent_id: agentId, kind, resource_id: resourceId, visible }),

  // ---- visibility (Skill，按名称) ----
  skillVisibility: (name: string) => get<{ agents: string[] }>(`/visibility/skill/${name}`).then((r) => arr(r.agents)),
  toggleSkillVisibility: (agentId: string, skillName: string, visible: boolean) =>
    post<{ ok: boolean }>("/visibility/skill/toggle", { agent_id: agentId, skill_name: skillName, visible }),

  // ---- intercept rules ----
  interceptRules: () => get<{ rules: InterceptRule[] }>("/intercept/rules").then((r) => arr(r.rules)),
  createInterceptRule: (rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    post<InterceptRule>("/intercept/rules", rule),
  updateInterceptRule: (id: number, rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    put<InterceptRule>(`/intercept/rules/${id}`, rule),
  deleteInterceptRule: (id: number) => del<{ deleted: number }>(`/intercept/rules/${id}`),
  toggleInterceptRule: (id: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/intercept/rules/${id}/toggle`, { enabled }),

  // ---- asset intercept rules（资产拦截：全局黑名单） ----
  assetInterceptRules: () => get<{ rules: AssetInterceptRule[] }>("/asset-intercept/rules").then((r) => arr(r.rules)),
  createAssetInterceptRule: (rule: Pick<AssetInterceptRule, "enabled" | "kind" | "pattern" | "note">) =>
    post<AssetInterceptRule>("/asset-intercept/rules", rule),
  updateAssetInterceptRule: (id: number, rule: Pick<AssetInterceptRule, "enabled" | "kind" | "pattern" | "note">) =>
    put<AssetInterceptRule>(`/asset-intercept/rules/${id}`, rule),
  deleteAssetInterceptRule: (id: number) => del<{ deleted: number }>(`/asset-intercept/rules/${id}`),
  toggleAssetInterceptRule: (id: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/asset-intercept/rules/${id}/toggle`, { enabled }),

  // ---- intercept pending (ask) ----
  interceptPending: () => get<{ pending: InterceptPending[] }>("/intercept/pending").then((r) => arr(r.pending)),
  interceptGetOne: (id: number) => get<InterceptPending>(`/intercept/pending/${id}`),
  interceptDecide: (id: number, decision: "allowed" | "denied") =>
    post<{ ok: boolean }>(`/intercept/pending/${id}/decide`, { decision }),
  interceptExecution: (id: number, conversationId?: number) =>
    get<import("@/lib/types").InterceptExecution>(
      `/intercept/history/${id}/execution${conversationId ? `?conversation=${conversationId}` : ""}`,
    ),
  interceptDetail: (id: number) => get<InterceptDetail>(`/intercept/history/${id}`),
  interceptHistory: () => get<{ items: InterceptApprovalRow[] }>("/intercept/history").then((r) => arr(r.items)),
  interceptHistoryPage: (page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/history?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),
  interceptTask: (taskId: string) =>
    get<{ items: InterceptApprovalRow[] }>(`/intercept/task/${taskId}`).then((r) => arr(r.items)),
  interceptTaskPage: (taskId: string, page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/task/${encodeURIComponent(taskId)}?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),

  // ---- intercept master switch (拦截总开关) ----
  // When off, tool calls skip rule matching, the model judge and human approval.
  interceptGetConfig: async (): Promise<{ enabled: boolean }> => {
    if (MOCK) return { enabled: true };
    const token = getToken();
    const r = await fetch("/api/intercept/config", {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },
  interceptSetConfig: async (enabled: boolean): Promise<{ enabled: boolean }> => {
    if (MOCK) return { enabled };
    const token = getToken();
    const r = await fetch("/api/intercept/config", {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ enabled }),
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },

  // ---- intercept tool-config (全局工具拦截范围) ----
  interceptGetToolConfig: async (): Promise<{ enabled_tools: string[] }> => {
    if (MOCK) return { enabled_tools: ["bash"] };
    const token = getToken();
    const r = await fetch("/api/intercept/tool-config", {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },
  interceptSetToolConfig: async (enabledTools: string[]): Promise<void> => {
    if (MOCK) return;
    const token = getToken();
    const r = await fetch("/api/intercept/tool-config", {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ enabled_tools: enabledTools }),
    });
    if (!r.ok) throw new Error(await r.text());
  },

  // ---- intercept LLM judge (模型兜底审批,全局配置) ----
  interceptGetJudgeConfig: () => get<JudgeConfig>("/intercept/judge"),
  interceptSetJudgeConfig: (cfg: JudgeConfig) => put<{ ok: boolean }>("/intercept/judge", cfg),
  interceptJudgeUsage: (days = 30) => get<JudgeUsage>(`/intercept/judge/usage?days=${days}`),

  // ---- commands (tool execution history, any tool) ----
  commands: (params?: { task?: string; q?: string; page?: number; size?: number }) => {
    const sp = new URLSearchParams();
    if (params?.task) sp.set("task", params.task);
    if (params?.q) sp.set("q", params.q);
    sp.set("page", String(params?.page ?? 0));
    sp.set("size", String(params?.size ?? 50));
    return get<{ commands: CommandRecord[]; total: number }>(`/commands?${sp}`);
  },
  // 各工具调用次数；沿用列表的 task/q 筛选，统计的是整个结果集而非当前页。
  commandStats: (params?: { task?: string; q?: string }) => {
    const sp = new URLSearchParams();
    if (params?.task) sp.set("task", params.task);
    if (params?.q) sp.set("q", params.q);
    return get<{ stats: ToolStat[] }>(`/commands/stats?${sp}`);
  },

  // ---- LLM records ----
  llmRecords: (params?: { model?: string; session?: string; task?: string; page?: number; size?: number }) => {
    const sp = new URLSearchParams();
    if (params?.model) sp.set("model", params.model);
    if (params?.session) sp.set("session", params.session);
    if (params?.task) sp.set("task", params.task);
    if (params?.page !== undefined) sp.set("page", String(params.page));
    sp.set("size", String(params?.size ?? 50));
    return get<{ records: LLMRecordItem[]; total: number }>(`/llm/records?${sp}`);
  },
  llmRecordDetail: (id: number) => get<LLMRecordDetail>(`/llm/records/${id}`),
  llmTasks: () => get<{ tasks: LLMTask[] }>(`/llm/records/tasks`),
  llmRecordsDeleteTask: (task: string) => del<{ deleted: number }>(`/llm/records?task=${encodeURIComponent(task)}`),
  // 按模型聚合本任务的 token 用量（来自常开的 llm_usage 计量账本，逐次调用精确，
  // per-agent 绑定 / 轮询 / 中断消耗都覆盖）。
  tokensByModel: (task: string) =>
    get<{ models: ModelTokenStat[] }>(`/llm/records/by-model?task=${encodeURIComponent(task)}`),

  // ---- 一键更新 ----
  // 检查以后端为准：下载是后端做的，浏览器能连 GitHub 而服务器连不上的情况很常见
  // （服务器在内网、代理只配在浏览器上），那时点更新必然失败。
  // 后端对 GitHub 的查询结果有 30 分钟缓存（未认证的 GitHub API 是 60 次/小时/IP，
  // 顶栏每次整页加载都会查一次，不缓存会很快耗光配额）。force=true 强制回源，
  // 留给用户显式点「检查更新」时用。
  checkUpdate: (force = false) => get<UpdateCheck>(`/update/check${force ? "?force=1" : ""}`),
  // 202 即返回，实际下载在后台跑，进度走 /api/update/stream。
  applyUpdate: () => post<{ ok: boolean; target: string }>(`/update/apply`),
  rollbackUpdate: () => post<{ ok: boolean }>(`/update/rollback`),
};
