"use client";

import * as React from "react";

import Link from "next/link";

import { useShallow } from "zustand/react/shallow";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { APP_CONFIG } from "@/config/app-config";
import { useCurrentUser } from "@/hooks/use-current-user";
import { useAuth } from "@/lib/auth-context";
import type { Permissions } from "@/lib/types";
import { type NavGroup, type NavMainItem, sidebarItems } from "@/navigation/sidebar/sidebar-items";
import { usePreferencesStore } from "@/stores/preferences/preferences-provider";

import { NavMain } from "./nav-main";
import { NavUser } from "./nav-user";

// canSeeNavItem 判断当前账号是否该看到某个导航入口：
// - adminOnly（系统区 / 工作空间 / 资产同步 / 账号与部门管理）：只有管理员才显示；
// - requires：直接查对应权限（访客没有 can_chat，就看不到对话入口）。
// 仅仅是「不把无权入口摆出来」，后端每个接口仍独立校验权限。
export function canSeeNavItem(
  item: Pick<NavMainItem, "adminOnly" | "requires">,
  permissions: Permissions | undefined,
): boolean {
  // 权限未知时按 fail-closed 处理：管理类入口一律不显示（避免接口还没回来时闪出
  // 无权入口），普通入口照常显示——否则首屏会把侧边栏清空，体验很差。真正的边界
  // 始终在后端。
  if (!permissions) return !item.adminOnly;
  if (item.adminOnly && !permissions.can_manage_users) return false;
  if (item.requires && !permissions[item.requires]) return false;
  return true;
}

// filterNavGroups 按组过滤入口；一组里一条都不剩时整组隐藏，免得留个空标题。
function filterNavGroups(groups: readonly NavGroup[], permissions: Permissions | undefined): NavGroup[] {
  return groups
    .map((group) => ({ ...group, items: group.items.filter((item) => canSeeNavItem(item, permissions)) }))
    .filter((group) => group.items.length > 0);
}

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const currentUser = useCurrentUser();
  const { permissions } = useAuth();
  const { sidebarVariant, sidebarCollapsible, isSynced } = usePreferencesStore(
    useShallow((s) => ({
      sidebarVariant: s.sidebarVariant,
      sidebarCollapsible: s.sidebarCollapsible,
      isSynced: s.isSynced,
    })),
  );

  // 账号/权限变化（登录、切换账号、/api/auth/me 回来）后重新计算可见入口。
  const navGroups = React.useMemo(() => filterNavGroups(sidebarItems, permissions), [permissions]);

  const variant = isSynced ? sidebarVariant : props.variant;
  const collapsible = isSynced ? sidebarCollapsible : props.collapsible;

  return (
    <Sidebar {...props} variant={variant} collapsible={collapsible}>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild>
              <Link prefetch={false} href="/function/tasks">
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src="/logo.png" alt="SCA Audit" width={40} height={40} className="shrink-0" />
                <span className="font-semibold text-base">{APP_CONFIG.name}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <NavMain items={navGroups} />
        {/* <NavDocuments items={data.documents} /> */}
        {/* <NavSecondary items={data.navSecondary} className="mt-auto" /> */}
      </SidebarContent>
      <SidebarFooter>
        <NavUser user={currentUser} />
      </SidebarFooter>
    </Sidebar>
  );
}
