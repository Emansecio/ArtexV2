"use client";

import * as React from "react";

import {
  BrainIcon,
  CheckIcon,
  Code2Icon,
  CopyIcon,
  HistoryIcon,
  MoreHorizontalIcon,
  PanelLeftOpenIcon,
  PauseIcon,
  PlayIcon,
  RefreshCwIcon,
  Trash2Icon,
  UserIcon,
} from "lucide-react";
import { toast } from "sonner";

import { SideQuestionButton } from "@/components/side-question-workspace";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { useSideQuestions } from "@/hooks/use-side-questions";
import { t } from "@/i18n";
import { taskAssetSourceLabel, taskAssetTypeLabel } from "@/lib/task-assets";
import type { IntentAsset, Session, TaskLLMResolution, TokenTotal } from "@/lib/types";

import { CompactTokenPill, workerStatusIcon } from "./agent-sidebar";

function resolutionLabel(r: TaskLLMResolution): string {
  return r.name || r.model || t("Unnamed config");
}

function truncateAssetLabel(value: string, maxChars = 28): string {
  const chars = Array.from(value.trim());
  if (chars.length <= maxChars) return chars.join("");
  return `${chars.slice(0, Math.max(0, maxChars - 1)).join("")}…`;
}

function WorkerAssetBadge({ assets }: { assets: IntentAsset[] }) {
  const displayAssets = assets.filter(
    (asset) => asset.type === "root_domain" || asset.type === "subdomain" || asset.type === "ip",
  );
  if (displayAssets.length === 0) return null;

  const first = displayAssets[0];
  const firstRawLabel = first.label.trim() || `#${first.asset_id}`;
  const firstLabel = truncateAssetLabel(firstRawLabel);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="max-w-52 shrink-0 font-mono text-[11px] font-normal" title={firstRawLabel}>
          <span className="truncate">{firstLabel}</span>
          {displayAssets.length > 1 && <span className="shrink-0 tabular-nums">+{displayAssets.length - 1}</span>}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="bottom" align="start" className="max-w-sm">
        <div className="flex flex-col gap-2">
          {displayAssets.map((asset) => (
            <div key={`${asset.intent_id}-${asset.asset_id}`} className="min-w-0">
              <div className="break-all font-mono text-xs">{asset.label.trim() || `#${asset.asset_id}`}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">
                {taskAssetTypeLabel(asset.type)} · {taskAssetSourceLabel(asset.source)}
                {asset.inherited ? ` · ${t("from source task #{id}", { id: asset.source_task_id ?? "" })}` : ""}
              </div>
              <div className="mt-0.5 [overflow-wrap:anywhere] text-xs">{asset.source_summary}</div>
            </div>
          ))}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

export interface AgentTranscriptHeaderProps {
  active: Session;
  displayTitle: string;
  intentJson?: unknown;
  deleted?: boolean;
  railCollapsed: boolean;
  onExpandRail: () => void;
  side: ReturnType<typeof useSideQuestions>;
  activeResolution?: TaskLLMResolution;
  activeAssets?: IntentAsset[];
  hasMoreHistory?: boolean;
  tokens: TokenTotal;
  durationNode: React.ReactNode;
  controllingIntent: string | null;
  rerunningIntent: boolean;
  onControlWorker: (session: Session, action: "pause" | "resume") => Promise<void>;
  onRerunWorker: (session: Session) => Promise<void>;
  onRequestCancelWorker: (session: Session) => void;
}

export function AgentTranscriptHeader({
  active,
  displayTitle,
  intentJson,
  deleted,
  railCollapsed,
  onExpandRail,
  side,
  activeResolution,
  activeAssets,
  hasMoreHistory,
  tokens,
  durationNode,
  controllingIntent,
  rerunningIntent,
  onControlWorker,
  onRerunWorker,
  onRequestCancelWorker,
}: AgentTranscriptHeaderProps) {
  const [jsonOpen, setJsonOpen] = React.useState(false);
  const [copied, setCopied] = React.useState(false);

  const isWorker = active.role === "worker";
  const isMain = active.role === "mainagent";
  const isPlanner = active.role === "planner";
  const isSystem = active.role === "system";
  const busy = controllingIntent === active.intent_id || rerunningIntent;

  const canPause = isWorker && !active.inherited && !deleted && active.status === "running";
  const canResume = isWorker && !active.inherited && !deleted && active.status === "paused";
  const canRerun =
    isWorker &&
    !active.inherited &&
    !deleted &&
    (active.status === "blocked" || active.status === "exhausted" || active.status === "stopped");
  const canDelete =
    isWorker &&
    !active.inherited &&
    !deleted &&
    (active.status === "running" || active.status === "paused" || active.status === "pending");

  const formattedJson = React.useMemo(
    () => (intentJson ? JSON.stringify(intentJson, null, 2) : ""),
    [intentJson],
  );

  async function copyJson() {
    if (!formattedJson) return;
    try {
      await navigator.clipboard.writeText(formattedJson);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      toast.error(t("Copy failed"));
    }
  }

  return (
    <>
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5 border-b px-3 py-2 sm:px-4">
        {railCollapsed && (
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            className="hidden shrink-0 text-muted-foreground lg:inline-flex"
            onClick={onExpandRail}
            aria-label={t("Expand")}
            title={t("Expand")}
          >
            <PanelLeftOpenIcon />
          </Button>
        )}

        {/* Role icon + real identity */}
        <div className="flex min-w-0 flex-1 items-center gap-2">
          {isMain && <UserIcon className="size-4 shrink-0 text-primary" />}
          {isPlanner && <BrainIcon className="size-4 shrink-0 text-amber-500" />}
          {isSystem && <HistoryIcon className="size-4 shrink-0 text-muted-foreground" />}
          {isWorker && workerStatusIcon(active.status)}

          {isWorker && active.intent_id && (
            <code className="shrink-0 rounded bg-muted px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-muted-foreground">
              #{active.intent_id}
            </code>
          )}
          <span className="min-w-0 truncate text-sm font-semibold" title={displayTitle}>
            {displayTitle}
          </span>
          {active.live && (
            <span className="inline-flex shrink-0 items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
              <span className="size-1 motion-safe:animate-pulse rounded-full bg-blue-500" />
              {t("Live")}
            </span>
          )}
        </div>

        {/* Metadata pills */}
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          {activeAssets && activeAssets.length > 0 && <WorkerAssetBadge assets={activeAssets} />}
          {activeResolution && (
            <Tooltip>
              <TooltipTrigger asChild>
                <Badge
                  variant="outline"
                  className="max-w-28 shrink-0 font-normal"
                  aria-label={
                    activeResolution.available
                      ? t("Current config: {name}", { name: resolutionLabel(activeResolution) })
                      : t("Model unavailable")
                  }
                >
                  <span className="truncate">
                    {activeResolution.available ? resolutionLabel(activeResolution) : t("Model unavailable")}
                  </span>
                </Badge>
              </TooltipTrigger>
              <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
                {activeResolution.available
                  ? [resolutionLabel(activeResolution), activeResolution.model].filter(Boolean).join(" / ")
                  : activeResolution.reason || t("No available LLM config")}
              </TooltipContent>
            </Tooltip>
          )}
          {active.inherited && active.source_task_id && (
            <Badge variant="outline" className="font-normal">
              {t("Source task #{id} · read-only history", { id: active.source_task_id })}
            </Badge>
          )}
          <CompactTokenPill tokens={tokens} />
          {durationNode}
          {hasMoreHistory && (
            <span className="hidden text-[10px] xl:inline" title={t("Scroll up to load earlier history")}>
              ↑ {t("Earlier history")}
            </span>
          )}
        </div>

        {/* Contextual actions */}
        <div className="flex shrink-0 items-center gap-1">
          {canPause && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="h-7 px-2 text-xs"
              disabled={busy}
              onClick={() => void onControlWorker(active, "pause")}
            >
              {busy ? <Spinner className="size-3" /> : <PauseIcon className="size-3" />}
              {t("Pause")}
            </Button>
          )}
          {canResume && (
            <Button
              type="button"
              size="sm"
              variant="default"
              className="h-7 px-2 text-xs"
              disabled={busy}
              onClick={() => void onControlWorker(active, "resume")}
            >
              {busy ? <Spinner className="size-3" /> : <PlayIcon className="size-3" />}
              {t("Resume")}
            </Button>
          )}
          {canRerun && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="h-7 px-2 text-xs"
              disabled={busy}
              onClick={() => void onRerunWorker(active)}
            >
              <RefreshCwIcon className={`size-3 ${rerunningIntent ? "animate-spin" : ""}`} />
              {t("Re-run")}
            </Button>
          )}
          <SideQuestionButton side={side} />
          {isWorker && (Boolean(intentJson) || canDelete) && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button type="button" variant="ghost" size="icon-xs" aria-label={t("Actions")} title={t("Actions")}>
                  <MoreHorizontalIcon />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-48">
                {Boolean(intentJson) && (
                  <DropdownMenuItem onSelect={() => setJsonOpen(true)}>
                    <Code2Icon />
                    {t("Inspect intent JSON")}
                  </DropdownMenuItem>
                )}
                {Boolean(intentJson) && canDelete && <DropdownMenuSeparator />}
                {canDelete && (
                  <DropdownMenuItem variant="destructive" onSelect={() => onRequestCancelWorker(active)}>
                    <Trash2Icon />
                    {t("Delete")}
                  </DropdownMenuItem>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
      </div>

      {/* Intent JSON Inspector Dialog */}
      <Dialog open={jsonOpen} onOpenChange={setJsonOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle className="flex items-center justify-between gap-2 pr-6">
              <span className="truncate">
                {active.intent_id ? `Worker #${active.intent_id}` : displayTitle} · JSON
              </span>
              <Button type="button" size="sm" variant="outline" className="h-7 px-2 text-xs" onClick={() => void copyJson()}>
                {copied ? <CheckIcon className="size-3 text-emerald-500" /> : <CopyIcon className="size-3" />}
                {copied ? t("Copied") : t("Copy")}
              </Button>
            </DialogTitle>
            <DialogDescription className="truncate">{displayTitle}</DialogDescription>
          </DialogHeader>
          <pre className="max-h-[60vh] overflow-auto rounded-md border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
            {formattedJson}
          </pre>
        </DialogContent>
      </Dialog>
    </>
  );
}
