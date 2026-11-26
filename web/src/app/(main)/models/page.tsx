"use client";

import * as React from "react";

import {
  BrainIcon,
  Loader2Icon,
  PencilIcon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import type { LLMPoolMember, LLMPoolStatus, LLMProfile, OAuthProviderStatus, OllamaStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

import { FallbackStudio } from "./_components/fallback-studio";
import {
  formatContextK,
  formatTokensShort,
  getProviders,
  healthOf,
  type ProfileFormat,
  providerConnectionState,
  providerMeta,
  reasoningSummary,
} from "./_components/llm-helpers";
import { ProfileEditorSheet } from "./_components/profile-editor";
import { ProviderIcon } from "./_components/provider-icons";
import { RetryPolicyPanel } from "./_components/retry-panel";

export default function ModelsPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [oauthList, setOauthList] = React.useState<OAuthProviderStatus[]>([]);
  const [ollamaStatus, setOllamaStatus] = React.useState<OllamaStatus | null>(null);
  const [verifying, setVerifying] = React.useState(false);
  const [tab, setTab] = React.useState("profiles");
  const [providerFilter, setProviderFilter] = React.useState<"all" | ProfileFormat>("all");
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const [testingId, setTestingId] = React.useState<string | null>(null);

  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* ignore */
    }
  }, []);

  const loadOAuth = React.useCallback(async (opts?: { verify?: boolean; force?: boolean }) => {
    try {
      const res = await api.llmOAuthStatus(opts);
      setOauthList(res.providers);
      if (res.ollama) setOllamaStatus(res.ollama);
    } catch {
      /* ignore */
    }
  }, []);

  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
    } catch {
      /* ignore */
    }
    await Promise.all([loadPool(), loadOAuth({ verify: true })]);
  }, [loadPool, loadOAuth]);

  const verifyConnections = React.useCallback(async () => {
    if (verifying) return;
    setVerifying(true);
    try {
      await Promise.all([loadPool(), loadOAuth({ verify: true, force: true })]);
    } finally {
      setVerifying(false);
    }
  }, [verifying, loadPool, loadOAuth]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const oauthMap = React.useMemo(() => {
    const m = new Map<string, OAuthProviderStatus>();
    for (const s of oauthList) m.set(s.provider, s);
    return m;
  }, [oauthList]);

  const healthMap = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  const activeProfile = React.useMemo(() => profiles.find((p) => p.is_default) ?? null, [profiles]);
  const poolOn = pool?.enabled ?? false;
  const activeChainCount = (pool?.chain ?? []).filter((m) => m.active || !m.excluded).length;

  const filteredProfiles = React.useMemo(() => {
    if (providerFilter === "all") return profiles;
    return profiles.filter((p) => p.format === providerFilter);
  }, [profiles, providerFilter]);

  React.useEffect(() => {
    if (providerFilter !== "all" && !profiles.some((p) => p.format === providerFilter)) {
      setProviderFilter("all");
    }
  }, [profiles, providerFilter]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(t("Activated: {name}", { name }));
      await load();
    } catch (e) {
      toast.error(t("Activation failed: {error}", { error: (e as Error).message }));
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error(t("Cannot delete the active profile"));
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(t("Deleted: {name}", { name: p.name }));
      await load();
    } catch (e) {
      toast.error(t("Delete failed: {error}", { error: (e as Error).message }));
    }
  }

  async function quickTest(p: LLMProfile) {
    if (testingId) return;
    setTestingId(p.id);
    try {
      const r = await api.testLLM(
        p.format,
        p.model,
        p.base_url ?? "",
        "",
        p.proxy ?? "",
        p.thinking_type ?? "",
        p.reasoning_effort ?? "",
        Number(p.id),
        p.streaming ?? true,
        p.session_header_key ?? "",
        p.user_agent ?? "",
      );
      if (r.ok) {
        toast.success(t("Connected · {latency}ms · {model}", { latency: r.latency_ms ?? "?", model: r.model ?? p.model }), {
          description: r.reply ? t("Reply: {reply}", { reply: r.reply }) : undefined,
        });
      } else {
        toast.error(t("Connection failed: {error}", { error: r.error ?? t("unknown") }));
      }
    } catch (e) {
      toast.error(t("Test error: {error}", { error: (e as Error).message }));
    } finally {
      setTestingId(null);
    }
  }

  const activeHealth = activeProfile ? healthOf(activeProfile, healthMap.get(activeProfile.id), oauthMap) : null;
  const activeReasoning = activeProfile ? reasoningSummary(activeProfile) : null;

  return (
    <div className="flex flex-1 flex-col gap-5 animate-in fade-in-0 duration-200">
      {/* Header & Hero Summary */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">{t("Models & Fallbacks")}</h1>
          <p className="text-muted-foreground text-sm">
            {t("Configure AI providers, models, reasoning levels, and automatic failover chains.")}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => void verifyConnections()} disabled={verifying}>
            <RefreshCwIcon className={cn("size-3.5", verifying && "animate-spin")} />
            {verifying ? t("Checking…") : t("Check connections")}
          </Button>
          <Button size="sm" onClick={() => openEditor(null)}>
            <PlusIcon /> {t("New provider")}
          </Button>
        </div>
      </div>

      <div className="grid gap-3 md:grid-cols-3">
        <div className="flex items-center gap-3 rounded-xl border bg-card px-4 py-3 md:col-span-2">
          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-amber-500/10 text-amber-500">
            <StarIcon className="size-4 fill-amber-400 text-amber-400" />
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-muted-foreground text-xs">{t("Active model")}</span>
              {activeProfile && (
                <Badge variant="outline" className="gap-1 text-[10px] uppercase" lang="en">
                  <ProviderIcon format={activeProfile.format} className="size-3" />
                  {providerMeta(activeProfile.format).shortLabel}
                </Badge>
              )}
              {activeReasoning && (
                <Badge variant="secondary" className="gap-1 text-[10px]">
                  <BrainIcon className="size-2.5" /> {activeReasoning}
                </Badge>
              )}
              {activeHealth && (
                <Badge variant="outline" className={cn("ml-auto text-[10px]", activeHealth.cls)} title={activeHealth.hint}>
                  <span className={cn("mr-1 inline-block size-1.5 rounded-full", activeHealth.dotCls)} />
                  {activeHealth.label}
                </Badge>
              )}
            </div>
            {activeProfile ? (
              <div className="mt-0.5 flex flex-wrap items-baseline gap-x-2">
                <span className="truncate font-medium text-sm">{activeProfile.name}</span>
                <code className="truncate font-mono text-muted-foreground text-xs">{activeProfile.model}</code>
              </div>
            ) : (
              <p className="mt-0.5 text-muted-foreground text-sm">{t("No active profile configured")}</p>
            )}
          </div>
        </div>

        <button
          type="button"
          onClick={() => setTab("fallbacks")}
          className={cn(
            "flex items-center gap-3 rounded-xl border px-4 py-3 text-left transition-all duration-200 hover:border-foreground/25",
            poolOn ? "border-emerald-500/35 bg-emerald-500/[0.04]" : "bg-card",
          )}
        >
          <div
            className={cn(
              "flex size-9 shrink-0 items-center justify-center rounded-lg",
              poolOn ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400" : "bg-muted text-muted-foreground",
            )}
          >
            <ZapIcon className="size-4" />
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <span className="font-medium text-sm">{t("Fallbacks & Pool")}</span>
              <Badge
                variant="outline"
                className={cn(
                  "ml-auto text-[10px]",
                  poolOn
                    ? "border-emerald-500/50 text-emerald-600 dark:text-emerald-400"
                    : "text-muted-foreground",
                )}
              >
                {poolOn ? t("On") : t("Off")}
              </Badge>
            </div>
            <p className="mt-0.5 truncate text-muted-foreground text-xs">
              {t("{count} profile(s) in chain", { count: activeChainCount })}
            </p>
          </div>
        </button>
      </div>

      {/* Main Tabs with native sliding underline */}
      <Tabs value={tab} onValueChange={setTab} className="flex-1">
        <TabsList variant="line">
          <TabsTrigger value="profiles">{t("Providers & Models")}</TabsTrigger>
          <TabsTrigger value="fallbacks" className="gap-1.5">
            {t("Fallbacks & Pool")}
            {poolOn && <span className="inline-block size-1.5 rounded-full bg-emerald-500" />}
          </TabsTrigger>
          <TabsTrigger value="retry">{t("Retry & backoff")}</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4 grid gap-4 animate-in fade-in-0 slide-in-from-bottom-1 duration-200">
          {/* Provider filter pills */}
          {profiles.length > 0 && (
            <div className="flex flex-wrap items-center gap-1.5">
              <button
                type="button"
                onClick={() => setProviderFilter("all")}
                className={cn(
                  "rounded-full border px-3 py-1 text-xs font-medium transition-colors",
                  providerFilter === "all"
                    ? "border-primary bg-primary text-primary-foreground"
                    : "border-border bg-card text-muted-foreground hover:text-foreground",
                )}
              >
                {t("All")} ({profiles.length})
              </button>
              {getProviders().map((prov) => {
                const count = profiles.filter((p) => p.format === prov.id).length;
                if (count === 0) return null;
                const active = providerFilter === prov.id;
                const conn = providerConnectionState(prov, profiles, healthMap, oauthMap, ollamaStatus);
                return (
                  <button
                    key={prov.id}
                    type="button"
                    title={`${prov.shortLabel} — ${conn.label}: ${conn.hint}`}
                    onClick={() => setProviderFilter(prov.id)}
                    className={cn(
                      "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition-colors",
                      active
                        ? "border-primary bg-primary text-primary-foreground"
                        : "border-border bg-card text-muted-foreground hover:text-foreground",
                    )}
                  >
                    <span className={cn("inline-block size-1.5 rounded-full", conn.dotCls)} />
                    <ProviderIcon format={prov.id} className="size-3.5" />
                    {prov.shortLabel} ({count})
                  </button>
                );
              })}
            </div>
          )}

          {/* Profile cards grid */}
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {filteredProfiles.map((p) => {
              const h = healthOf(p, healthMap.get(p.id), oauthMap);
              const reasoning = reasoningSummary(p);
              const isTesting = testingId === p.id;
              return (
                // biome-ignore lint/a11y/useSemanticElements: card contains action buttons; native <button> would nest buttons
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-all duration-200 hover:-translate-y-0.5 hover:border-foreground/30 hover:shadow-sm",
                    p.is_default && "border-amber-400/50 bg-amber-400/[0.04]",
                  )}
                >
                  <CardContent className="grid gap-3 px-4">
                    <div className="flex items-start gap-2.5">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0 transition-transform",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="gap-1 text-[10px] uppercase" lang="en">
                            <ProviderIcon format={p.format} className="size-3" />
                            {providerMeta(p.format).shortLabel}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto text-[10px]", h.cls)} title={h.hint}>
                            <span className={cn("mr-1 inline-block size-1.5 rounded-full", h.dotCls)} />
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap items-center gap-1.5 pl-6.5 text-muted-foreground text-xs">
                      {reasoning && (
                        <Badge variant="secondary" className="gap-1 font-normal text-[11px]">
                          <BrainIcon className="size-3" /> {reasoning}
                        </Badge>
                      )}
                      {(p.context_window_k ?? 0) > 0 && (
                        <Badge variant="outline" className="font-mono font-normal text-[11px]">
                          {formatContextK(p.context_window_k)}
                        </Badge>
                      )}
                      {(p.max_tokens ?? 0) > 0 && (
                        <Badge variant="outline" className="font-mono font-normal text-[11px]">
                          {t("{n} out", { n: formatTokensShort(p.max_tokens ?? 0) })}
                        </Badge>
                      )}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? (
                          <Badge variant="outline" className="font-normal text-[11px] opacity-75">
                            {t("Excluded from pool")}
                          </Badge>
                        ) : (
                          <Badge variant="outline" className="font-normal text-[11px]">
                            {t("Priority {priority}", { priority: p.priority ?? 0 })}
                          </Badge>
                        ))}
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6.5 text-muted-foreground text-xs">
                      {p.api_key_hint && <span className="font-mono">{p.api_key_hint}</span>}
                      <span>
                        {t("{rps}/s · {rpm}/min", { rps: p.rate_per_second, rpm: p.rate_per_minute })}
                      </span>
                      {p.proxy && <span className="truncate">{t("Proxy {proxy}", { proxy: p.proxy })}</span>}
                    </div>

                    <div className="mt-0.5 flex gap-1.5 border-t pt-3">
                      <Button
                        size="sm"
                        variant={p.is_default ? "secondary" : "outline"}
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? t("Active") : t("Set as active")}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label={t("Test connection")}
                        title={t("Test connection")}
                        disabled={isTesting}
                        onClick={(e) => {
                          e.stopPropagation();
                          void quickTest(p);
                        }}
                      >
                        {isTesting ? <Loader2Icon className="size-3.5 animate-spin" /> : <PlugZapIcon className="size-3.5" />}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label={t("Edit")}
                        title={t("Edit")}
                        onClick={(e) => {
                          e.stopPropagation();
                          openEditor(p);
                        }}
                      >
                        <PencilIcon className="size-3.5" />
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label={t("Delete profile")}
                        title={t("Delete profile")}
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="size-3.5 text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}

            {filteredProfiles.length === 0 && (
              <div className="col-span-full rounded-xl border border-dashed p-10 text-center text-muted-foreground text-sm">
                {profiles.length === 0
                  ? t('No model profiles yet — click "New provider" in the top right to add the first one.')
                  : t("No profiles match this provider filter.")}
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="fallbacks" className="mt-4">
          <FallbackStudio profiles={profiles} pool={pool} onReload={load} onEditProfile={openEditor} />
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileEditorSheet
        profile={editing}
        open={editOpen}
        onOpenChange={setEditOpen}
        onSaved={() => void load()}
        oauthMap={oauthMap}
        onReloadOAuth={() => loadOAuth({ verify: true, force: true })}
        profiles={profiles}
        healthMap={healthMap}
        ollamaStatus={ollamaStatus}
      />
    </div>
  );
}
