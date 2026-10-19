"use client";

import type { ReactNode } from "react";

import { AdminOnly } from "@/app/(main)/_components/admin-only";

// 流量页没有导航入口（只能直敲 URL），因此页面守卫是它唯一的可见性控制。
export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  return <AdminOnly>{children}</AdminOnly>;
}
