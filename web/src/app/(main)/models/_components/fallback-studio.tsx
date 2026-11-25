"use client";

import * as React from "react";

import Link from "next/link";

import {
  ArrowDownIcon,
  ArrowUpIcon,
  EyeIcon,
  EyeOffIcon,
  RotateCcwIcon,
  ShieldCheckIcon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import { setVisibleInterval } from "@/lib/polling";
import type { LLMPoolStatus, LLMProfile } from "@/lib/types";
import { cn } from "@/lib/utils";

import { cooldownText, profileSavePayload, rich } from "./llm-helpers";

export function FallbackStudio({
  profiles,
  pool,
  onReload,
  onEditProfile,
}: {
  profiles: LLMProfile[];
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
  onEditProfile: (p: LLMProfile) => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [updatingId, setUpdatingId] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    return setVisibleInterval(() => void onReload(), 10_000);
  }, [pool, onReload]);

  const profileMap = React.useMemo(() => {
    const map = new Map<string, LLMProfile>();
    for (const p of profiles) map.set(p.id, p);
    return map;
  }, [profiles]);

  async function toggleSetting(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? t("LLM pool enabled") : t("LLM pool disabled"));
      } else {
        toast.success(t("Fallback settings updated"));
      }
    } catch (e) {
      toast.error(t("Failed to apply settings: {error}", { error: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? t("Profile recovered") : t("All profiles recovered"));
    } catch (e) {
      toast.error(t("Recovery failed: {error}", { error: (e as Error).message }));
    }
  }

  async function updateProfilePool(profileId: string, patch: { priority?: number; pool_exclude?: boolean }) {
    const p = profileMap.get(profileId);
    if (!p || updatingId) return;
    setUpdatingId(profileId);
    try {
      await api.saveLLMProfile(profileSavePayload(p, patch));
      await onReload();
      toast.success(t("Fallback settings updated"));
    } catch (e) {
      toast.error(t("Save failed: {error}", { error: (e as Error).message }));
    } finally {
      setUpdatingId(null);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const excludedList = chain.filter((m) => m.excluded && !m.active);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <div className="grid gap-5 animate-in fade-in-0 slide-in-from-bottom-1 duration-200">
      {/* Control switches */}
      <div className="grid gap-3 md:grid-cols-2">
        <div
          className={cn(
            "flex items-start justify-between gap-4 rounded-xl border p-4 transition-colors",
            enabled ? "border-emerald-500/35 bg-emerald-500/[0.03]" : "bg-card",
          )}
        >
          <div className="grid gap-1">
            <div className="flex items-center gap-2">
              <ZapIcon className={cn("size-4", enabled ? "text-emerald-500" : "text-muted-foreground")} />
              <Label className="text-sm font-medium">{t("Enable pool")}</Label>
            </div>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {rich(
                t(
                  "When enabled, an Agent <b>without a pinned model</b> automatically fails over to the next config when the current one is unusable (insufficient balance / dead Key / rate limiting / service error).",
                ),
              )}
            </p>
          </div>
          <Switch
            checked={enabled}
            disabled={busy}
            onCheckedChange={(v) => void toggleSetting({ llm_pool_enabled: v })}
            aria-label={t("LLM pool toggle")}
          />
        </div>

        <div
          className={cn(
            "flex items-start justify-between gap-4 rounded-xl border bg-card p-4 transition-opacity",
            !enabled && "opacity-60",
          )}
        >
          <div className="grid gap-1">
            <div className="flex items-center gap-2">
              <ShieldCheckIcon className="size-4 text-muted-foreground" />
              <Label className="text-sm font-medium">{t("Also fall back when a pinned config fails")}</Label>
            </div>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {t(
                "Off by default: when an Agent or task pins a config, only that config is used and a failure stays a failure (no silent switch to another model). When enabled, a failing pinned config also falls back to the pool chain below.",
              )}
            </p>
          </div>
          <Switch
            checked={pool?.bind_fallback ?? false}
            disabled={busy || !enabled}
            onCheckedChange={(v) => void toggleSetting({ llm_pool_bind_fallback: v })}
            aria-label={t("Bound-config failure fallback toggle")}
          />
        </div>
      </div>

      {/* Failover pipeline */}
      <div className="grid gap-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="font-medium text-sm">{t("Pool order")}</h2>
            <p className="text-muted-foreground text-xs">
              {t("Adjust priority with arrows to control the exact failover sequence.")}
            </p>
          </div>
          {tripped.length > 0 && (
            <Button size="sm" variant="outline" onClick={() => void recover()}>
              <RotateCcwIcon className="size-3.5" /> {t("Recover all")}
            </Button>
          )}
        </div>

        {inChain.length < 2 && (
          <div className="rounded-xl border border-amber-500/30 bg-amber-500/5 px-4 py-3 text-amber-700 text-xs dark:text-amber-300">
            {t(
              "Only {count} usable profile(s) right now, so the pool has no effect — at least 2 usable profiles (API Key set, or Ollama) with pool participation are required.",
              { count: inChain.length },
            )}
          </div>
        )}

        <div className="grid gap-0">
          {inChain.map((m, idx) => {
            const prof = profileMap.get(m.profile_id);
            const isUpdating = updatingId === m.profile_id;
            return (
              <React.Fragment key={m.profile_id}>
                <div
                  className={cn(
                    "group flex flex-wrap items-center gap-3 rounded-xl border bg-card p-3.5 transition-all duration-200 hover:border-foreground/25",
                    m.active && "border-amber-400/50 bg-amber-400/[0.04]",
                    m.state === "tripped" && "border-destructive/45 bg-destructive/[0.03]",
                  )}
                >
                  <div
                    className={cn(
                      "flex size-7 shrink-0 items-center justify-center rounded-full font-mono font-semibold text-xs",
                      m.active
                        ? "bg-amber-500/15 text-amber-600 dark:text-amber-400"
                        : "bg-muted text-muted-foreground",
                    )}
                  >
                    {idx + 1}
                  </div>

                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <button
                        type="button"
                        className="truncate font-medium text-sm hover:underline"
                        onClick={() => prof && onEditProfile(prof)}
                      >
                        {m.name}
                      </button>
                      {m.active && (
                        <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                          {t("Active")}
                        </Badge>
                      )}
                      {m.state === "ok" && (
                        <Badge variant="outline" className="border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                          {t("Healthy")}
                        </Badge>
                      )}
                      {m.state === "degraded" && (
                        <Badge variant="outline" className="border-amber-500/50 text-amber-600 dark:text-amber-400">
                          {t("{count} consecutive failures", { count: m.fails })}
                        </Badge>
                      )}
                      {m.state === "tripped" && (
                        <Badge variant="outline" className="border-destructive/50 text-destructive">
                          {m.cooldown_secs > 0
                            ? t("Tripped · {cooldown}", { cooldown: cooldownText(m.cooldown_secs) })
                            : t("Tripped")}
                        </Badge>
                      )}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-muted-foreground text-xs">
                      <code className="truncate font-mono">{m.model}</code>
                      {!m.active && <span>{t("Priority {priority}", { priority: m.priority })}</span>}
                    </div>
                    {m.last_error && (
                      <p className="mt-1 truncate font-mono text-destructive/80 text-xs" title={m.last_error}>
                        {m.last_error}
                      </p>
                    )}
                  </div>

                  <div className="flex items-center gap-1">
                    {m.state !== "ok" && (
                      <Button
                        size="icon"
                        variant="ghost"
                        className="size-8"
                        aria-label={t("Recover now")}
                        title={t("Recover now: clears the breaker so the next call retries this profile")}
                        onClick={() => void recover(m.profile_id)}
                      >
                        <RotateCcwIcon className="size-3.5" />
                      </Button>
                    )}
                    {!m.active && prof && (
                      <>
                        <Button
                          size="icon"
                          variant="ghost"
                          className="size-8"
                          disabled={isUpdating || idx <= 1}
                          aria-label={t("Increase priority")}
                          title={t("Increase priority")}
                          onClick={() => {
                            const above = inChain[idx - 1];
                            if (!above) return;
                            void updateProfilePool(m.profile_id, { priority: (above.priority ?? 0) + 1 });
                          }}
                        >
                          <ArrowUpIcon className="size-3.5" />
                        </Button>
                        <Button
                          size="icon"
                          variant="ghost"
                          className="size-8"
                          disabled={isUpdating || idx >= inChain.length - 1}
                          aria-label={t("Decrease priority")}
                          title={t("Decrease priority")}
                          onClick={() => {
                            const below = inChain[idx + 1];
                            if (!below) return;
                            void updateProfilePool(m.profile_id, { priority: (below.priority ?? 0) - 1 });
                          }}
                        >
                          <ArrowDownIcon className="size-3.5" />
                        </Button>
                        <Button
                          size="icon"
                          variant="ghost"
                          className="size-8 text-muted-foreground hover:text-foreground"
                          disabled={isUpdating}
                          aria-label={t("Exclude from pool")}
                          title={t("Exclude from pool")}
                          onClick={() => void updateProfilePool(m.profile_id, { pool_exclude: true })}
                        >
                          <EyeOffIcon className="size-3.5" />
                        </Button>
                      </>
                    )}
                  </div>
                </div>
                {idx < inChain.length - 1 && (
                  <div className="flex items-center pl-6.5 py-0.5">
                    <div className="h-4 w-px bg-border" />
                  </div>
                )}
              </React.Fragment>
            );
          })}
          {inChain.length === 0 && (
            <div className="rounded-xl border border-dashed p-8 text-center text-muted-foreground text-sm">
              {t("No profiles yet")}
            </div>
          )}
        </div>
      </div>

      {/* Excluded profiles */}
      {excludedList.length > 0 && (
        <div className="grid gap-2">
          <h3 className="font-medium text-muted-foreground text-xs uppercase tracking-wider">
            {t("Excluded from pool")}
          </h3>
          <div className="grid gap-2 sm:grid-cols-2">
            {excludedList.map((m) => (
              <div
                key={m.profile_id}
                className="flex items-center justify-between gap-3 rounded-lg border border-dashed px-3.5 py-2.5 text-sm opacity-80 transition-opacity hover:opacity-100"
              >
                <div className="min-w-0">
                  <div className="truncate font-medium">{m.name}</div>
                  <code className="block truncate font-mono text-muted-foreground text-xs">{m.model}</code>
                </div>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={updatingId === m.profile_id}
                  onClick={() => void updateProfilePool(m.profile_id, { pool_exclude: false })}
                >
                  <EyeIcon className="size-3.5" /> {t("Include")}
                </Button>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Footer note */}
      <div className="rounded-xl border border-dashed bg-muted/20 p-3.5 text-muted-foreground text-xs leading-relaxed">
        <p>
          {t(
            "The active profile is always first; the rest follow priority from high to low (set per profile). After a failure a profile enters a cooldown (60s → 5min → 30min), is skipped while cooling down, and switches back automatically once recovered. Profiles whose context window cannot fit the current request are skipped. Agents and tasks with a pinned model do not join the pool by default.",
          )}
        </p>
        <p className="mt-1.5">
          {t("Task-specific model chains and multi-LLM worker rotation are configured per task in")}{" "}
          <Link href="/function/tasks" className="font-medium text-foreground underline underline-offset-2">
            {t("Tasks")}
          </Link>
          .
        </p>
      </div>
    </div>
  );
}
