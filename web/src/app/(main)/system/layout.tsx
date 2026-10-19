"use client";

import type { ReactNode } from "react";

import { AdminOnly } from "@/app/(main)/_components/admin-only";

// 系统区（LLM / Agent / Skill / 通知推送 / 日志 / 系统配置 / 拦截审批 / 工具 / MCP）
// 整体仅管理员可进，与 server/policy.go 的 adminOnlyAreas 对应。
export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  return <AdminOnly>{children}</AdminOnly>;
}
