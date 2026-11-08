"use client";

import * as React from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import {
  ArchiveIcon,
  ArrowLeftIcon,
  BrainIcon,
  Building2Icon,
  CheckIcon,
  CircleAlertIcon,
  GitBranchIcon,
  PauseIcon,
  PlayIcon,
  RocketIcon,
} from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Popover,
  PopoverContent,
  PopoverDescription,
  PopoverHeader,
  PopoverTitle,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";
import { useAuth } from "@/lib/auth-context";
import type { Department, LLMProfile, Task } from "@/lib/types";

import { AssetsTab } from "./_tabs/assets-tab";
import { BroadcastTab } from "./_tabs/broadcast-tab";
import { CoverageGraphTab } from "./_tabs/coverage-graph-tab";
import { DepsTab } from "./_tabs/deps-tab";
import { FindingsTab } from "./_tabs/findings-tab";
import { GraphTab } from "./_tabs/graph-tab";
import { InterceptTab } from "./_tabs/intercept-tab";
import { OverviewTab } from "./_tabs/overview-tab";
import { READ_ONLY_TITLE, writeTitle } from "./_tabs/read-only";
import { ReportTab } from "./_tabs/report-tab";
import { RetestsTab } from "./_tabs/retests-tab";
import { SessionsTab } from "./_tabs/sessions-tab";

// 归属部门下拉里「不指定」的哨兵值：Radix Select 的 value 只能是字符串，
// 而 api.setTaskDepartment(id, null) 才是移出部门的语义。
const NO_DEPARTMENT = "__none__";

const TABS = [
  { value: "sessions", label: "会话" },
  { value: "overview", label: "总览" },
  { value: "graph", label: "探索链路" },
  { value: "broadcast", label: "播报板" },
  { value: "findings", label: "发现" },
  { value: "retests", label: "复核" },
  { value: "assets", label: "测试资产" },
  { value: "deps", label: "依赖" },
  { value: "coverage", label: "资产覆盖图" },
  { value: "intercept", label: "拦截审批" },
  { value: "report", label: "报告" },
];

function taskProfileIDs(task: Task): string[] {
  if (task.llm_profile_ids && task.llm_profile_ids.length > 0) {
    return task.llm_profile_ids.map(String);
  }
  return task.llm_profile_id ? [String(task.llm_profile_id)] : [];
}

function TaskLLMControl({
  task,
  profiles,
  canWrite,
  onUpdated,
}: {
  task: Task;
  profiles: LLMProfile[];
  // 部门访客（can_write=false）只能查看配置链，不能改：编辑区与保存都禁用。
  canWrite: boolean;
  onUpdated: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [profileIDs, setProfileIDs] = React.useState<string[]>(() => taskProfileIDs(task));
  const [activeProfileID, setActiveProfileID] = React.useState(
    task.active_llm_profile_id ? String(task.active_llm_profile_id) : (taskProfileIDs(task)[0] ?? ""),
  );
  const [saving, setSaving] = React.useState(false);
  const popoverContentRef = React.useRef<HTMLDivElement>(null);

  const chain = taskProfileIDs(task);
  const exhausted = task.llm_failover_state === "chain_exhausted";
  // 任何状态都可以改链,终态也不例外:任务结束后主 Agent 对话仍走这条链,
  // 链上模型出问题时必须能换掉,否则已完成任务就没法继续交互。
  const terminal = ["done", "failed", "timeout"].includes(task.status);
  // A null active profile on an exhausted, non-empty chain is a persisted end
  // cursor. Keep the status display honest; choosing the first profile is only
  // the editor's reset draft and does not mean it is currently active.
  let activeID = chain[0] ?? "";
  if (exhausted) activeID = "";
  if (task.active_llm_profile_id) activeID = String(task.active_llm_profile_id);
  const activeProfile = profiles.find((profile) => profile.id === activeID);
  const currentLabel = exhausted
    ? "配置链已耗尽"
    : (activeProfile?.name ?? (activeID ? `配置 #${activeID}` : "跟随默认配置"));
  const activeIndex = chain.indexOf(activeID);
  const backupCount = activeIndex >= 0 ? Math.max(0, chain.length - activeIndex - 1) : 0;
  const currentTitle = [currentLabel, activeProfile?.model, backupCount > 0 ? `${backupCount} 个备用` : ""]
    .filter(Boolean)
    .join(" · ");
  let editorDescription = "调整顺序或当前配置后，将从下一次 LLM 调用开始生效。";
  if (terminal) editorDescription = "任务已结束，改动只影响后续的主 Agent 对话。";
  if (!canWrite) editorDescription = "部门访客为只读，无操作权限：只能查看当前配置链。";
  let saveLabel = "保存";
  if (exhausted) saveLabel = "保存并重置";
  if (saving) saveLabel = "保存中";

  const syncDraft = React.useCallback(() => {
    const next = taskProfileIDs(task);
    setProfileIDs(next);
    setActiveProfileID(task.active_llm_profile_id ? String(task.active_llm_profile_id) : (next[0] ?? ""));
  }, [task]);

  const handleOpenChange = (next: boolean) => {
    setOpen(next);
    if (next) syncDraft();
  };

  const handleProfileIDsChange = (next: string[]) => {
    setProfileIDs(next);
    setActiveProfileID((current) => (next.includes(current) ? current : (next[0] ?? "")));
  };

  const save = async () => {
    if (!canWrite) return;
    setSaving(true);
    try {
      const result = await api.updateTaskLLMProfiles(
        task.id,
        profileIDs.map(Number),
        activeProfileID ? Number(activeProfileID) : undefined,
      );
      if (result.switch_event) {
        toast.info(result.switch_event.summary, { id: `task-${task.id}-llm-${result.switch_event.seq}` });
      } else {
        toast.success(
          result.reopened_intents > 0
            ? `LLM 配置已更新，并恢复 ${result.reopened_intents} 条额度阻塞意图`
            : "LLM 配置已更新",
        );
      }
      setOpen(false);
      onUpdated();
    } catch (error) {
      toast.error("更新失败：" + (error as Error).message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger asChild>
        <Button
          size="sm"
          variant={exhausted ? "destructive" : "outline"}
          aria-label="查看或切换任务 LLM 配置"
          title={currentTitle}
        >
          <BrainIcon data-icon="inline-start" />
          <span className="hidden max-w-36 truncate lg:inline">{currentLabel}</span>
          {backupCount > 0 && <span className="hidden text-muted-foreground xl:inline">+{backupCount}</span>}
        </Button>
      </PopoverTrigger>
      <PopoverContent ref={popoverContentRef} align="start" className="w-[min(28rem,calc(100vw-2rem))] gap-4 p-4">
        <PopoverHeader>
          <PopoverTitle>任务 LLM 配置链</PopoverTitle>
          <PopoverDescription>{editorDescription}</PopoverDescription>
        </PopoverHeader>

        {exhausted && (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>配置链额度已耗尽</AlertTitle>
            <AlertDescription>
              {task.llm_failover_reason ?? "所有已选配置均被判定为额度不足。保存配置链可重置故障状态。"}
            </AlertDescription>
          </Alert>
        )}

        <TaskLLMProfileChain
          profiles={profiles}
          value={profileIDs}
          onValueChange={handleProfileIDsChange}
          activeProfileId={activeProfileID}
          onActiveProfileChange={setActiveProfileID}
          inputId="task-llm-profiles"
          disabled={saving || !canWrite}
          portalContainer={popoverContentRef}
        />

        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={() => setOpen(false)}>
            关闭
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={save}
            disabled={saving || !canWrite}
            title={writeTitle(canWrite, "保存任务 LLM 配置链")}
          >
            {saving && <Spinner data-icon="inline-start" />}
            {saveLabel}
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  );
}

function TaskDetailInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  const { user, permissions } = useAuth();
  // AuthProvider 挂在 (main)/layout.tsx。若它还没挂载，useAuth() 只会给出空 user，
  // 这时回退到本地会话缓存判断管理员，否则管理员会看不到「归属部门」的调整入口。
  const isAdmin = user ? !!user.is_admin : !!auth.getCurrentUser()?.isAdmin;
  const [task, setTask] = React.useState<Task | null>(null);
  const [paused, setPaused] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [tab, setTab] = React.useState("sessions");
  const [interceptPendingCount, setInterceptPendingCount] = React.useState(0);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [archiving, setArchiving] = React.useState(false);
  // 手动开启任务：created 状态的任务不会自动开跑，用户点「开启任务」才进入探索。
  const [starting, setStarting] = React.useState(false);
  // 开启失败的原因（409「任务已经在运行中 / 任务已结束 / 任务已暂停」等）内联展示，不弹 toast 砸脸。
  const [startError, setStartError] = React.useState("");
  const [departments, setDepartments] = React.useState<Department[]>([]);
  const [deptSaving, setDeptSaving] = React.useState(false);

  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
  }, []);

  // 只有管理员能改归属，也只有管理员会拿到部门列表（普通账号请求会被后端 403）。
  React.useEffect(() => {
    if (!isAdmin) return;
    let alive = true;
    api
      .departments()
      .then((rows) => {
        if (alive) setDepartments(rows);
      })
      .catch(() => {
        // 部门列表拉取失败时仍展示当前归属，管理员可刷新重试。
      });
    return () => {
      alive = false;
    };
  }, [isAdmin]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(id)
        .then((rows) => {
          if (alive) setInterceptPendingCount(rows.filter((r) => r.status === "pending").length);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [id]);

  const taskLoadInFlight = React.useRef<string | null>(null);
  const load = React.useCallback(() => {
    if (taskLoadInFlight.current === id) return;
    taskLoadInFlight.current = id;
    Promise.all([api.task(id), api.stats(id).catch(() => null)])
      .then(([task, s]) => {
        if (taskLoadInFlight.current !== id) return;
        const base = { ...task };
        const at = s?.active_task;
        if (at) {
          base.in_flight = at.in_flight;
          base.goals_total = at.goals_total;
          base.goals_met = at.goals_met;
          base.engine_mode = s?.engine_mode ?? at.engine_mode;
          base.paused = at.paused;
        }
        setTask(base);
        setPaused(at?.paused ?? base.paused ?? false);
      })
      .catch(() => {
        // Keep the last rendered task state during a transient poll failure.
      })
      .finally(() => {
        if (taskLoadInFlight.current === id) {
          taskLoadInFlight.current = null;
          setLoaded(true);
        }
      });
  }, [id]);
  React.useEffect(() => {
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  }, [load]);

  // 部门访客（后端 task.can_write === false）只读：暂停/继续/归档/改 LLM/改部门/对话等一律禁用。
  // 后端未返回该字段时按可写处理（保持既有行为）；另加权限兜底：纯访客账号对任何任务都只读。
  const canWrite = task?.can_write !== false && !permissions.is_visitor_only;

  async function togglePause() {
    if (!canWrite) return;
    if (!task || task.status === "created") return;
    if (["done", "failed", "timeout"].includes(task.status)) return;
    const next = !paused;
    try {
      await api.controlTask(id, next ? "pause" : "resume");
      setPaused(next);
      toast.success(next ? "已暂停探索" : "已恢复探索");
    } catch (e) {
      toast.error("操作失败：" + (e as Error).message);
    }
  }

  // 手动开启任务：成功后先把返回的任务合并进本地状态（按钮立刻消失，不必等下一次轮询），
  // 再复用页面既有的 load() 刷新任务 + stats（各 Tab 的图/列表由它们自己的轮询跟进）。
  async function startTask() {
    if (!task || starting || !canWrite) return;
    setStarting(true);
    setStartError("");
    try {
      const updated = await api.startTask(task.id);
      setTask((prev) => (prev ? { ...prev, ...updated } : updated));
      toast.success("任务已开启，探索即将开始");
      load();
    } catch (e) {
      // 409（已在运行 / 已结束 / 已暂停）等后端原话直接内联展示，便于用户判断下一步。
      setStartError((e as Error).message || "开启失败，请稍后重试");
    } finally {
      setStarting(false);
    }
  }

  // 管理员调整任务归属部门；null = 不指定（移出部门）。
  async function changeDepartment(value: string) {
    if (!task || deptSaving || !canWrite) return;
    const nextId = value === NO_DEPARTMENT ? null : Number(value);
    if (nextId === (task.department_id ?? null)) return;
    setDeptSaving(true);
    try {
      const updated = await api.setTaskDepartment(task.id, nextId);
      setTask((prev) => (prev ? { ...prev, ...updated } : updated));
      toast.success(nextId === null ? "已改为「不指定」归属部门" : `已归属到「${updated.department_name ?? nextId}」`);
      load();
    } catch (e) {
      toast.error("调整归属部门失败：" + (e as Error).message);
    } finally {
      setDeptSaving(false);
    }
  }

  async function archiveTask() {
    if (!task || archiving || !canWrite) return;
    setArchiving(true);
    try {
      await api.archiveTask(task.id);
      toast.success("任务已加入归档队列");
      router.push("/function/tasks");
    } catch (error) {
      toast.error(`归档失败：${(error as Error).message}`);
      setArchiving(false);
    }
  }

  if (!task) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? `任务 ${id} 已被删除、归档或不存在` : "加载中…"}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/function/tasks">
              <ArrowLeftIcon /> 返回任务列表
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const completed = task.status === "done";
  const terminal = ["done", "failed", "timeout"].includes(task.status);
  // created = 源码已就绪但用户还没点「开启任务」：此时没有可暂停/恢复的东西。
  const notStarted = task.status === "created";
  const archiveLifecycleEligible = terminal || paused || task.status === "paused";
  const canArchive = archiveLifecycleEligible && !task.archive_blocked_by_task_id;
  let archiveDisabledReason = task.queued ? "排队中的任务必须先暂停" : "运行中的任务必须先暂停";
  if (archiveLifecycleEligible && task.archive_blocked_by_task_id) {
    archiveDisabledReason = `任务被未归档任务 #${task.archive_blocked_by_task_id} 直接继承，请先归档依赖任务`;
  }
  // 只读原因优先级最高：访客连「暂停」按钮为什么点不动都该一眼看懂。
  if (!canWrite) archiveDisabledReason = READ_ONLY_TITLE;
  const engineMode = paused ? "paused" : (task.engine_mode ?? "idle");
  let controlVariant: "default" | "secondary" | "outline" = "outline";
  let controlIcon = <PauseIcon data-icon="inline-start" />;
  let controlLabel = "暂停";
  if (terminal) {
    controlVariant = "secondary";
    controlIcon = <CheckIcon data-icon="inline-start" />;
    controlLabel = completed ? "已完成" : "已结束";
  } else if (paused) {
    controlVariant = "default";
    controlIcon = <PlayIcon data-icon="inline-start" />;
    controlLabel = "恢复";
  }
  let controlTitle = writeTitle(canWrite, paused ? "恢复任务探索" : "暂停任务探索");
  if (notStarted) controlTitle = writeTitle(canWrite, "任务尚未开启，先点「开启任务」");
  const archiveTrigger = (
    <Button
      size="icon-sm"
      variant="ghost"
      disabled={!canArchive || archiving || !canWrite}
      aria-label={canArchive ? "归档任务" : archiveDisabledReason}
    >
      {archiving ? <Spinner /> : <ArchiveIcon />}
    </Button>
  );
  const departmentValue = task.department_id != null ? String(task.department_id) : NO_DEPARTMENT;

  return (
    <Tabs value={tab} onValueChange={setTab} className="flex flex-1 flex-col gap-0">
      {/* Top fixed area */}
      <header className="sticky top-0 z-10 flex flex-col gap-2 border-b bg-background/95 px-4 py-2.5 backdrop-blur lg:px-6">
        <div className="flex items-center gap-2">
          <SidebarTrigger className="-ml-1" />
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/function/tasks">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <h1 className="min-w-0 flex-1 truncate text-sm font-semibold" title={task.name || task.description}>
            {task.name || task.description}
          </h1>
          <code className="hidden rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground sm:inline">
            {task.id}
          </code>
          <Separator orientation="vertical" className="mx-1 hidden h-4 sm:block" />
          {/* 手动开启任务：仅 created（且可写）时出现；开启后状态变化，按钮自然消失。 */}
          {notStarted && canWrite && (
            <Button size="sm" onClick={() => void startTask()} disabled={starting} title="开始执行本任务的审计流程">
              {starting ? <Spinner data-icon="inline-start" /> : <RocketIcon data-icon="inline-start" />}
              开启任务
            </Button>
          )}
          <TaskLLMControl task={task} profiles={profiles} canWrite={canWrite} onUpdated={load} />
          <StatusBadge domain={terminal ? "task" : "engine"} value={terminal ? task.status : engineMode} dot />
          {canArchive && canWrite ? (
            <AlertDialog>
              <AlertDialogTrigger asChild>{archiveTrigger}</AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>归档任务 #{task.id}？</AlertDialogTitle>
                  <AlertDialogDescription>
                    任务图谱、关联记录、独占资产与流量、工作文件和 LLM
                    历史将压缩到冷存储。归档完成后可在任务列表的“已归档”页还原。
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>取消</AlertDialogCancel>
                  <AlertDialogAction onClick={() => void archiveTask()}>确认归档</AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          ) : (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="inline-flex">{archiveTrigger}</span>
              </TooltipTrigger>
              <TooltipContent>{archiveDisabledReason}</TooltipContent>
            </Tooltip>
          )}
          <Button
            size="sm"
            variant={controlVariant}
            onClick={() => void togglePause()}
            disabled={terminal || notStarted || !canWrite}
            title={controlTitle}
          >
            {controlIcon}
            {controlLabel}
          </Button>
        </div>
        {/* 归属部门：有值时贴在目标行右侧；管理员可在此直接改归属（访客只读时禁用）。 */}
        <div className="flex min-w-0 items-center gap-2">
          <p className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{task.goal}</p>
          {isAdmin ? (
            <Select
              value={departmentValue}
              onValueChange={(value) => void changeDepartment(value)}
              disabled={deptSaving || !canWrite}
            >
              <SelectTrigger
                size="sm"
                className="h-6 max-w-48 shrink-0 gap-1 border-dashed px-1.5 text-xs"
                aria-label="任务归属部门"
                title={writeTitle(canWrite, "调整任务归属部门（管理员）")}
              >
                <Building2Icon className="size-3 text-muted-foreground" />
                <SelectValue placeholder="不指定部门" />
              </SelectTrigger>
              <SelectContent position="popper" align="end">
                <SelectGroup>
                  <SelectItem value={NO_DEPARTMENT}>不指定</SelectItem>
                  {departments.map((dept) => (
                    <SelectItem key={dept.id} value={String(dept.id)}>
                      {dept.name}
                    </SelectItem>
                  ))}
                  {/* 部门列表拉取失败/未加载完时，仍要让当前归属有对应项，
                      否则触发器会误显示成「不指定」。 */}
                  {task.department_id != null && !departments.some((dept) => dept.id === task.department_id) && (
                    <SelectItem value={String(task.department_id)}>
                      {task.department_name || `部门 #${task.department_id}`}
                    </SelectItem>
                  )}
                </SelectGroup>
              </SelectContent>
            </Select>
          ) : task.department_name ? (
            <span
              className="inline-flex shrink-0 items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground"
              title={`归属部门：${task.department_name}`}
            >
              <Building2Icon className="size-3" />
              {task.department_name}
            </span>
          ) : null}
          {/* 源码来源：放在标题下的元信息行，任务跑起来之后也一直可见。
              之前只写在「未开启任务」引导卡里，一开启就消失了——而那正是用户想回溯
              「这次审的是哪个 commit」的时刻。 */}
          {task.source_origin === "github" && task.source_commit ? (
            <a
              href={task.source_url}
              target="_blank"
              rel="noreferrer"
              className="bg-muted text-muted-foreground hover:text-foreground inline-flex max-w-80 shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-xs"
              title={`源码来源：${task.source_url} @ ${task.source_commit}`}
            >
              <GitBranchIcon className="size-3 shrink-0" />
              <span className="truncate">{(task.source_url ?? "").replace(/^https?:\/\/github\.com\//, "")}</span>
              <code className="shrink-0 font-mono">{task.source_commit.slice(0, 7)}</code>
            </a>
          ) : null}
        </div>
        {startError && (
          <p role="alert" className="truncate text-xs text-destructive" title={startError}>
            {startError}
          </p>
        )}
        {/* Tabs */}
        <div className="no-scrollbar min-w-0 overflow-x-auto">
          <TabsList variant="default" className="min-w-max">
            {TABS.map((t) => (
              <TabsTrigger key={t.value} value={t.value}>
                {t.label}
                {t.value === "intercept" && interceptPendingCount > 0 && (
                  <span className="ml-1.5 inline-flex h-4 min-w-[16px] items-center justify-center rounded-full bg-amber-500 px-1 text-[10px] font-semibold leading-none text-white">
                    {interceptPendingCount > 99 ? "99+" : interceptPendingCount}
                  </span>
                )}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>
      </header>

      {/* Tab content */}
      <div className="flex-1 p-4 lg:p-6">
        {/* 未开启任务的引导：只在默认 Tab（会话）出现，用既有 Card 样式，避免新造视觉。
            有源码目录就报路径；没有则提示需要源码包，因为探索目标要靠它落地。 */}
        {notStarted && tab === "sessions" && (
          <Card className="mb-4 gap-2 border-dashed py-3">
            <CardContent className="flex items-start gap-3">
              <div className="bg-primary/10 flex size-8 shrink-0 items-center justify-center rounded-full">
                <RocketIcon className="text-primary size-4" />
              </div>
              <div className="flex min-w-0 flex-col gap-1">
                <p className="text-sm font-medium">
                  {!canWrite
                    ? "任务尚未开启"
                    : task.source_path
                      ? "源码已就绪，点击右上角「开启任务」开始审计"
                      : "任务尚未开启，点击右上角「开启任务」开始审计"}
                </p>
                {task.source_path ? (
                  <p className="text-xs text-muted-foreground">
                    源码目录：
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono">{task.source_path}</code>
                  </p>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    当前任务没有源码目录：Agent
                    只能依据描述与目标自行探索。如需按源码审计，请到任务列表删除本任务后重新创建并上传源码包。
                  </p>
                )}
                <p className="text-xs text-muted-foreground">
                  {canWrite
                    ? "开启前探索链路与目标是空的；开启后主 Agent 会把目标拆成意图并派发给 Worker。"
                    : "部门访客为只读：需要部门所有者或管理员点击「开启任务」后才能开始审计。"}
                </p>
              </div>
            </CardContent>
          </Card>
        )}
        <TabsContent value="sessions" className="mt-0">
          <SessionsTab taskId={id} canWrite={canWrite} />
        </TabsContent>
        <TabsContent value="overview" className="mt-0">
          <OverviewTab taskId={id} canWrite={canWrite} />
        </TabsContent>
        <TabsContent value="graph" className="mt-0">
          <GraphTab taskId={id} />
        </TabsContent>
        <TabsContent value="broadcast" className="mt-0">
          <BroadcastTab taskId={id} />
        </TabsContent>
        <TabsContent value="findings" className="mt-0">
          <FindingsTab taskId={id} canWrite={canWrite} />
        </TabsContent>
        <TabsContent value="retests" className="mt-0">
          <RetestsTab key={id} taskId={id} />
        </TabsContent>
        <TabsContent value="assets" className="mt-0">
          <AssetsTab taskId={id} canWrite={canWrite} />
        </TabsContent>
        <TabsContent value="deps" className="mt-0">
          <DepsTab taskId={id} canWrite={canWrite} />
        </TabsContent>
        <TabsContent value="coverage" className="mt-0">
          <CoverageGraphTab taskId={id} coverageEnabled={task?.coverage_enabled !== false} />
        </TabsContent>
        <TabsContent value="intercept" className="mt-0">
          <InterceptTab taskId={id} />
        </TabsContent>
        <TabsContent value="report" className="mt-0">
          <ReportTab taskId={id} />
        </TabsContent>
      </div>
    </Tabs>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function TaskDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <TaskDetailInner />
    </React.Suspense>
  );
}
