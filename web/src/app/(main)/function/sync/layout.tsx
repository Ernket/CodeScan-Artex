"use client";

import type { ReactNode } from "react";

import { AdminOnly } from "@/app/(main)/_components/admin-only";

// 资产同步（ScopeSentry 数据源与导入）仅管理员可进。
export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  return <AdminOnly>{children}</AdminOnly>;
}
