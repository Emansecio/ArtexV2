"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";

import { ChevronRight } from "lucide-react";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { t } from "@/i18n";
import { cn } from "@/lib/utils";
import type {
  NavBadge,
  NavGroup,
  NavMainItem,
  NavMainLinkItem,
  NavMainParentItem,
} from "@/navigation/sidebar/sidebar-items";
import { useInterceptPendingCount } from "@/lib/intercept-pending";
import { useRunningTasksCount } from "@/lib/running-tasks";

const SIDEBAR_OPEN_KEY = "artex.sidebar.open";

interface NavMainProps {
  readonly items: readonly NavGroup[];
}
interface NavItemProps {
  readonly item: NavMainItem;
  readonly isItemActive: (item: NavMainItem) => boolean;
  readonly activeSubUrl: (item: NavMainParentItem) => string | null;
  readonly isOpen: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly pendingCount: number;
  readonly runningCount: number;
}

interface NavLinkItemProps {
  readonly item: NavMainLinkItem;
  readonly isActive: boolean;
  readonly showIconFallback: boolean;
  readonly pendingCount: number;
  readonly runningCount: number;
}

interface NavLinkIconProps {
  readonly item: NavMainLinkItem;
  readonly showFallback: boolean;
}

interface NavDropdownItemProps {
  readonly item: NavMainParentItem;
  readonly isActive: boolean;
  readonly activeSubUrl: string | null;
  readonly pendingCount: number;
}

interface NavCollapsibleItemProps {
  readonly item: NavMainParentItem;
  readonly isActive: boolean;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly activeSubUrl: string | null;
  readonly pendingCount: number;
}

function CollapsedIconFallback({ title }: { title: string }) {
  return (
    <span className="flex size-4 shrink-0 items-center justify-center rounded-xs font-medium text-[10px] outline">
      {title.slice(0, 1)}
    </span>
  );
}

function hasSubItems(item: NavMainItem): item is NavMainParentItem {
  return Boolean(item.subItems?.length);
}

function matchPath(path: string, url: string): boolean {
  return path === url || path.startsWith(`${url}/`);
}

export function NavMain({ items }: NavMainProps) {
  const rawPath = usePathname();
  const pendingCount = useInterceptPendingCount();
  const runningCount = useRunningTasksCount();
  // Route segments contain Chinese, so the pathname may be percent-encoded; decode it before comparing against nav urls so highlighting hits.
  const path = (() => {
    try {
      return decodeURIComponent(rawPath);
    } catch {
      return rawPath;
    }
  })();

  const activeSubUrl = React.useCallback(
    (item: NavMainParentItem): string | null => {
      let best: string | null = null;
      for (const sub of item.subItems) {
        if (matchPath(path, sub.url) && (!best || sub.url.length > best.length)) {
          best = sub.url;
        }
      }
      return best;
    },
    [path],
  );

  const isItemActive = (item: NavMainItem) => {
    if (hasSubItems(item)) {
      return activeSubUrl(item) !== null;
    }
    return matchPath(path, item.url);
  };

  const [openMap, setOpenMap] = React.useState<Record<string, boolean>>(() => {
    const init: Record<string, boolean> = {};
    for (const group of items) {
      for (const item of group.items) {
        if (hasSubItems(item) && item.subItems.some((sub) => matchPath(path, sub.url))) {
          init[item.id] = true;
        }
      }
    }
    return init;
  });

  React.useEffect(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(SIDEBAR_OPEN_KEY) ?? "{}") as Record<string, boolean>;
      if (saved && typeof saved === "object") {
        setOpenMap((prev) => ({ ...saved, ...prev }));
      }
    } catch {
      /* ignore malformed storage */
    }
  }, []);

  React.useEffect(() => {
    const activeIds: string[] = [];
    for (const group of items) {
      for (const item of group.items) {
        if (hasSubItems(item) && activeSubUrl(item) !== null) {
          activeIds.push(item.id);
        }
      }
    }
    if (activeIds.length === 0) return;
    setOpenMap((prev) => {
      if (activeIds.every((id) => prev[id])) return prev;
      const next = { ...prev };
      for (const id of activeIds) next[id] = true;
      return next;
    });
  }, [items, activeSubUrl]);

  const handleOpenChange = (id: string, open: boolean) => {
    setOpenMap((prev) => {
      const next = { ...prev, [id]: open };
      try {
        localStorage.setItem(SIDEBAR_OPEN_KEY, JSON.stringify(next));
      } catch {
        /* ignore storage quota errors */
      }
      return next;
    });
  };

  return (
    <>
      {items.map((group) => (
        <SidebarGroup key={group.id}>
          {group.label && (
            <SidebarGroupLabel className="group-data-[collapsible=icon]:pointer-events-none">
              {group.label}
            </SidebarGroupLabel>
          )}
          <SidebarGroupContent>
            <SidebarMenu>
              {group.items.map((item) => (
                <NavItem
                  key={item.id}
                  item={item}
                  isItemActive={isItemActive}
                  activeSubUrl={activeSubUrl}
                  isOpen={Boolean(openMap[item.id])}
                  onOpenChange={(open) => handleOpenChange(item.id, open)}
                  pendingCount={pendingCount}
                  runningCount={runningCount}
                />
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      ))}
    </>
  );
}

function NavItem({ item, isItemActive, activeSubUrl, isOpen, onOpenChange, pendingCount, runningCount }: NavItemProps) {
  const { state, isMobile } = useSidebar();
  const isCollapsedDesktop = state === "collapsed" && !isMobile;

  if (!hasSubItems(item)) {
    return (
      <NavLinkItem
        item={item}
        isActive={isItemActive(item)}
        showIconFallback={isCollapsedDesktop}
        pendingCount={pendingCount}
        runningCount={runningCount}
      />
    );
  }

  const subActive = activeSubUrl(item);

  if (isCollapsedDesktop) {
    return (
      <NavDropdownItem
        item={item}
        isActive={isItemActive(item)}
        activeSubUrl={subActive}
        pendingCount={pendingCount}
      />
    );
  }

  return (
    <NavCollapsibleItem
      item={item}
      isActive={isItemActive(item)}
      open={isOpen}
      onOpenChange={onOpenChange}
      activeSubUrl={subActive}
      pendingCount={pendingCount}
    />
  );
}

function PendingPill({ count, className }: { count: number; className?: string }) {
  if (count <= 0) return null;
  return (
    <span
      className={cn(
        "flex h-4.5 min-w-[18px] items-center justify-center rounded-full bg-amber-500 px-1 text-[10px] font-semibold leading-none text-white shadow-sm ring-1 ring-amber-400/40 tabular-nums",
        className,
      )}
    >
      {count > 99 ? "99+" : count}
    </span>
  );
}

function RunningPill({ count, className }: { count: number; className?: string }) {
  if (count <= 0) return null;
  return (
    <span
      className={cn(
        "inline-flex h-4.5 min-w-[18px] items-center justify-center gap-1 rounded-full bg-blue-500/15 px-1.5 text-[10px] font-semibold leading-none text-blue-600 tabular-nums dark:text-blue-400",
        className,
      )}
    >
      <span className="size-1.5 motion-safe:animate-pulse rounded-full bg-blue-500" />
      {count > 99 ? "99+" : count}
    </span>
  );
}

function NavLinkItem({ item, isActive, showIconFallback, pendingCount, runningCount }: NavLinkItemProps) {
  return (
    <SidebarMenuItem>
      <SidebarMenuButton asChild aria-disabled={item.disabled} tooltip={item.title} isActive={isActive}>
        <Link
          prefetch={false}
          href={item.url}
          target={item.newTab ? "_blank" : undefined}
          rel={item.newTab ? "noreferrer" : undefined}
        >
          <NavLinkIcon item={item} showFallback={showIconFallback} />
          <span>{item.title}</span>
        </Link>
      </SidebarMenuButton>
      {item.id === "approvals" && pendingCount > 0 ? (
        <SidebarMenuBadge>
          <PendingPill count={pendingCount} />
        </SidebarMenuBadge>
      ) : item.id === "tasks" && runningCount > 0 ? (
        <SidebarMenuBadge>
          <RunningPill count={runningCount} />
        </SidebarMenuBadge>
      ) : (
        <NavItemBadge badge={item.badge} />
      )}
    </SidebarMenuItem>
  );
}

function NavLinkIcon({ item, showFallback }: NavLinkIconProps) {
  const Icon = item.icon;

  if (Icon) {
    return <Icon />;
  }

  if (showFallback) {
    return <CollapsedIconFallback title={item.title} />;
  }

  return null;
}

function NavDropdownItem({ item, isActive, activeSubUrl, pendingCount }: NavDropdownItemProps) {
  const Icon = item.icon;
  const showParentDot = item.id === "interception" && pendingCount > 0;

  return (
    <SidebarMenuItem>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <SidebarMenuButton tooltip={item.title} isActive={isActive} disabled={item.disabled} className="relative">
            {Icon ? <Icon /> : <CollapsedIconFallback title={item.title} />}
            <span>{item.title}</span>
            {showParentDot && (
              <span
                aria-hidden="true"
                className="absolute top-1.5 right-1.5 size-2 rounded-full bg-amber-500 ring-2 ring-sidebar"
              />
            )}
          </SidebarMenuButton>
        </DropdownMenuTrigger>

        <DropdownMenuContent side="right" align="start" sideOffset={12} className="w-48">
          <DropdownMenuGroup>
            {item.subItems.map((subItem) => {
              const SubIcon = subItem.icon;

              return (
                <DropdownMenuItem key={subItem.id} asChild disabled={subItem.disabled}>
                  <Link
                    prefetch={false}
                    href={subItem.url}
                    target={subItem.newTab ? "_blank" : undefined}
                    rel={subItem.newTab ? "noreferrer" : undefined}
                    aria-current={activeSubUrl === subItem.url ? "page" : undefined}
                    className="flex items-center gap-2"
                  >
                    {SubIcon && <SubIcon />}
                    <span className="truncate">{subItem.title}</span>
                    {subItem.id === "approvals" && <PendingPill count={pendingCount} className="ml-auto" />}
                  </Link>
                </DropdownMenuItem>
              );
            })}
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
    </SidebarMenuItem>
  );
}

function NavCollapsibleItem({
  item,
  isActive,
  open,
  onOpenChange,
  activeSubUrl,
  pendingCount,
}: NavCollapsibleItemProps) {
  const Icon = item.icon;
  const showParentBadge = item.id === "interception" && pendingCount > 0;

  return (
    <Collapsible asChild open={open} onOpenChange={onOpenChange} className="group/collapsible">
      <SidebarMenuItem>
        <CollapsibleTrigger asChild>
          <SidebarMenuButton tooltip={item.title} isActive={isActive} disabled={item.disabled}>
            {Icon && <Icon />}
            <span>{item.title}</span>
            {showParentBadge ? (
              <span className="ml-auto flex items-center gap-1.5">
                <PendingPill count={pendingCount} />
                <ChevronRight className="size-4 transition-transform duration-200 group-data-[state=open]/collapsible:rotate-90" />
              </span>
            ) : (
              <ChevronRight className="ml-auto transition-transform duration-200 group-data-[state=open]/collapsible:rotate-90" />
            )}
          </SidebarMenuButton>
        </CollapsibleTrigger>
        <NavItemBadge badge={item.badge} />

        <CollapsibleContent>
          <SidebarMenuSub>
            {item.subItems.map((subItem) => {
              const SubIcon = subItem.icon;

              return (
                <SidebarMenuSubItem key={subItem.id}>
                  <SidebarMenuSubButton
                    asChild
                    aria-disabled={subItem.disabled}
                    isActive={activeSubUrl === subItem.url}
                  >
                    <Link
                      prefetch={false}
                      href={subItem.url}
                      target={subItem.newTab ? "_blank" : undefined}
                      rel={subItem.newTab ? "noreferrer" : undefined}
                    >
                      {SubIcon && <SubIcon />}
                      <span className="truncate">{subItem.title}</span>
                      {subItem.id === "approvals" && <PendingPill count={pendingCount} className="ml-auto" />}
                    </Link>
                  </SidebarMenuSubButton>
                </SidebarMenuSubItem>
              );
            })}
          </SidebarMenuSub>
        </CollapsibleContent>
      </SidebarMenuItem>
    </Collapsible>
  );
}

function NavItemBadge({ badge }: { badge?: NavBadge }) {
  if (!badge) {
    return null;
  }

  return (
    <SidebarMenuBadge
      className={cn(
        "rounded-sm border capitalize",
        badge === "new" &&
          "border-green-600 text-green-600 peer-hover/menu-button:text-green-600 peer-data-active/menu-button:text-green-600",
        badge === "soon" && "border-muted-foreground text-muted-foreground",
      )}
    >
      {t(badge)}
    </SidebarMenuBadge>
  );
}
