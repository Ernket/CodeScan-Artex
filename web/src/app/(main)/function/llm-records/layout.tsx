"use client";

import type { ReactNode } from "react";

import { AdminOnly } from "@/app/(main)/_components/admin-only";

// LLM 录制含原始请求/响应体（源码 + 提示词），敏感度等同系统配置，仅管理员可进。
export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  return <AdminOnly>{children}</AdminOnly>;
}
