"use client";

import * as React from "react";

import Link from "next/link";
import dynamic from "next/dynamic";
import { useRouter, useSearchParams } from "next/navigation";

import {
  ArchiveIcon,
  ArrowLeftIcon,
  BotIcon,
  BrainIcon,
  BugIcon,
  CheckIcon,
  CircleAlertIcon,
  FileTextIcon,
  GitForkIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  NetworkIcon,
  PauseIcon,
  PlayIcon,
  RadarIcon,
  RadioIcon,
  RotateCwIcon,
  ShieldAlertIcon,
  ShieldCheckIcon,
} from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverDescription,
  PopoverHeader,
  PopoverTitle,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import type { LLMProfile, Task } from "@/lib/types";

import { SessionsTab } from "./_tabs/sessions-tab";
import { setVisibleInterval } from "@/lib/polling";

const AssetsTab = dynamic(() => import("./_tabs/assets-tab").then((m) => m.AssetsTab), { ssr: false });
const BroadcastTab = dynamic(() => import("./_tabs/broadcast-tab").then((m) => m.BroadcastTab), { ssr: false });
const CoverageGraphTab = dynamic(
  () => import("./_tabs/coverage-graph-tab").then((m) => m.CoverageGraphTab),
  { ssr: false },
);
const CredentialsTab = dynamic(() => import("./_tabs/credentials-tab").then((m) => m.CredentialsTab), {
  ssr: false,
});
const FindingsTab = dynamic(() => import("./_tabs/findings-tab").then((m) => m.FindingsTab), { ssr: false });
const GraphTab = dynamic(() => import("./_tabs/graph-tab").then((m) => m.GraphTab), { ssr: false });
const InterceptTab = dynamic(() => import("./_tabs/intercept-tab").then((m) => m.InterceptTab), { ssr: false });
const OverviewTab = dynamic(() => import("./_tabs/overview-tab").then((m) => m.OverviewTab), { ssr: false });
const TaskScopeRulesSection = dynamic(
  () => import("./_tabs/overview-tab").then((m) => m.TaskScopeRulesSection),
  { ssr: false },
);
const ReportTab = dynamic(() => import("./_tabs/report-tab").then((m) => m.ReportTab), { ssr: false });
const RetestsTab = dynamic(() => import("./_tabs/retests-tab").then((m) => m.RetestsTab), { ssr: false });

type PrimaryTab = "agents" | "overview" | "findings" | "graph" | "assets" | "approvals";

const PRIMARY_TABS: { value: PrimaryTab; label: string; icon: React.ElementType }[] = [
  { value: "agents", label: t("Agents"), icon: BotIcon },
  { value: "overview", label: t("Overview"), icon: LayoutDashboardIcon },
  { value: "findings", label: t("Findings"), icon: BugIcon },
  { value: "graph", label: t("Graph"), icon: GitForkIcon },
  { value: "assets", label: t("Assets & Scope"), icon: NetworkIcon },
  { value: "approvals", label: t("Approvals"), icon: ShieldAlertIcon },
];

type SubTabOption = { value: string; label: string; icon: React.ElementType };

const SUB_TABS: Partial<Record<PrimaryTab, SubTabOption[]>> = {
  agents: [
    { value: "sessions", label: t("Live agents"), icon: BotIcon },
    { value: "broadcast", label: t("Broadcast"), icon: RadioIcon },
  ],
  findings: [
    { value: "findings", label: t("Findings"), icon: BugIcon },
    { value: "retests", label: t("Retests"), icon: RotateCwIcon },
    { value: "report", label: t("Report"), icon: FileTextIcon },
  ],
  graph: [
    { value: "graph", label: t("Exploration graph"), icon: GitForkIcon },
    { value: "coverage", label: t("Asset coverage"), icon: RadarIcon },
  ],
  assets: [
    { value: "assets", label: t("Test assets"), icon: NetworkIcon },
    { value: "credentials", label: t("Credentials"), icon: KeyRoundIcon },
    { value: "scope", label: t("Scope & Rules"), icon: ShieldCheckIcon },
  ],
};

function resolveTabParam(raw: string | null): { primary: PrimaryTab; view: string } {
  switch (raw) {
    case "broadcast":
      return { primary: "agents", view: "broadcast" };
    case "overview":
      return { primary: "overview", view: "overview" };
    case "findings":
      return { primary: "findings", view: "findings" };
    case "retests":
      return { primary: "findings", view: "retests" };
    case "report":
      return { primary: "findings", view: "report" };
    case "graph":
      return { primary: "graph", view: "graph" };
    case "coverage":
      return { primary: "graph", view: "coverage" };
    case "assets":
      return { primary: "assets", view: "assets" };
    case "credentials":
      return { primary: "assets", view: "credentials" };
    case "scope":
      return { primary: "assets", view: "scope" };
    case "intercept":
    case "approvals":
      return { primary: "approvals", view: "intercept" };
    case "sessions":
    case "agents":
    default:
      return { primary: "agents", view: "sessions" };
  }
}

function defaultViewForPrimary(primary: PrimaryTab): string {
  switch (primary) {
    case "agents":
      return "sessions";
    case "overview":
      return "overview";
    case "findings":
      return "findings";
    case "graph":
      return "graph";
    case "assets":
      return "assets";
    case "approvals":
      return "intercept";
  }
}

function taskProfileIDs(task: Task): string[] {
  if (task.llm_profile_ids && task.llm_profile_ids.length > 0) {
    return task.llm_profile_ids.map(String);
  }
  return task.llm_profile_id ? [String(task.llm_profile_id)] : [];
}

function TaskLLMControl({ task, profiles, onUpdated }: { task: Task; profiles: LLMProfile[]; onUpdated: () => void }) {
  const [open, setOpen] = React.useState(false);
  const [profileIDs, setProfileIDs] = React.useState<string[]>(() => taskProfileIDs(task));
  const [llmRotate, setLLMRotate] = React.useState(task.llm_rotate ?? false);
  const [activeProfileID, setActiveProfileID] = React.useState(
    task.active_llm_profile_id ? String(task.active_llm_profile_id) : (taskProfileIDs(task)[0] ?? ""),
  );
  const [saving, setSaving] = React.useState(false);
  const popoverContentRef = React.useRef<HTMLDivElement>(null);

  const chain = taskProfileIDs(task);
  const exhausted = task.llm_failover_state === "chain_exhausted";
  // The chain can be edited in any state, terminal included: after the task ends the main agent conversation still uses this chain,
  // and a broken model on it must be replaceable, or a completed task could no longer be interacted with.
  const terminal = ["done", "failed", "timeout"].includes(task.status);
  // A null active profile on an exhausted, non-empty chain is a persisted end
  // cursor. Keep the status display honest; choosing the first profile is only
  // the editor's reset draft and does not mean it is currently active.
  let activeID = chain[0] ?? "";
  if (exhausted) activeID = "";
  if (task.active_llm_profile_id) activeID = String(task.active_llm_profile_id);
  const activeProfile = profiles.find((profile) => profile.id === activeID);
  const currentLabel = exhausted
    ? t("Configuration chain exhausted")
    : task.llm_rotate
      ? t("Worker rotation ({count})", { count: chain.length })
      : (activeProfile?.name ?? (activeID ? t("Config #{id}", { id: activeID }) : t("Follow default config")));
  const activeIndex = chain.indexOf(activeID);
  const backupCount = activeIndex >= 0 ? Math.max(0, chain.length - activeIndex - 1) : 0;
  const currentTitle = [currentLabel, activeProfile?.model, backupCount > 0 ? t("{count} backups", { count: backupCount }) : ""]
    .filter(Boolean)
    .join(" · ");
  let editorDescription = t(
    "Rotation changes apply to new worker intents. Profile changes apply to subsequent LLM calls; in-flight calls keep their provider. Saving resets exhausted profiles.",
  );
  if (terminal) editorDescription = t("The task has ended; changes only affect subsequent main agent conversations.");
  let saveLabel = t("Save");
  if (exhausted) saveLabel = t("Save and reset");
  if (saving) saveLabel = t("Saving");

  const syncDraft = React.useCallback(() => {
    const next = taskProfileIDs(task);
    setLLMRotate(task.llm_rotate ?? false);
    setProfileIDs(next);
    setActiveProfileID(task.active_llm_profile_id ? String(task.active_llm_profile_id) : (next[0] ?? ""));
  }, [task]);

  const handleOpenChange = (next: boolean) => {
    setOpen(next);
    if (next) syncDraft();
  };

  const handleProfileIDsChange = (next: string[]) => {
    setProfileIDs(next);
    setActiveProfileID((current) => (next.includes(current) ? current : (next[0] ?? "")));
  };

  const save = async () => {
    setSaving(true);
    try {
      const result = await api.updateTaskLLMProfiles(
        task.id,
        profileIDs.map(Number),
        activeProfileID ? Number(activeProfileID) : undefined,
        llmRotate,
      );
      if (result.switch_event) {
        toast.info(result.switch_event.summary, { id: `task-${task.id}-llm-${result.switch_event.seq}` });
      } else {
        toast.success(
          result.reopened_intents > 0
            ? t("LLM configs updated and {count} quota-blocked intents reopened", { count: result.reopened_intents })
            : t("LLM configs updated"),
        );
      }
      setOpen(false);
      onUpdated();
    } catch (error) {
      toast.error(t("Update failed: {error}", { error: (error as Error).message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger asChild>
        <Button
          size="sm"
          variant={exhausted ? "destructive" : "outline"}
          aria-label={t("View or switch the task LLM config")}
          title={currentTitle}
        >
          <BrainIcon data-icon="inline-start" />
          <span className="hidden max-w-36 truncate lg:inline">{currentLabel}</span>
          {!task.llm_rotate && backupCount > 0 && <span className="hidden text-muted-foreground xl:inline">+{backupCount}</span>}
        </Button>
      </PopoverTrigger>
      <PopoverContent ref={popoverContentRef} align="start" className="w-[min(28rem,calc(100vw-2rem))] gap-4 p-4">
        <PopoverHeader>
          <PopoverTitle>
            {task.llm_rotate ? t("Task LLM config chain · Worker rotation on") : t("Task LLM config chain")}
          </PopoverTitle>
          <PopoverDescription>{editorDescription}</PopoverDescription>
        </PopoverHeader>

        {exhausted && (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>{t("Configuration chain quota exhausted")}</AlertTitle>
            <AlertDescription>
              {task.llm_failover_reason ??
                t("All selected configs were judged to be out of quota. Saving the chain resets the failure state.")}
            </AlertDescription>
          </Alert>
        )}

        <TaskLLMProfileChain
          profiles={profiles}
          value={profileIDs}
          rotate={llmRotate}
          onRotateChange={setLLMRotate}
          onValueChange={handleProfileIDsChange}
          activeProfileId={activeProfileID}
          onActiveProfileChange={setActiveProfileID}
          inputId="task-llm-profiles"
          disabled={saving}
          portalContainer={popoverContentRef}
        />

        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={() => setOpen(false)}>
            {t("Close")}
          </Button>
          <Button type="button" size="sm" onClick={save} disabled={saving}>
            {saving && <Spinner data-icon="inline-start" />}
            {saveLabel}
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  );
}

function TaskDetailInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  const [task, setTask] = React.useState<Task | null>(null);
  const [paused, setPaused] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const initialTab = searchParams.get("tab");
  const initialResolved = React.useMemo(() => resolveTabParam(initialTab), [initialTab]);
  const [primaryTab, setPrimaryTab] = React.useState<PrimaryTab>(initialResolved.primary);
  const [activeView, setActiveView] = React.useState<string>(initialResolved.view);
  const [goalExpanded, setGoalExpanded] = React.useState(false);
  const [interceptPendingCount, setInterceptPendingCount] = React.useState(0);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [archiving, setArchiving] = React.useState(false);

  React.useEffect(() => {
    const resolved = resolveTabParam(initialTab);
    setPrimaryTab(resolved.primary);
    setActiveView(resolved.view);
    setGoalExpanded(false);
  }, [id, initialTab]);

  const syncTabUrl = React.useCallback((view: string) => {
    const url = new URL(window.location.href);
    if (view === "sessions") url.searchParams.delete("tab");
    else url.searchParams.set("tab", view);
    window.history.replaceState(null, "", url);
  }, []);

  const handlePrimaryTabChange = React.useCallback(
    (next: string) => {
      const nextPrimary = next as PrimaryTab;
      const nextView = defaultViewForPrimary(nextPrimary);
      setPrimaryTab(nextPrimary);
      setActiveView(nextView);
      syncTabUrl(nextView);
    },
    [syncTabUrl],
  );

  const handleSubViewChange = React.useCallback(
    (nextView: string) => {
      const resolved = resolveTabParam(nextView);
      setPrimaryTab(resolved.primary);
      setActiveView(resolved.view);
      syncTabUrl(resolved.view);
    },
    [syncTabUrl],
  );

  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
  }, []);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(id)
        .then((rows) => {
          if (alive) setInterceptPendingCount(rows.filter((r) => r.status === "pending").length);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const stopPolling = setVisibleInterval(load, 5000, { topics: ["intercept"] });
    return () => {
      alive = false;
      stopPolling();
    };
  }, [id]);

  const taskLoadInFlight = React.useRef<string | null>(null);
  React.useEffect(() => {
    setTask(null);
    setLoaded(false);
    setPaused(false);
    setInterceptPendingCount(0);
    taskLoadInFlight.current = null;
  }, [id]);
  const load = React.useCallback(() => {
    if (!id) {
      setLoaded(true);
      return Promise.resolve();
    }
    if (taskLoadInFlight.current === id) return Promise.resolve();
    taskLoadInFlight.current = id;
    return Promise.all([api.task(id), api.stats(id).catch(() => null)])
      .then(([task, s]) => {
        if (taskLoadInFlight.current !== id) return;
        const base = { ...task };
        const at = s?.active_task;
        if (at) {
          base.in_flight = at.in_flight;
          base.goals_total = at.goals_total;
          base.goals_met = at.goals_met;
          base.engine_mode = s?.engine_mode ?? at.engine_mode;
          base.paused = at.paused;
        }
        setTask(base);
        setPaused(at?.paused ?? base.paused ?? false);
      })
      .catch(() => {
        // Keep the last rendered task state during a transient poll failure.
      })
      .finally(() => {
        if (taskLoadInFlight.current === id) {
          taskLoadInFlight.current = null;
          setLoaded(true);
        }
      });
  }, [id]);
  React.useEffect(() => {
    void load();
    const stopPolling = setVisibleInterval(load, 5000, { topics: ["tasks", "stats"] });
    return stopPolling;
  }, [load]);

  async function togglePause() {
    if (task && ["done", "failed", "timeout"].includes(task.status)) return;
    const next = !paused;
    try {
      await api.controlTask(id, next ? "pause" : "resume");
      setPaused(next);
      toast.success(next ? t("Exploration paused") : t("Exploration resumed"));
    } catch (e) {
      toast.error(t("Operation failed: {error}", { error: (e as Error).message }));
    }
  }

  async function archiveTask() {
    if (!task || archiving) return;
    setArchiving(true);
    try {
      await api.archiveTask(task.id);
      toast.success(t("Task queued for archiving"));
      router.push("/function/tasks");
    } catch (error) {
      toast.error(t("Archive failed: {error}", { error: (error as Error).message }));
      setArchiving(false);
    }
  }

  if (!task) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? t("Task {id} was deleted, archived, or does not exist", { id }) : t("Loading…")}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/function/tasks">
              <ArrowLeftIcon /> {t("Back to task list")}
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const completed = task.status === "done";
  const terminal = ["done", "failed", "timeout"].includes(task.status);
  const archiveLifecycleEligible = terminal || paused || task.status === "paused";
  const canArchive = archiveLifecycleEligible && !task.archive_blocked_by_task_id;
  let archiveDisabledReason = task.queued ? t("Queued tasks must be paused first") : t("Running tasks must be paused first");
  if (archiveLifecycleEligible && task.archive_blocked_by_task_id) {
    archiveDisabledReason = t(
      "The task is directly inherited by unarchived task #{id}; archive the dependent task first",
      { id: task.archive_blocked_by_task_id },
    );
  }
  const engineMode = paused ? "paused" : (task.engine_mode ?? "idle");
  let controlVariant: "default" | "secondary" | "outline" = "outline";
  let controlIcon = <PauseIcon data-icon="inline-start" />;
  let controlLabel = t("Pause");
  if (terminal) {
    controlVariant = "secondary";
    controlIcon = <CheckIcon data-icon="inline-start" />;
    controlLabel = completed ? t("Completed") : t("Ended");
  } else if (paused) {
    controlVariant = "default";
    controlIcon = <PlayIcon data-icon="inline-start" />;
    controlLabel = t("Resume");
  }
  const archiveTrigger = (
    <Button
      size="icon-sm"
      variant="ghost"
      disabled={!canArchive || archiving}
      aria-label={canArchive ? t("Archive task") : archiveDisabledReason}
    >
      {archiving ? <Spinner /> : <ArchiveIcon />}
    </Button>
  );

  const findingsCount = task.findings
    ? task.findings.critical + task.findings.high + task.findings.medium + task.findings.low
    : 0;

  const subTabs = SUB_TABS[primaryTab];

  return (
    <Tabs value={primaryTab} onValueChange={handlePrimaryTabChange} className="flex flex-1 flex-col gap-0">
      {/* Top fixed area */}
      <header className="sticky top-0 z-10 flex flex-col gap-2 border-b bg-background/95 px-4 py-2.5 backdrop-blur lg:px-6">
        <div className="flex items-center gap-2">
          <SidebarTrigger className="-ml-1" />
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/function/tasks">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <h1 className="min-w-0 flex-1 truncate text-sm font-semibold" title={task.name || task.description}>
            {task.name || task.description}
          </h1>
          <code className="hidden rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground sm:inline">
            {task.id}
          </code>
          <Separator orientation="vertical" className="mx-1 hidden h-4 sm:block" />
          <TaskLLMControl task={task} profiles={profiles} onUpdated={load} />
          <StatusBadge domain={terminal ? "task" : "engine"} value={terminal ? task.status : engineMode} dot />
          {canArchive ? (
            <AlertDialog>
              <AlertDialogTrigger asChild>{archiveTrigger}</AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{t("Archive task #{id}?", { id: task.id })}</AlertDialogTitle>
                  <AlertDialogDescription>
                    {t(
                      "The task graph, related records, exclusive assets and traffic, working files, and LLM history will be compressed into cold storage. Once archiving completes, restore it from the \"Archived\" tab of the task list.",
                    )}
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{t("Cancel")}</AlertDialogCancel>
                  <AlertDialogAction onClick={() => void archiveTask()}>{t("Confirm archive")}</AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          ) : (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="inline-flex">{archiveTrigger}</span>
              </TooltipTrigger>
              <TooltipContent>{archiveDisabledReason}</TooltipContent>
            </Tooltip>
          )}
          <Button size="sm" variant={controlVariant} onClick={togglePause} disabled={terminal}>
            {controlIcon}
            {controlLabel}
          </Button>
        </div>
        {task.goal && (
          <button
            type="button"
            onClick={() => setGoalExpanded((v) => !v)}
            aria-expanded={goalExpanded}
            className={
              goalExpanded
                ? "text-left text-xs whitespace-pre-wrap text-muted-foreground hover:text-foreground"
                : "truncate text-left text-xs text-muted-foreground hover:text-foreground"
            }
            title={goalExpanded ? t("Collapse") : t("Expand")}
          >
            {task.goal}
          </button>
        )}
        {/* Primary tabs + optional compact sub-view switcher */}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="scroll-fade-x min-w-0">
            <TabsList variant="line" className="min-w-max items-center">
              {PRIMARY_TABS.map((tabItem) => {
                const Icon = tabItem.icon;
                return (
                  <TabsTrigger
                    key={tabItem.value}
                    value={tabItem.value}
                    className="gap-1.5 px-2.5"
                    onClick={(e) => e.currentTarget.scrollIntoView({ block: "nearest", inline: "nearest" })}
                  >
                    <Icon className="size-3.5" />
                    {tabItem.label}
                    {tabItem.value === "findings" && findingsCount > 0 && (
                      <span className="rounded-full bg-muted px-1.5 py-0.2 text-[10px] font-medium tabular-nums text-muted-foreground">
                        {findingsCount}
                      </span>
                    )}
                    {tabItem.value === "approvals" && interceptPendingCount > 0 && (
                      <span className="ml-0.5 inline-flex h-4 min-w-[16px] items-center justify-center rounded-full bg-amber-500 px-1 text-[10px] font-semibold leading-none text-white">
                        {interceptPendingCount > 99 ? "99+" : interceptPendingCount}
                      </span>
                    )}
                  </TabsTrigger>
                );
              })}
            </TabsList>
          </div>
          {subTabs && subTabs.length > 1 && (
            <div className="inline-flex items-center gap-0.5 rounded-lg bg-muted/60 p-0.5 text-xs">
              {subTabs.map((item) => {
                const SubIcon = item.icon;
                const active = activeView === item.value;
                return (
                  <button
                    key={item.value}
                    type="button"
                    onClick={() => handleSubViewChange(item.value)}
                    className={
                      active
                        ? "inline-flex items-center gap-1.5 rounded-md bg-background px-2.5 py-1 font-medium text-foreground shadow-2xs transition-colors"
                        : "inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-muted-foreground transition-colors hover:text-foreground"
                    }
                  >
                    <SubIcon className="size-3.5" />
                    {item.label}
                  </button>
                );
              })}
            </div>
          )}
        </div>
      </header>

      {/* Tab content */}
      <div className="flex-1 p-4 lg:p-6">
        <TabsContent value="agents" className="mt-0">
          {activeView === "broadcast" ? (
            <BroadcastTab taskId={id} />
          ) : (
            <SessionsTab taskId={id} taskPaused={task?.status === "paused"} />
          )}
        </TabsContent>
        <TabsContent value="overview" className="mt-0">
          <OverviewTab taskId={id} />
        </TabsContent>
        <TabsContent value="findings" className="mt-0">
          {activeView === "retests" ? (
            <RetestsTab key={id} taskId={id} />
          ) : activeView === "report" ? (
            <ReportTab taskId={id} />
          ) : (
            <FindingsTab taskId={id} />
          )}
        </TabsContent>
        <TabsContent value="graph" className="mt-0">
          {activeView === "coverage" ? (
            <CoverageGraphTab taskId={id} coverageEnabled={task?.coverage_enabled !== false} />
          ) : (
            <GraphTab taskId={id} />
          )}
        </TabsContent>
        <TabsContent value="assets" className="mt-0">
          {activeView === "credentials" ? (
            <CredentialsTab taskId={id} />
          ) : activeView === "scope" ? (
            <TaskScopeRulesSection taskId={id} />
          ) : (
            <AssetsTab taskId={id} />
          )}
        </TabsContent>
        <TabsContent value="approvals" className="mt-0">
          <InterceptTab taskId={id} />
        </TabsContent>
      </div>
    </Tabs>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function TaskDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <TaskDetailInner />
    </React.Suspense>
  );
}
