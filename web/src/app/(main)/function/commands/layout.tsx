"use client";

import type { ReactNode } from "react";

import { AdminOnly } from "@/app/(main)/_components/admin-only";

// 命令/工具调用明细页没有导航入口（只能直敲 URL），仅管理员可进。
export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  return <AdminOnly>{children}</AdminOnly>;
}
