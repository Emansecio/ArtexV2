"use client";

import * as React from "react";

import {
  BrainIcon,
  ChevronDownIcon,
  CircleCheckIcon,
  CircleSlashIcon,
  CircleXIcon,
  ClockIcon,
  HistoryIcon,
  Loader2Icon,
  PanelLeftCloseIcon,
  PauseIcon,
  PlusIcon,
  RadioIcon,
  RotateCwIcon,
  SearchIcon,
  ShieldAlertIcon,
  Trash2Icon,
  UserIcon,
  WifiOffIcon,
  XIcon,
  ZapOffIcon,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { intlLocale, t } from "@/i18n";
import { MOCK } from "@/lib/mock/enabled";
import type { IntentAsset, Session, SessionStatus, TokenTotal } from "@/lib/types";
import { cn } from "@/lib/utils";

export type WorkerFilter = "all" | "active" | "done" | "failed";

export function fmtCompactTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

export function workerStatusIcon(status: SessionStatus) {
  switch (status) {
    case "running":
      return <Loader2Icon className="size-3.5 shrink-0 animate-spin text-blue-500" />;
    case "paused":
      return <PauseIcon className="size-3.5 shrink-0 text-amber-500" />;
    case "pending":
      return <ClockIcon className="size-3.5 shrink-0 text-muted-foreground" />;
    case "done":
      return <CircleCheckIcon className="size-3.5 shrink-0 text-emerald-500" />;
    case "stopped":
      return <CircleSlashIcon className="size-3.5 shrink-0 text-amber-500" />;
    case "blocked":
      return <CircleXIcon className="size-3.5 shrink-0 text-red-500" />;
    case "exhausted":
      return <ZapOffIcon className="size-3.5 shrink-0 text-violet-500" />;
    case "deleted":
      return <CircleSlashIcon className="size-3.5 shrink-0 text-muted-foreground" />;
  }
}

function workerStatusShortLabel(status: SessionStatus, deleted?: boolean): { label: string; cls: string } {
  if (deleted || status === "deleted") return { label: t("Deleted"), cls: "text-destructive" };
  switch (status) {
    case "running":
      return { label: t("Running"), cls: "text-blue-600 dark:text-blue-400" };
    case "paused":
      return { label: t("Paused"), cls: "text-amber-600 dark:text-amber-400" };
    case "pending":
      return { label: t("Queued"), cls: "text-muted-foreground" };
    case "done":
      return { label: t("Completed"), cls: "text-emerald-600 dark:text-emerald-400" };
    case "stopped":
      return { label: t("Stopped"), cls: "text-amber-600 dark:text-amber-400" };
    case "blocked":
      return { label: t("Blocked"), cls: "text-red-600 dark:text-red-400" };
    case "exhausted":
      return { label: t("Exhausted"), cls: "text-violet-600 dark:text-violet-400" };
  }
}

function primaryAssetLabel(assets?: IntentAsset[]): string {
  if (!assets || assets.length === 0) return "";
  const preferred =
    assets.find((a) => a.type === "root_domain" || a.type === "subdomain" || a.type === "ip") ?? assets[0];
  return preferred.label.trim() || `#${preferred.asset_id}`;
}

export function CompactTokenPill({
  tokens,
  prefix,
  className,
}: {
  tokens: TokenTotal;
  prefix?: string;
  className?: string;
}) {
  const total = tokens.input_tokens + tokens.output_tokens;
  if (total <= 0 && tokens.cache_read_tokens <= 0) return null;
  const cachePct =
    tokens.input_tokens > 0 ? Math.round((tokens.cache_read_tokens / tokens.input_tokens) * 100) : 0;

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className={cn(
            "inline-flex shrink-0 items-center gap-1 rounded-md bg-muted/70 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-muted-foreground",
            className,
          )}
        >
          {prefix && <span>{prefix}</span>}
          <span className="text-foreground">{fmtCompactTokens(total)}</span>
          {cachePct > 0 && <span className="text-emerald-600 dark:text-emerald-400">{cachePct}%</span>}
        </span>
      </TooltipTrigger>
      <TooltipContent>
        {t("Input {input} · Output {output} · Cache read {cacheRead} · Cache write {cacheWrite}", {
          input: tokens.input_tokens.toLocaleString(intlLocale),
          output: tokens.output_tokens.toLocaleString(intlLocale),
          cacheRead: tokens.cache_read_tokens.toLocaleString(intlLocale),
          cacheWrite: tokens.cache_write_tokens.toLocaleString(intlLocale),
        })}
      </TooltipContent>
    </Tooltip>
  );
}

export interface AgentSidebarProps {
  activeId: string;
  activeDisplayTitle: string;
  mainSessions: Session[];
  plannerSession: Session;
  systemSession: Session;
  workerSessions: Session[];
  sessionMeta: Map<string, { title: string; json: unknown; deleted: boolean; deleteReason: string }>;
  intentAssetsByID: Map<string, IntentAsset[]>;
  unreadFor: (session: Session) => number;
  hasPendingFor: (session: Session) => boolean;
  sseLive: boolean;
  taskTokens: TokenTotal | null;
  listOpen: boolean;
  onToggleListOpen: () => void;
  railCollapsed: boolean;
  onCollapseRail: () => void;
  hiddenUnread: number;
  creatingMain: boolean;
  onRequestNewMain: () => void;
  onSelectSession: (session: Session) => void;
  onRequestCancelWorker: (session: Session) => void;
  controllingIntent: string | null;
  intentsHasMore: boolean;
  loadingOlderIntents: boolean;
  onLoadOlderIntents: () => void;
  mainLoaded: boolean;
}

export function AgentSidebar({
  activeId,
  activeDisplayTitle,
  mainSessions,
  plannerSession,
  systemSession,
  workerSessions,
  sessionMeta,
  intentAssetsByID,
  unreadFor,
  hasPendingFor,
  sseLive,
  taskTokens,
  listOpen,
  onToggleListOpen,
  railCollapsed,
  onCollapseRail,
  hiddenUnread,
  creatingMain,
  onRequestNewMain,
  onSelectSession,
  onRequestCancelWorker,
  controllingIntent,
  intentsHasMore,
  loadingOlderIntents,
  onLoadOlderIntents,
  mainLoaded,
}: AgentSidebarProps) {
  const [workerFilter, setWorkerFilter] = React.useState<WorkerFilter>("all");
  const [workerSearch, setWorkerSearch] = React.useState("");

  // Active main session card: show currently selected main segment, or newest (index 0).
  const displayedMain = React.useMemo(
    () => mainSessions.find((s) => s.id === activeId) ?? mainSessions[0],
    [mainSessions, activeId],
  );
  const mainActive = displayedMain?.id === activeId;
  const plannerActive = plannerSession.id === activeId;
  const systemActive = systemSession.id === activeId;
  const systemUnread = unreadFor(systemSession);

  const workerCounts = React.useMemo(() => {
    let active = 0;
    let done = 0;
    let failed = 0;
    for (const s of workerSessions) {
      if (s.status === "running" || s.status === "pending" || s.status === "paused") active++;
      else if (s.status === "done") done++;
      else if (s.status === "blocked" || s.status === "exhausted" || s.status === "stopped") failed++;
    }
    return { all: workerSessions.length, active, done, failed };
  }, [workerSessions]);

  const filteredWorkers = React.useMemo(() => {
    const q = workerSearch.trim().toLowerCase();
    return workerSessions.filter((s) => {
      if (workerFilter === "active" && !(s.status === "running" || s.status === "pending" || s.status === "paused")) {
        return false;
      }
      if (workerFilter === "done" && s.status !== "done") return false;
      if (workerFilter === "failed" && !(s.status === "blocked" || s.status === "exhausted" || s.status === "stopped")) {
        return false;
      }
      if (!q) return true;
      const meta = sessionMeta.get(s.id);
      const title = (meta?.title ?? s.title).toLowerCase();
      const asset = primaryAssetLabel(s.intent_id ? intentAssetsByID.get(s.intent_id) : undefined).toLowerCase();
      return title.includes(q) || (s.intent_id ?? "").toLowerCase().includes(q) || asset.includes(q);
    });
  }, [workerSessions, workerFilter, workerSearch, sessionMeta, intentAssetsByID]);

  return (
    <div
      className={cn(
        "flex min-h-0 flex-col overflow-hidden rounded-lg border bg-card",
        railCollapsed && "lg:hidden",
      )}
    >
      {/* Top bar */}
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <button
          type="button"
          onClick={onToggleListOpen}
          aria-expanded={listOpen}
          aria-controls="session-list-panel"
          className="flex min-w-0 flex-1 items-center gap-1.5 text-left text-xs font-semibold text-foreground lg:pointer-events-none"
        >
          <ChevronDownIcon
            className={cn("size-3.5 shrink-0 text-muted-foreground transition-transform lg:hidden", !listOpen && "-rotate-90")}
          />
          <span className="shrink-0">{t("Agents")}</span>
          {!listOpen && (
            <>
              <span className="min-w-0 truncate font-normal text-muted-foreground lg:hidden" title={activeDisplayTitle}>
                · {activeDisplayTitle}
              </span>
              {hiddenUnread > 0 && (
                <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 lg:hidden dark:text-blue-400">
                  {hiddenUnread > 99 ? "99+" : hiddenUnread}
                </span>
              )}
            </>
          )}
        </button>
        {taskTokens && <CompactTokenPill tokens={taskTokens} className="hidden sm:inline-flex" />}
        {!MOCK && (
          <span
            className={cn(
              "inline-flex shrink-0 items-center gap-1 text-[10px] font-medium",
              sseLive ? "text-emerald-600 dark:text-emerald-400" : "text-amber-500",
            )}
            title={
              sseLive
                ? t("Live connection healthy")
                : t("Live connection lost, reconnecting automatically (history still visible)")
            }
          >
            {sseLive ? (
              <>
                <span className="size-1.5 motion-safe:animate-pulse rounded-full bg-emerald-500" />
                {t("Live")}
              </>
            ) : (
              <>
                <WifiOffIcon className="size-3" />
                {t("Reconnecting")}
              </>
            )}
          </span>
        )}
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          className="hidden shrink-0 text-muted-foreground lg:inline-flex"
          onClick={onCollapseRail}
          aria-label={t("Collapse")}
          title={t("Collapse")}
        >
          <PanelLeftCloseIcon />
        </Button>
      </div>

      {/* Body */}
      <div id="session-list-panel" className={cn("flex min-h-0 flex-1 flex-col", !listOpen && "max-lg:hidden")}>
        {/* Fixed Core Agents block */}
        <div className="flex flex-col gap-1.5 border-b bg-muted/20 p-2">
          <div className="px-1 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
            {t("Core agents")}
          </div>

          {/* Main Agent card */}
          {displayedMain && (
            <div
              className={cn(
                "flex items-center gap-2 rounded-md border px-2.5 py-2 transition-colors",
                mainActive
                  ? "border-primary/40 bg-accent text-accent-foreground shadow-2xs"
                  : "border-transparent bg-card/70 hover:border-border hover:bg-accent/40",
              )}
            >
              <button
                type="button"
                onClick={() => onSelectSession(displayedMain)}
                className="flex min-w-0 flex-1 items-center gap-2 text-left"
              >
                <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
                  {displayedMain.live ? <Loader2Icon className="size-3.5 animate-spin" /> : <UserIcon className="size-3.5" />}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-1.5">
                    <span className="truncate text-xs font-semibold">{t("Main Agent")}</span>
                    {hasPendingFor(displayedMain) && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
                    {!mainActive && unreadFor(displayedMain) > 0 && (
                      <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                        {unreadFor(displayedMain) > 99 ? "99+" : unreadFor(displayedMain)}
                      </span>
                    )}
                  </span>
                  <span className="block truncate text-[11px] text-muted-foreground">
                    {displayedMain.live ? t("Running") : t("Ready for input")}
                  </span>
                </span>
              </button>
              <div className="flex shrink-0 items-center gap-0.5">
                {mainSessions.length > 1 && (
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        className="h-6 px-1.5 font-mono text-[10px] text-muted-foreground"
                        title={t("Switch main agent session")}
                      >
                        #{(displayedMain.seg ?? 0) + 1}
                        <ChevronDownIcon className="size-3" />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end" className="w-48">
                      {mainSessions.map((seg) => (
                        <DropdownMenuItem key={seg.id} onSelect={() => onSelectSession(seg)}>
                          <span className="flex-1 truncate">{seg.title}</span>
                          {seg.id === activeId && <span className="size-1.5 rounded-full bg-primary" />}
                        </DropdownMenuItem>
                      ))}
                    </DropdownMenuContent>
                  </DropdownMenu>
                )}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  onClick={onRequestNewMain}
                  disabled={creatingMain}
                  title={t("New main agent session (clears context, task state preserved)")}
                  aria-label={t("New main agent session")}
                  className="text-muted-foreground hover:text-foreground"
                >
                  {creatingMain ? <Loader2Icon className="size-3.5 animate-spin" /> : <PlusIcon className="size-3.5" />}
                </Button>
              </div>
            </div>
          )}

          {/* Planner card */}
          <button
            type="button"
            onClick={() => onSelectSession(plannerSession)}
            className={cn(
              "flex w-full items-center gap-2 rounded-md border px-2.5 py-2 text-left transition-colors",
              plannerActive
                ? "border-amber-500/40 bg-accent text-accent-foreground shadow-2xs"
                : "border-transparent bg-card/70 hover:border-border hover:bg-accent/40",
            )}
          >
            <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-amber-500/15 text-amber-600 dark:text-amber-400">
              {plannerSession.live ? <Loader2Icon className="size-3.5 animate-spin" /> : <BrainIcon className="size-3.5" />}
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1.5">
                <span className="truncate text-xs font-semibold">{t("Planner")}</span>
                {hasPendingFor(plannerSession) && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
                {!plannerActive && unreadFor(plannerSession) > 0 && (
                  <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                    {unreadFor(plannerSession) > 99 ? "99+" : unreadFor(plannerSession)}
                  </span>
                )}
              </span>
              <span className="block truncate text-[11px] text-muted-foreground">
                {plannerSession.live ? t("Planning…") : t("Standby")}
              </span>
            </span>
          </button>
        </div>

        {/* Workers header + filters + search */}
        <div className="flex flex-col gap-1.5 border-b px-2.5 py-2">
          <div className="flex items-center justify-between gap-2">
            <span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              <RadioIcon className="size-3" />
              {t("Workers")}
              <span className="rounded-full bg-muted px-1.5 py-0.2 font-mono text-[10px] tabular-nums text-foreground">
                {workerCounts.all}
              </span>
            </span>
            {workerCounts.active > 0 && (
              <span className="inline-flex items-center gap-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                <span className="size-1.5 motion-safe:animate-pulse rounded-full bg-blue-500" />
                {workerCounts.active} {t("Active")}
              </span>
            )}
          </div>
          {workerSessions.length > 0 && (
            <div className="grid grid-cols-4 gap-0.5 rounded-md bg-muted/60 p-0.5 text-[10px]">
              {(
                [
                  ["all", t("All"), workerCounts.all],
                  ["active", t("Active"), workerCounts.active],
                  ["done", t("Completed"), workerCounts.done],
                  ["failed", t("Failed"), workerCounts.failed],
                ] as const
              ).map(([key, label, count]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setWorkerFilter(key)}
                  className={cn(
                    "flex items-center justify-center gap-1 rounded px-1 py-0.5 transition-colors",
                    workerFilter === key
                      ? "bg-background font-medium text-foreground shadow-2xs"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <span className="truncate">{label}</span>
                  <span className="tabular-nums opacity-70">{count}</span>
                </button>
              ))}
            </div>
          )}
          {workerSessions.length > 5 && (
            <div className="relative">
              <SearchIcon className="pointer-events-none absolute top-1/2 left-2 size-3 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={workerSearch}
                onChange={(e) => setWorkerSearch(e.target.value)}
                placeholder={t("Search workers…")}
                className="h-7 pl-7 pr-6 text-xs"
              />
              {workerSearch && (
                <button
                  type="button"
                  onClick={() => setWorkerSearch("")}
                  aria-label={t("Clear search")}
                  className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                >
                  <XIcon className="size-3" />
                </button>
              )}
            </div>
          )}
        </div>

        {/* Workers list */}
        <ScrollArea type="auto" className="min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!">
          <div className="flex flex-col gap-1 p-2">
            {filteredWorkers.map((s) => {
              const meta = sessionMeta.get(s.id);
              const deleted = meta?.deleted;
              const displayTitle = meta?.title ?? s.title;
              const active = s.id === activeId;
              const unread = unreadFor(s);
              const pending = hasPendingFor(s);
              const asset = primaryAssetLabel(s.intent_id ? intentAssetsByID.get(s.intent_id) : undefined);
              const statusMeta = workerStatusShortLabel(s.status, deleted);
              const cancellable =
                !s.inherited && !deleted && (s.status === "running" || s.status === "paused" || s.status === "pending");

              return (
                <div
                  key={s.id}
                  className={cn(
                    "group/worker relative flex items-start rounded-md border transition-colors",
                    active
                      ? "border-primary/40 bg-accent text-accent-foreground"
                      : "border-transparent hover:border-border/60 hover:bg-accent/40",
                  )}
                >
                  <button
                    type="button"
                    onClick={() => onSelectSession(s)}
                    className="flex min-w-0 flex-1 flex-col gap-1 px-2 py-1.5 text-left"
                  >
                    <div className="flex min-w-0 items-center gap-1.5">
                      {deleted ? <Trash2Icon className="size-3.5 shrink-0 text-destructive" /> : workerStatusIcon(s.status)}
                      {s.intent_id && (
                        <span className="shrink-0 font-mono text-[10px] tabular-nums text-muted-foreground">
                          #{s.intent_id}
                        </span>
                      )}
                      <span
                        className={cn(
                          "min-w-0 flex-1 truncate text-xs font-medium",
                          deleted && "text-muted-foreground line-through",
                        )}
                        title={displayTitle}
                      >
                        {displayTitle}
                      </span>
                      {pending && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
                      {!active && unread > 0 && (
                        <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                          {unread > 99 ? "99+" : unread}
                        </span>
                      )}
                    </div>
                    <div className="flex min-w-0 items-center gap-1.5 pl-5 text-[10px] text-muted-foreground">
                      <span className={cn("shrink-0 font-medium", statusMeta.cls)}>{statusMeta.label}</span>
                      {asset && (
                        <>
                          <span>·</span>
                          <span className="min-w-0 truncate font-mono" title={asset}>
                            {asset}
                          </span>
                        </>
                      )}
                      {s.inherited && s.source_task_id && (
                        <Badge variant="outline" className="h-4 px-1 text-[9px] font-normal">
                          {t("Source #{id}", { id: s.source_task_id })}
                        </Badge>
                      )}
                    </div>
                  </button>
                  {cancellable && (
                    <div className="flex shrink-0 items-center pt-1.5 pr-1 opacity-100 sm:opacity-0 sm:transition-opacity sm:group-hover/worker:opacity-100 sm:group-focus-within/worker:opacity-100">
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon-xs"
                        onClick={() => onRequestCancelWorker(s)}
                        disabled={controllingIntent === s.intent_id}
                        title={t("Delete this intent (reason required; soft or hard delete)")}
                        aria-label={t("Delete this intent (reason required; soft or hard delete)")}
                        className="text-destructive hover:text-destructive"
                      >
                        <Trash2Icon />
                      </Button>
                    </div>
                  )}
                </div>
              );
            })}

            {intentsHasMore && (
              <button
                type="button"
                onClick={onLoadOlderIntents}
                disabled={loadingOlderIntents}
                className="mt-0.5 flex items-center justify-center gap-1 rounded-md px-2 py-1.5 text-xs text-muted-foreground hover:bg-accent/50"
              >
                {loadingOlderIntents ? (
                  <Loader2Icon className="size-3.5 animate-spin" />
                ) : (
                  <RotateCwIcon className="size-3.5" />
                )}
                {t("Load earlier workers")}
              </button>
            )}

            {mainLoaded && workerSessions.length === 0 && (
              <div className="px-2 py-4 text-center text-xs text-muted-foreground">
                {t("No running worker sessions.")}
              </div>
            )}
            {workerSessions.length > 0 && filteredWorkers.length === 0 && (
              <div className="px-2 py-4 text-center text-xs text-muted-foreground">{t("No matching tasks.")}</div>
            )}
          </div>
        </ScrollArea>

        {/* System Audit footer */}
        <div className="border-t p-1.5">
          <button
            type="button"
            onClick={() => onSelectSession(systemSession)}
            className={cn(
              "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs transition-colors",
              systemActive
                ? "bg-accent font-medium text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50 hover:text-foreground",
            )}
          >
            <HistoryIcon className="size-3.5 shrink-0" />
            <span className="min-w-0 flex-1 truncate">{t("System audit")}</span>
            {!systemActive && systemUnread > 0 && (
              <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-amber-500/20 px-1 text-[10px] font-medium tabular-nums text-amber-600 dark:text-amber-400">
                {systemUnread > 99 ? "99+" : systemUnread}
              </span>
            )}
          </button>
        </div>
      </div>
    </div>
  );
}
