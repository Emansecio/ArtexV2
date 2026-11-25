"use client";

import { type ReactNode, useEffect, useState } from "react";

import Link from "next/link";
import { usePathname } from "next/navigation";

import { ShieldAlert, X } from "lucide-react";

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import { useInterceptPendingCount } from "@/lib/intercept-pending";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import { cn } from "@/lib/utils";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";

import { LayoutControls } from "./sidebar/layout-controls";
import { SearchDialog } from "./sidebar/search-dialog";
import { ThemeSwitcher } from "./sidebar/theme-switcher";

// Task detail pages stay as-is: they bring their own header/Tabs and padding, so no extra global header or padding is stacked here.
function isFullBleed(pathname: string) {
  const p = (() => {
    try {
      return decodeURIComponent(pathname);
    } catch {
      return pathname;
    }
  })();
  // Static export enables trailingSlash, so the list page's own pathname is "/function/tasks/",
  // strip the trailing slash before the prefix test, or the list page would be mistaken for a detail page and lose the global header.
  const normalized = p.replace(/\/+$/, "");
  return normalized.startsWith("/function/tasks/") || normalized.startsWith("/function/findings/");
}

const RELEASES_URL = "https://github.com/BaaRai/ARTEX/releases";
const UPDATE_DISMISSED_KEY = "artex-update-dismissed";

function resolveBreadcrumb(pathname: string): { group?: string; page: string } | null {
  const normalized = pathname.replace(/\/+$/, "") || "/";
  let best: { group?: string; page: string; len: number } | null = null;
  for (const group of sidebarItems) {
    for (const item of group.items) {
      if (item.subItems) {
        for (const sub of item.subItems) {
          if ((normalized === sub.url || normalized.startsWith(`${sub.url}/`)) && sub.url.length > (best?.len ?? 0)) {
            best = { group: item.title, page: sub.title, len: sub.url.length };
          }
        }
      } else if (
        (normalized === item.url || normalized.startsWith(`${item.url}/`)) &&
        item.url.length > (best?.len ?? 0)
      ) {
        best = { group: group.label, page: item.title, len: item.url.length };
      }
    }
  }
  return best ? { group: best.group, page: best.page } : null;
}

export function MainContent({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const pendingCount = useInterceptPendingCount();
  const crumb = resolveBreadcrumb(pathname);
  const [version, setVersion] = useState("");
  // Only set when the backend has the opt-in update check on (ARTEX_UPDATE_CHECK) and found a newer release.
  const [latest, setLatest] = useState("");

  useEffect(() => {
    api
      .health()
      .then((h) => {
        setVersion((h.version ?? "").replace(/^v(?=\d)/, ""));
        const l = h.latest_version ?? "";
        setLatest(l && getLocalStorageValue(UPDATE_DISMISSED_KEY) !== l ? l : "");
      })
      .catch(() => setVersion(""));
  }, []);

  const updateBanner = latest ? (
    <div className="flex shrink-0 items-center justify-center gap-2 border-b bg-muted/50 px-4 py-1 text-xs">
      <a href={RELEASES_URL} target="_blank" rel="noreferrer" className="underline-offset-2 hover:underline">
        {t("ARTEX {version} is available. See the release notes.", { version: latest })}
      </a>
      <button
        type="button"
        aria-label={t("Dismiss update notice")}
        className="text-muted-foreground hover:text-foreground"
        onClick={() => {
          setLocalStorageValue(UPDATE_DISMISSED_KEY, latest);
          setLatest("");
        }}
      >
        <X className="size-3.5" />
      </button>
    </div>
  ) : null;

  if (isFullBleed(pathname)) {
    return (
      <>
        <SearchDialog showTrigger={false} />
        {updateBanner}
        {children}
      </>
    );
  }

  return (
    <>
      {updateBanner}
      <header
        className={cn(
          "flex h-12 shrink-0 items-center gap-2 border-b transition-[width,height] ease-linear group-has-data-[collapsible=icon]/sidebar-wrapper:h-12",
          "[html[data-navbar-style=sticky]_&]:sticky [html[data-navbar-style=sticky]_&]:top-0 [html[data-navbar-style=sticky]_&]:z-50 [html[data-navbar-style=sticky]_&]:overflow-hidden [html[data-navbar-style=sticky]_&]:rounded-t-[inherit] [html[data-navbar-style=sticky]_&]:bg-background/95",
        )}
      >
        <div className="flex w-full items-center justify-between gap-2 px-4 lg:px-6">
          <div className="flex min-w-0 items-center gap-1 lg:gap-2">
            <SidebarTrigger className="-ml-1" />
            <Separator
              orientation="vertical"
              className="mx-1.5 data-[orientation=vertical]:h-4 data-[orientation=vertical]:self-center"
            />
            {crumb && (
              <Breadcrumb className="hidden min-w-0 sm:block">
                <BreadcrumbList className="flex-nowrap text-xs">
                  {crumb.group && (
                    <>
                      <BreadcrumbItem className="hidden md:inline-flex">
                        <span className="truncate">{crumb.group}</span>
                      </BreadcrumbItem>
                      <BreadcrumbSeparator className="hidden md:inline-flex" />
                    </>
                  )}
                  <BreadcrumbItem className="min-w-0">
                    <BreadcrumbPage className="truncate font-medium">{crumb.page}</BreadcrumbPage>
                  </BreadcrumbItem>
                </BreadcrumbList>
              </Breadcrumb>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-1.5 sm:gap-2">
            <SearchDialog />
            {pendingCount > 0 && (
              <Link
                href="/system/intercept/approvals"
                className="inline-flex h-7 items-center gap-1.5 rounded-md border border-amber-500/30 bg-amber-500/10 px-2 text-xs font-medium text-amber-600 transition-colors hover:bg-amber-500/20 dark:text-amber-400"
                title={t("Pending approvals")}
              >
                <ShieldAlert className="size-3.5 shrink-0" />
                <span className="hidden md:inline">{t("Pending approvals")}</span>
                <span className="tabular-nums font-semibold">{pendingCount > 99 ? "99+" : pendingCount}</span>
              </Link>
            )}
            {version && (
              <span className="hidden font-medium text-muted-foreground text-xs tabular-nums xl:inline">
                {t("Version · {version}", { version })}
              </span>
            )}
            <LayoutControls />
            <ThemeSwitcher />
          </div>
        </div>
      </header>
      <div className="min-h-0 min-w-0 flex-1 overflow-x-hidden p-4 has-data-[content-padding=false]:p-0 md:p-6 md:has-data-[content-padding=false]:p-0">
        {children}
      </div>
    </>
  );
}
