import {
  BellRing,
  Bot,
  Brain,
  Bug,
  Building2,
  FolderOpen,
  FolderSync,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Radio,
  ScrollText,
  Settings2,
  Sparkles,
  Target,
  Users,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
  /**
   * adminOnly: 仅管理员可见。侧边栏按当前账号权限过滤（后端仍独立校验；
   * 这里只是不把无权入口摆出来）。取值 = 需要哪个权限，见 lib/types.ts Permissions。
   */
  adminOnly?: boolean;
  /** requires: 需要该权限才显示（例如访客没有 can_chat，就不显示对话）。 */
  requires?: "can_chat" | "can_create_task";
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

// SCA Audit 是代码审计平台：对外发请求/抓包/执行命令相关的入口（流量、工具执行、MCP、
// 自定义工具、拦截规则/审批）已从导航移除——对应的 agent 能力也已从工具层删除。
export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    label: "功能",
    items: [
      { id: "dashboard", title: "仪表盘", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", title: "对话", url: "/chat", icon: MessageSquare, requires: "can_chat" },
      { id: "tasks", title: "任务", url: "/function/tasks", icon: Target },
      { id: "findings", title: "发现", url: "/function/findings", icon: Bug },
      { id: "llm-records", title: "LLM 录制", url: "/function/llm-records", icon: Radio, adminOnly: true },
      { id: "assets", title: "测试资产", url: "/function/assets", icon: Network },
      { id: "sync", title: "资产同步", url: "/function/sync", icon: FolderSync, adminOnly: true },
      { id: "workspace", title: "工作空间", url: "/function/workspace", icon: FolderOpen, adminOnly: true },
    ],
  },
  {
    id: 2,
    label: "系统",
    items: [
      { id: "users", title: "账号管理", url: "/system/users", icon: Users, adminOnly: true },
      { id: "departments", title: "部门管理", url: "/system/departments", icon: Building2, adminOnly: true },
      { id: "llm", title: "LLM", url: "/system/llm", icon: Brain, adminOnly: true },
      { id: "agents", title: "Agent", url: "/system/agents", icon: Bot, adminOnly: true },
      { id: "skills", title: "Skill", url: "/system/skills", icon: Sparkles, adminOnly: true },
      { id: "notify", title: "通知推送", url: "/system/notify", icon: BellRing, adminOnly: true },
      { id: "logs", title: "日志", url: "/system/logs", icon: ScrollText, adminOnly: true },
      { id: "settings", title: "系统配置", url: "/system/settings", icon: Settings2, adminOnly: true },
    ],
  },
];
