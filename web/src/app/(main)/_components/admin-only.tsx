"use client";

import type { ReactNode } from "react";
import * as React from "react";

import { useRouter } from "next/navigation";

import { useAuth } from "@/lib/auth-context";

// AdminOnly 是「管理员专属」页面的客户端门禁：系统区（LLM / Agent / Skill / 通知推送 /
// 日志 / 系统配置 / 拦截审批 / 工具 / MCP）、工作空间、资产同步、LLM 录制、流量。
//
// 为什么门禁必须放在客户端：生产构建是静态导出（next.config.mjs 在 NEXT_EXPORT=1 时
// 设 output:"export"），而静态导出不执行 middleware/proxy——所以 web/src/proxy.ts
// 只在 next dev / 非导出部署下生效，不能承担这些页面的守卫。
//
// 前端守卫只是体验层：真正的边界在后端（server/policy.go 的 guardAdminAreas）。
// 它解决的问题是——非管理员直接敲 URL 时，不再打开一个「界面全在、一操作就报 403」的页面。
export function AdminOnly({ children }: { children: ReactNode }) {
  const { user, permissions, loading } = useAuth();
  const router = useRouter();

  // can_manage_users 在后端只对 IsAdmin 为真（见 server/auth.go:238-248），
  // 所以两个判据等价；同时接受两者是为了不依赖单一字段。
  const isAdmin = user?.is_admin === true || permissions?.can_manage_users === true;

  React.useEffect(() => {
    if (loading || isAdmin) return;
    router.replace("/dashboard");
  }, [loading, isAdmin, router]);

  // 权限未确定、或确认无权时都不渲染内容：既避免管理界面闪现，也避免它把注定 403 的
  // 请求发出去（否则用户看到满屏错误提示）。
  if (loading || !isAdmin) return null;
  return <>{children}</>;
}
