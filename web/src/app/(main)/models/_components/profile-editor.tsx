"use client";

import * as React from "react";

import {
  CheckIcon,
  ChevronDownIcon,
  ExternalLinkIcon,
  KeyRoundIcon,
  Loader2Icon,
  LogInIcon,
  LogOutIcon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  SaveIcon,
  SlidersHorizontalIcon,
  SparklesIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import type { LLMPoolMember, LLMProfile, LLMRetryOverride, ModelDetail, OAuthProviderStatus, OllamaStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

import {
  CONTEXT_TOKEN_PRESETS,
  formatTokensShort,
  fromStore,
  getEffortLevels,
  getMaxTokensFieldHint,
  getMaxTokensFields,
  getProviders,
  getThinkingTypes,
  NONE,
  normalizeFormat,
  OUTPUT_TOKEN_PRESETS,
  type ProfileFormat,
  providerConnectionState,
  providerMeta,
  rich,
  toStore,
} from "./llm-helpers";
import { ProviderIcon } from "./provider-icons";
import { ProfileRetryFields, ZERO_OVERRIDE } from "./retry-panel";

function StepBadge({ step, title }: { step: number; title: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-primary/10 font-mono font-semibold text-primary text-xs">
        {step}
      </span>
      <h3 className="font-medium text-sm tracking-tight">{title}</h3>
    </div>
  );
}

export function ProfileEditorSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
  oauthMap,
  onReloadOAuth,
  profiles = [],
  healthMap = new Map(),
  ollamaStatus = null,
}: {
  profile: LLMProfile | null;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
  oauthMap: Map<string, OAuthProviderStatus>;
  onReloadOAuth: () => Promise<void>;
  profiles?: LLMProfile[];
  healthMap?: Map<string, LLMPoolMember>;
  ollamaStatus?: OllamaStatus | null;
}) {
  const isNew = !profile;
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<ProfileFormat>("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0");
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0");
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true);
  const [maxTokens, setMaxTokens] = React.useState("0");
  const [maxTokensField, setMaxTokensField] = React.useState(NONE);
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState("");
  const [userAgent, setUserAgent] = React.useState("");
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE);
  const [advancedOpen, setAdvancedOpen] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<string[]>([]);
  const [modelDetails, setModelDetails] = React.useState<Map<string, ModelDetail>>(new Map());
  const [filteredCount, setFilteredCount] = React.useState(0);
  const [includeAllModels, setIncludeAllModels] = React.useState(false);
  const [lookedUpDetail, setLookedUpDetail] = React.useState<ModelDetail | null>(null);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);
  const [modelQuery, setModelQuery] = React.useState("");
  const [oauthBusy, setOauthBusy] = React.useState(false);
  const [manualCode, setManualCode] = React.useState("");
  const [manualOpen, setManualOpen] = React.useState(false);
  const autoNameRef = React.useRef("");
  const fetchSeqRef = React.useRef(0);

  React.useEffect(() => {
    if (!open) {
      fetchSeqRef.current++;
      return;
    }
    setName(profile?.name ?? "");
    autoNameRef.current = "";
    setFormat(normalizeFormat(profile?.format));
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(profile?.context_window_k ? String(profile.context_window_k * 1000) : "0");
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setUserAgent(profile?.user_agent ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelDetails(new Map());
    setFilteredCount(0);
    setIncludeAllModels(false);
    setLookedUpDetail(null);
    setModelsOpen(false);
    setModelQuery("");
    setManualCode("");
    setManualOpen(false);
    setAdvancedOpen(false);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;
  const providers = getProviders();
  const thinkingTypes = getThinkingTypes();
  const effortLevels = getEffortLevels();
  const maxTokensFields = getMaxTokensFields();
  const meta = providerMeta(format);
  const oauthProv = meta.oauthProvider ?? null;
  const oauthStatus = oauthProv ? oauthMap.get(oauthProv) : undefined;

  // Poll OAuth status while a browser login is pending in this drawer.
  React.useEffect(() => {
    if (!open || !oauthProv || !oauthStatus?.pending) return;
    const timer = window.setInterval(() => void onReloadOAuth(), 2000);
    return () => window.clearInterval(timer);
  }, [open, oauthProv, oauthStatus?.pending, onReloadOAuth]);

  async function startOAuth() {
    if (!oauthProv || oauthBusy) return;
    setOauthBusy(true);
    try {
      const res = await api.startLLMOAuth(oauthProv);
      if (!res?.auth_url) {
        toast.error(t("OAuth sign-in requires the ArtexV2 backend server to be running."));
        return;
      }
      window.open(res.auth_url, "_blank", "noopener,noreferrer");
      if (res.manual_required) {
        setManualOpen(true);
        toast.info(t("Complete Google sign-in in the browser tab, then paste the authorization code or callback URL below."));
      } else {
        toast.info(t("Complete sign-in in the browser tab that just opened."));
      }
      await onReloadOAuth();
    } catch (e) {
      toast.error(t("OAuth sign-in failed: {error}", { error: (e as Error).message }));
    } finally {
      setOauthBusy(false);
    }
  }

  async function submitManualOAuth() {
    if (!oauthProv || !manualCode.trim() || oauthBusy) return;
    setOauthBusy(true);
    try {
      await api.completeLLMOAuth(oauthProv, manualCode.trim());
      setManualCode("");
      setManualOpen(false);
      await onReloadOAuth();
      toast.success(t("OAuth account connected"));
    } catch (e) {
      toast.error(t("OAuth sign-in failed: {error}", { error: (e as Error).message }));
    } finally {
      setOauthBusy(false);
    }
  }

  async function disconnectOAuth() {
    if (!oauthProv || oauthBusy) return;
    setOauthBusy(true);
    try {
      await api.logoutLLMOAuth(oauthProv);
      await onReloadOAuth();
      toast.success(t("OAuth account disconnected"));
    } catch (e) {
      toast.error(t("Failed to disconnect: {error}", { error: (e as Error).message }));
    } finally {
      setOauthBusy(false);
    }
  }

  const filteredModels = React.useMemo(() => {
    const q = modelQuery.trim().toLowerCase();
    if (!q) return models;
    return models.filter((m) => m.toLowerCase().includes(q));
  }, [models, modelQuery]);

  const loadModels = React.useCallback(
    async (opts?: { silent?: boolean; force?: boolean; includeAll?: boolean }) => {
      const silent = opts?.silent ?? false;
      const force = opts?.force ?? false;
      const incAll = opts?.includeAll ?? includeAllModels;
      const seq = ++fetchSeqRef.current;
      setLoadingModels(true);
      if (!silent) {
        setModelQuery("");
      }
      try {
        const r = await api.fetchLLMModels(format, baseUrl, apiKey, proxy, profileId, {
          force,
          include_all: incAll,
        });
        if (seq !== fetchSeqRef.current) return;
        if (r.ok && r.models && r.models.length > 0) {
          setModels(r.models);
          setFilteredCount(r.filtered_count ?? 0);
          const detailMap = new Map<string, ModelDetail>();
          for (const d of r.model_details ?? []) {
            detailMap.set(d.id, d);
          }
          setModelDetails(detailMap);
          if (!silent) {
            setModelsOpen(true);
            toast.success(t("Loaded {count} models", { count: r.models.length }));
          }
        } else if (!silent) {
          setModels([]);
          setModelDetails(new Map());
          setFilteredCount(0);
          toast.error(t("Failed to load models: {error}", { error: r.error ?? t("no models returned") }));
        }
      } catch (e) {
        if (seq !== fetchSeqRef.current) return;
        if (!silent) {
          toast.error(t("Error loading models: {error}", { error: (e as Error).message }));
        }
      } finally {
        if (seq === fetchSeqRef.current) {
          setLoadingModels(false);
        }
      }
    },
    [format, baseUrl, apiKey, proxy, profileId, includeAllModels],
  );

  // Automatically fetch available models in the background when a ready provider is active.
  const canAutoFetch =
    open &&
    (Boolean(oauthProv && oauthStatus?.connected) ||
      (format === "ollama" && ollamaStatus?.state === "connected") ||
      format === "opencode-zen" ||
      format === "opencode-go" ||
      Boolean(!isNew && profile?.api_key_hint && profile.format === format));

  React.useEffect(() => {
    if (!canAutoFetch) return;
    void loadModels({ silent: true });
    // Intentionally scoped to sheet open + provider readiness transitions.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, format, oauthStatus?.connected, ollamaStatus?.state, profile?.id]);

  // Debounced spec lookup for typed model IDs not yet present in the fetched list.
  React.useEffect(() => {
    if (!open) return;
    const trimmed = model.trim();
    if (!trimmed) {
      setLookedUpDetail(null);
      return;
    }
    if (modelDetails.has(trimmed)) {
      setLookedUpDetail(null);
      return;
    }
    let cancelled = false;
    const timer = window.setTimeout(() => {
      void api
        .lookupLLMModelSpec(format, trimmed, {
          base_url: baseUrl,
          proxy,
          api_key: apiKey,
          profile_id: profileId,
        })
        .then((res) => {
          if (cancelled) return;
          if (res.ok && res.found && res.detail) {
            setLookedUpDetail(res.detail);
          } else {
            setLookedUpDetail(null);
          }
        })
        .catch(() => {
          if (!cancelled) setLookedUpDetail(null);
        });
    }, 300);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [open, format, model, modelDetails, baseUrl, proxy, apiKey, profileId]);

  const selectedModelDetail = React.useMemo(() => {
    const trimmed = model.trim();
    if (!trimmed) return undefined;
    const exact = modelDetails.get(trimmed);
    if (exact) return exact;
    if (lookedUpDetail && lookedUpDetail.id.toLowerCase() === trimmed.toLowerCase()) {
      return lookedUpDetail;
    }
    return undefined;
  }, [model, modelDetails, lookedUpDetail]);

  function detectedContextTokens(detail?: ModelDetail): number {
    if (!detail) return 0;
    if (detail.context_window && detail.context_window > 0) return detail.context_window;
    if (detail.context_window_k && detail.context_window_k > 0) return detail.context_window_k * 1000;
    return 0;
  }

  function applyModelDetail(detail: ModelDetail, opts?: { notify?: boolean }) {
    const ctxTokens = detectedContextTokens(detail);
    if (ctxTokens > 0) {
      setCw(String(ctxTokens));
    }
    if (detail.max_output_tokens && detail.max_output_tokens > 0) {
      setMaxTokens(String(detail.max_output_tokens));
    }
    if (format === "openai") {
      setMaxTokensField(detail.max_tokens_field ? detail.max_tokens_field : NONE);
    }
    if (detail.thinking_type) {
      setThinkingType(detail.thinking_type);
    }
    if (detail.reasoning_levels && detail.reasoning_levels.length > 0) {
      if (effort === NONE) {
        if (detail.default_reasoning_effort && detail.reasoning_levels.includes(detail.default_reasoning_effort)) {
          setEffort(detail.default_reasoning_effort);
        }
      } else if (!detail.reasoning_levels.includes(effort)) {
        setEffort(
          detail.default_reasoning_effort && detail.reasoning_levels.includes(detail.default_reasoning_effort)
            ? detail.default_reasoning_effort
            : detail.reasoning_levels.includes("high")
              ? "high"
              : detail.reasoning_levels[0],
        );
      }
    }
    if (isNew && (!name.trim() || name.trim() === autoNameRef.current)) {
      const suggested = `${meta.shortLabel} · ${detail.label || detail.id}`;
      autoNameRef.current = suggested;
      setName(suggested);
    }
    if (opts?.notify) {
      toast.success(t("Applied auto-detected defaults for {model}", { model: detail.id }));
    }
  }

  function applySelectedModel(selectedId: string) {
    setModel(selectedId);
    const detail = modelDetails.get(selectedId);
    if (detail) {
      applyModelDetail(detail);
    } else if (isNew && (!name.trim() || name.trim() === autoNameRef.current)) {
      const suggested = `${meta.shortLabel} · ${selectedId}`;
      autoNameRef.current = suggested;
      setName(suggested);
    }
    setModelsOpen(false);
  }

  async function testConnection() {
    if (testing) return;
    setTesting(true);
    try {
      const r = await api.testLLM(
        format,
        model,
        baseUrl,
        apiKey,
        proxy,
        toStore(thinkingType),
        toStore(effort),
        profileId,
        streaming,
        sessionHeaderKey.trim(),
        userAgent.trim(),
        format === "openai" ? toStore(maxTokensField) : "",
      );
      if (r.ok) {
        toast.success(t("Connected · {latency}ms · {model}", { latency: r.latency_ms ?? "?", model: r.model ?? model }), {
          description: r.reply ? t("Reply: {reply}", { reply: r.reply }) : undefined,
        });
      } else {
        const suggestedField = r.suggested_fix?.max_tokens_field;
        if (format === "openai" && suggestedField !== undefined) {
          const nextField = suggestedField === "max_completion_tokens" ? "max_completion_tokens" : NONE;
          const fieldLabel = suggestedField === "max_completion_tokens" ? "max_completion_tokens" : "max_tokens";
          toast.error(t("Connection failed: {error}", { error: r.error ?? t("unknown") }), {
            action: {
              label: t("Use {field}", { field: fieldLabel }),
              onClick: () => {
                setMaxTokensField(nextField);
                toast.success(t("Cap field switched to {field}; test again.", { field: fieldLabel }));
              },
            },
          });
        } else {
          toast.error(t("Connection failed: {error}", { error: r.error ?? t("unknown") }));
        }
      }
    } catch (e) {
      toast.error(t("Test error: {error}", { error: (e as Error).message }));
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error(t("Please fill in a name and a model"));
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const cwRaw = Math.max(0, Math.round(Number(cw) || 0));
      const contextWindowK = cwRaw === 0 ? 0 : Math.min(2000, Math.max(1, Math.round(cwRaw / 1000)));
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim(),
        format,
        model: model.trim(),
        base_url: baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: contextWindowK,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming,
        max_tokens: Math.max(0, Number(maxTokens) || 0),
        max_tokens_field: format === "openai" ? toStore(maxTokensField) : "",
        session_header_key: sessionHeaderKey.trim(),
        user_agent: userAgent.trim(),
        retry,
      });
      if (isNew) {
        toast.success(t('Created: {name} (use "Set as active" on its card to enable it)', { name: name.trim() }));
      } else {
        toast.success(
          profile?.is_default ? t("Saved; the active profile takes effect immediately, no restart needed") : t("Saved"),
        );
      }
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(t("Save failed: {error}", { error: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  let keyPlaceholder = "sk-…";
  if (format === "ollama") {
    keyPlaceholder = t("optional — a local Ollama server runs keyless");
  } else if (keyHint) {
    keyPlaceholder = t("Set ({hint}); leave empty to keep it", { hint: keyHint });
  }

  const oauthSignInLabel = () => {
    switch (oauthProv) {
      case "chatgpt":
        return t("Sign in with ChatGPT");
      case "anthropic":
        return t("Sign in with Claude");
      case "devin":
        return t("Sign in with Devin");
      case "antigravity-cli":
        return t("Sign in with Google");
      default:
        return t("Sign in");
    }
  };

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[440px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="border-b px-5 py-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? t("New model profile") : t("Edit: {name}", { name: profile?.name ?? "" })}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                {t("Active")}
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? t("Choose a provider, select a model, and tune its reasoning level.")
              : t("Click save after editing; saving the active profile takes effect immediately for all agents.")}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 py-5">
          {/* Step 1: Provider */}
          <section className="grid gap-3.5">
            <StepBadge step={1} title={t("Provider")} />
            <div className="grid grid-cols-2 gap-2">
              {providers.map((p) => {
                const selected = format === p.id;
                const conn = providerConnectionState(p, profiles, healthMap, oauthMap, ollamaStatus);
                return (
                  <button
                    key={p.id}
                    type="button"
                    aria-pressed={selected}
                    title={`${p.shortLabel} — ${conn.label}: ${conn.hint}`}
                    onClick={() => {
                      if (format !== p.id) {
                        setFormat(p.id);
                        setModels([]);
                        setModelDetails(new Map());
                        setFilteredCount(0);
                        setIncludeAllModels(false);
                        setLookedUpDetail(null);
                        setModelsOpen(false);
                        setModelQuery("");
                      }
                    }}
                    className={cn(
                      "group relative flex flex-col gap-1 rounded-xl border p-3 text-left transition-all duration-200",
                      selected
                        ? "border-primary bg-primary/5 shadow-xs ring-1 ring-primary/30"
                        : "border-border bg-card hover:border-foreground/25 hover:bg-muted/40",
                    )}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span
                        className={cn(
                          "flex items-center gap-1.5 font-medium text-xs sm:text-sm",
                          selected ? "text-foreground" : "text-foreground/85",
                        )}
                      >
                        <span className={cn("transition-colors", selected ? "text-primary" : "text-muted-foreground")}>
                          <ProviderIcon format={p.id} className="size-4" />
                        </span>
                        <span className="truncate">{p.shortLabel}</span>
                      </span>
                      <span className="flex items-center gap-1.5">
                        {p.oauthProvider && (
                          <Badge variant="secondary" className="px-1 py-0 text-[9px] font-medium uppercase">
                            OAuth
                          </Badge>
                        )}
                        {selected && (
                          <span className="flex size-4 items-center justify-center rounded-full bg-primary text-primary-foreground animate-in zoom-in-75 duration-150">
                            <CheckIcon className="size-2.5" />
                          </span>
                        )}
                        <span
                          aria-label={`${p.shortLabel}: ${conn.label}`}
                          title={`${conn.label} — ${conn.hint}`}
                          className={cn("inline-block size-2 shrink-0 rounded-full", conn.dotCls)}
                        />
                      </span>
                    </div>
                    <p className="line-clamp-2 text-muted-foreground text-xs leading-snug">{p.desc}</p>
                  </button>
                );
              })}
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className={cn("grid gap-1.5", oauthProv && "sm:col-span-2")}>
                <Label htmlFor="p-name">{t("Name")}</Label>
                <Input
                  id="p-name"
                  placeholder={oauthProv ? meta.shortLabel : t("e.g. OpenAI production")}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              {!oauthProv && (
                <div className="grid gap-1.5">
                  <div className="flex items-center justify-between gap-2">
                    <Label htmlFor="p-api-key">
                      {format === "ollama" ? t("API Key (optional for Ollama)") : "API Key"}
                    </Label>
                    {meta.keyPageUrl && (
                      <a
                        href={meta.keyPageUrl}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 text-[11px] text-muted-foreground hover:text-foreground"
                      >
                        {t("Get key")} <ExternalLinkIcon className="size-2.5" />
                      </a>
                    )}
                  </div>
                  <Input
                    id="p-api-key"
                    type="password"
                    placeholder={keyPlaceholder}
                    value={apiKey}
                    onChange={(e) => setApiKey(e.target.value)}
                    autoComplete="new-password"
                    spellCheck={false}
                    data-1p-ignore="true"
                    data-lpignore="true"
                  />
                </div>
              )}
            </div>

            {oauthProv && (
              <div className="grid gap-2.5 rounded-xl border bg-muted/25 p-3.5 animate-in fade-in-0 duration-150">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <KeyRoundIcon className="size-4 text-primary" />
                    <div>
                      <p className="font-medium text-sm">
                        {oauthStatus?.connected
                          ? t("Connected via OAuth")
                          : oauthStatus?.pending
                            ? t("Waiting for browser sign-in…")
                            : t("Subscription sign-in")}
                      </p>
                      <p className="text-muted-foreground text-xs">
                        {oauthStatus?.connected
                          ? oauthStatus.account_hint || meta.shortLabel
                          : meta.desc}
                      </p>
                    </div>
                  </div>
                  <div className="flex items-center gap-1.5">
                    {oauthStatus?.connected ? (
                      <Button size="sm" variant="outline" disabled={oauthBusy} onClick={() => void disconnectOAuth()}>
                        <LogOutIcon className="size-3.5" /> {t("Disconnect")}
                      </Button>
                    ) : (
                      <Button size="sm" disabled={oauthBusy} onClick={() => void startOAuth()}>
                        {oauthBusy || oauthStatus?.pending ? (
                          <Loader2Icon className="size-3.5 animate-spin" />
                        ) : (
                          <LogInIcon className="size-3.5" />
                        )}
                        {oauthSignInLabel()}
                      </Button>
                    )}
                  </div>
                </div>
                {oauthStatus?.last_error && (
                  <p className="font-mono text-destructive text-xs">{oauthStatus.last_error}</p>
                )}
                {!oauthStatus?.connected && oauthStatus?.pending && (
                  <div className="grid gap-1.5 border-t pt-2.5">
                    <button
                      type="button"
                      className="text-left text-muted-foreground text-xs hover:text-foreground"
                      onClick={() => setManualOpen((v) => !v)}
                    >
                      {t("Using a remote browser? Paste redirect URL or code")}
                    </button>
                    {(manualOpen || oauthProv === "antigravity-cli") && (
                      <div className="flex gap-2">
                        <Input
                          className="h-8 font-mono text-xs"
                          placeholder={t("Paste redirect URL or authorization code…")}
                          value={manualCode}
                          onChange={(e) => setManualCode(e.target.value)}
                        />
                        <Button size="sm" variant="secondary" disabled={oauthBusy || !manualCode.trim()} onClick={() => void submitManualOAuth()}>
                          {t("Complete")}
                        </Button>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}

            <div className="grid gap-1.5">
              <Label htmlFor="p-base-url">{t("Base URL (optional)")}</Label>
              <Input
                id="p-base-url"
                className="font-mono text-xs"
                placeholder={meta.baseUrlPlaceholder}
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
              />
            </div>
          </section>

          {/* Step 2: Model */}
          <section className="grid gap-3.5 border-t pt-4">
            <StepBadge step={2} title={t("Model")} />
            <div className="grid gap-1.5">
              <Label htmlFor="p-model">{t("Model")}</Label>
              <div className="flex gap-2">
                <Input
                  id="p-model"
                  className="font-mono"
                  placeholder={meta.modelPlaceholder}
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                />
                <Popover
                  open={modelsOpen}
                  onOpenChange={(o) => {
                    setModelsOpen(o);
                    if (!o) setModelQuery("");
                  }}
                  modal
                >
                  <PopoverTrigger asChild>
                    <Button
                      type="button"
                      variant="outline"
                      className="shrink-0 gap-1.5"
                      disabled={loadingModels}
                      onClick={() => void loadModels({ silent: false, force: true })}
                      title={t("Refresh available models from the API")}
                    >
                      {loadingModels ? <Loader2Icon className="size-3.5 animate-spin" /> : <RefreshCwIcon className="size-3.5" />}
                      <span className="hidden sm:inline">
                        {models.length > 0 ? t("Models ({count})", { count: models.length }) : t("Fetch")}
                      </span>
                    </Button>
                  </PopoverTrigger>
                  {models.length > 0 && (
                    <PopoverContent className="w-96 gap-0 p-0" align="end">
                      <div className="border-b p-1">
                        <Input
                          autoFocus
                          className="h-8 border-0 shadow-none focus-visible:ring-0 dark:bg-transparent"
                          placeholder={t("Search {count} models…", { count: models.length })}
                          value={modelQuery}
                          onChange={(e) => setModelQuery(e.target.value)}
                          spellCheck={false}
                          aria-label={t("Search models")}
                        />
                      </div>
                      <div className="max-h-64 overflow-y-auto overscroll-contain p-1">
                        {filteredModels.length === 0 ? (
                          <p className="px-2 py-6 text-center text-muted-foreground text-xs">
                            {t("No model matches “{query}”", { query: modelQuery.trim() })}
                          </p>
                        ) : (
                          filteredModels.map((m) => {
                            const d = modelDetails.get(m);
                            const ctxT = detectedContextTokens(d);
                            const outT = d?.max_output_tokens ?? 0;
                            const specParts: string[] = [];
                            if (ctxT > 0) specParts.push(formatTokensShort(ctxT));
                            if (outT > 0) specParts.push(t("{n} out", { n: formatTokensShort(outT) }));
                            return (
                              <button
                                key={m}
                                type="button"
                                className="flex w-full shrink-0 items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left font-mono text-xs transition-colors hover:bg-accent hover:text-accent-foreground"
                                onClick={() => applySelectedModel(m)}
                              >
                                <span className="min-w-0 flex-1 truncate">
                                  {m}
                                  {d?.label && d.label !== m && (
                                    <span className="ml-1.5 font-sans text-[11px] text-muted-foreground">{d.label}</span>
                                  )}
                                </span>
                                {specParts.length > 0 && (
                                  <Badge variant="outline" className="shrink-0 px-1.5 py-0 font-mono text-[10px]">
                                    {specParts.join(" · ")}
                                  </Badge>
                                )}
                              </button>
                            );
                          })
                        )}
                      </div>
                      {(filteredCount > 0 || includeAllModels) && (
                        <div className="flex items-center justify-between border-t px-2.5 py-1.5 text-[11px] text-muted-foreground">
                          <span>
                            {includeAllModels
                              ? t("Showing all models (including non-chat)")
                              : t("{count} non-chat models hidden", { count: filteredCount })}
                          </span>
                          <button
                            type="button"
                            className="font-medium text-primary hover:underline"
                            disabled={loadingModels}
                            onClick={() => {
                              const next = !includeAllModels;
                              setIncludeAllModels(next);
                              void loadModels({ silent: false, force: false, includeAll: next });
                            }}
                          >
                            {includeAllModels ? t("Chat only") : t("Show all")}
                          </button>
                        </div>
                      )}
                    </PopoverContent>
                  )}
                </Popover>
              </div>
            </div>

            {selectedModelDetail &&
              (detectedContextTokens(selectedModelDetail) > 0 || (selectedModelDetail.max_output_tokens ?? 0) > 0) && (() => {
                const detCtx = detectedContextTokens(selectedModelDetail);
                const detOut = selectedModelDetail.max_output_tokens ?? 0;
                const curCtx = Math.round(Number(cw) || 0);
                const curOut = Math.round(Number(maxTokens) || 0);
                const ctxMatches = detCtx === 0 || Math.abs(curCtx - detCtx) < 1000;
                const outMatches = detOut === 0 || curOut === detOut;
                const fieldMatches =
                  format !== "openai" ||
                  !selectedModelDetail.max_tokens_field ||
                  maxTokensField === selectedModelDetail.max_tokens_field;
                const allMatch = ctxMatches && outMatches && fieldMatches;
                const sourceLabel =
                  selectedModelDetail.source === "api"
                    ? t("Live API")
                    : selectedModelDetail.source === "ollama"
                      ? "Ollama"
                      : selectedModelDetail.source === "models.dev"
                        ? "models.dev"
                        : t("Known spec");
                const hasPrice =
                  (selectedModelDetail.price_input_per_m ?? 0) > 0 || (selectedModelDetail.price_output_per_m ?? 0) > 0;
                return (
                  <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-primary/25 bg-primary/[0.04] px-3 py-2 text-xs">
                    <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
                      <SparklesIcon className="size-3.5 shrink-0 text-primary" />
                      <span className="font-medium text-foreground">{t("Auto-detected")}</span>
                      <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
                        {sourceLabel}
                      </Badge>
                      {detCtx > 0 && (
                        <span className="font-mono text-muted-foreground">
                          {t("Context {short}", { short: formatTokensShort(detCtx) })}
                        </span>
                      )}
                      {detOut > 0 && (
                        <span className="font-mono text-muted-foreground">
                          · {t("Output {short}", { short: formatTokensShort(detOut) })}
                        </span>
                      )}
                      {hasPrice && (
                        <span className="font-mono text-muted-foreground">
                          · ${selectedModelDetail.price_input_per_m ?? 0}/${selectedModelDetail.price_output_per_m ?? 0}/1M
                        </span>
                      )}
                      {format === "openai" && selectedModelDetail.max_tokens_field && (
                        <span className="font-mono text-muted-foreground">
                          · {selectedModelDetail.max_tokens_field}
                        </span>
                      )}
                    </div>
                    {allMatch ? (
                      <Badge
                        variant="outline"
                        className="shrink-0 gap-1 border-emerald-500/40 text-[10px] text-emerald-600 dark:text-emerald-400"
                      >
                        <CheckIcon className="size-2.5" /> {t("Applied")}
                      </Badge>
                    ) : (
                      <Button
                        type="button"
                        size="sm"
                        variant="secondary"
                        className="h-6 shrink-0 px-2 text-[11px]"
                        onClick={() => applyModelDetail(selectedModelDetail, { notify: true })}
                      >
                        {t("Apply defaults")}
                      </Button>
                    )}
                  </div>
                );
              })()}

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="grid gap-1.5">
                <div className="flex items-center justify-between gap-2">
                  <Label htmlFor="p-cw">{t("Context window (tokens)")}</Label>
                  <Badge variant="secondary" className="px-1.5 py-0 font-mono text-[10px]">
                    {Number(cw) > 0 ? formatTokensShort(Number(cw)) : t("Auto (200K)")}
                  </Badge>
                </div>
                <Input
                  id="p-cw"
                  type="number"
                  min={0}
                  max={2_000_000}
                  step={1000}
                  className="font-mono"
                  value={cw}
                  onChange={(e) => setCw(e.target.value)}
                  placeholder="200000"
                />
                <div className="flex flex-wrap gap-1">
                  {CONTEXT_TOKEN_PRESETS.map((tokens) => (
                    <button
                      key={tokens}
                      type="button"
                      onClick={() => setCw(String(tokens))}
                      className={cn(
                        "rounded px-1.5 py-0.5 font-mono text-[10px] transition-colors",
                        Number(cw) === tokens
                          ? "bg-primary text-primary-foreground"
                          : "bg-muted text-muted-foreground hover:bg-accent hover:text-foreground",
                      )}
                    >
                      {formatTokensShort(tokens)}
                    </button>
                  ))}
                </div>
                <p className="text-[11px] text-muted-foreground">
                  {t("Total input+output capacity in tokens (0 = default 200,000). Auto-filled on model selection.")}
                </p>
              </div>

              <div className="grid gap-1.5">
                <div className="flex items-center justify-between gap-2">
                  <Label htmlFor="p-max-tokens">{t("Output cap (max tokens)")}</Label>
                  <Badge variant="secondary" className="px-1.5 py-0 font-mono text-[10px]">
                    {Number(maxTokens) > 0 ? formatTokensShort(Number(maxTokens)) : t("Provider default")}
                  </Badge>
                </div>
                <Input
                  id="p-max-tokens"
                  type="number"
                  min={0}
                  step={1024}
                  className="font-mono"
                  value={maxTokens}
                  onChange={(e) => setMaxTokens(e.target.value)}
                  placeholder="0"
                />
                <div className="flex flex-wrap gap-1">
                  {OUTPUT_TOKEN_PRESETS.map((tokens) => (
                    <button
                      key={tokens}
                      type="button"
                      onClick={() => setMaxTokens(String(tokens))}
                      className={cn(
                        "rounded px-1.5 py-0.5 font-mono text-[10px] transition-colors",
                        Number(maxTokens) === tokens
                          ? "bg-primary text-primary-foreground"
                          : "bg-muted text-muted-foreground hover:bg-accent hover:text-foreground",
                      )}
                    >
                      {tokens === 0 ? t("Auto") : formatTokensShort(tokens)}
                    </button>
                  ))}
                </div>
                <p className="text-[11px] text-muted-foreground">
                  {t("Max tokens per reply (0 = omit and let the provider decide). Auto-filled on model selection.")}
                </p>
              </div>
            </div>

            {format === "openai" && (
              <div className="flex items-center justify-between gap-4 rounded-lg border bg-muted/25 p-3 animate-in fade-in-0 duration-150">
                <div className="grid gap-0.5">
                  <Label className="text-xs font-medium">{t("Cap field name")}</Label>
                  <p className="text-muted-foreground text-xs">{getMaxTokensFieldHint("openai")}</p>
                </div>
                <Select value={maxTokensField} onValueChange={setMaxTokensField}>
                  <SelectTrigger className="w-52 shrink-0">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {maxTokensFields.map((o) => (
                      <SelectItem key={o.value} value={o.value}>
                        {o.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
          </section>

          {/* Step 3: Reasoning level */}
          <section className="grid gap-3.5 border-t pt-4">
            <StepBadge step={3} title={t("Reasoning level")} />

            <div className="grid gap-2">
              <Label className="text-xs font-medium text-muted-foreground">{t("Thinking switch · thinking.type")}</Label>
              <div className="grid grid-cols-3 gap-1.5 rounded-lg bg-muted/60 p-1">
                {thinkingTypes.map((o) => {
                  const active = thinkingType === o.value;
                  return (
                    <button
                      key={o.value}
                      type="button"
                      aria-pressed={active}
                      onClick={() => setThinkingType(o.value)}
                      className={cn(
                        "rounded-md px-2.5 py-1.5 text-xs font-medium transition-all duration-150",
                        active
                          ? "bg-background text-foreground shadow-xs"
                          : "text-muted-foreground hover:text-foreground",
                      )}
                    >
                      {o.label}
                    </button>
                  );
                })}
              </div>
              <p className="text-muted-foreground text-xs">
                {t(
                  "Controls whether the thinking field is sent. Not sent = the field is omitted (compatible with models like MiniMax that lack support); Disabled = send disabled; Enabled = send enabled. Independent of the effort below.",
                )}
              </p>
            </div>

            <div className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <Label className="text-xs font-medium text-muted-foreground">{t("Thinking effort · reasoning_effort")}</Label>
                {selectedModelDetail?.reasoning_levels && selectedModelDetail.reasoning_levels.length > 0 && (
                  <span className="font-mono text-[10px] text-muted-foreground">
                    {t("Supported: {levels}", { levels: selectedModelDetail.reasoning_levels.join(", ") })}
                  </span>
                )}
              </div>
              <div className="grid grid-cols-3 gap-1.5 rounded-lg bg-muted/60 p-1 sm:grid-cols-6">
                {effortLevels.map((o) => {
                  const active = effort === o.value;
                  return (
                    <button
                      key={o.value}
                      type="button"
                      aria-pressed={active}
                      onClick={() => setEffort(o.value)}
                      className={cn(
                        "rounded-md px-2 py-1.5 text-xs font-medium transition-all duration-150",
                        active
                          ? "bg-background text-foreground shadow-xs ring-1 ring-primary/20"
                          : "text-muted-foreground hover:text-foreground",
                      )}
                    >
                      {o.label}
                    </button>
                  );
                })}
              </div>
              <p className="text-muted-foreground text-xs">
                {t(
                  "A separate effort knob (OpenAI reasoning_effort / Anthropic output_config.effort). Some APIs have no thinking field and activate thinking via effort alone, so it can be set on its own without sending the thinking switch.",
                )}
              </p>
            </div>
          </section>

          {/* Collapsible Advanced Section */}
          <Collapsible open={advancedOpen} onOpenChange={setAdvancedOpen} className="border-t pt-4">
            <CollapsibleTrigger asChild>
              <button
                type="button"
                className="flex w-full items-center justify-between rounded-lg border bg-muted/25 px-3.5 py-2.5 text-left transition-colors hover:bg-muted/50"
              >
                <span className="flex items-center gap-2 font-medium text-sm">
                  <SlidersHorizontalIcon className="size-4 text-muted-foreground" />
                  {t("Pool, network & retry overrides")}
                </span>
                <ChevronDownIcon
                  className={cn("size-4 text-muted-foreground transition-transform duration-200", advancedOpen && "rotate-180")}
                />
              </button>
            </CollapsibleTrigger>
            <CollapsibleContent className="pt-3">
              <div className="grid gap-4">
                <div className="grid gap-3 rounded-lg border p-3">
                  <div className="flex items-center justify-between gap-4">
                    <div className="grid gap-0.5">
                      <Label htmlFor="p-priority" className="text-sm">
                        {t("Pool priority")}
                      </Label>
                      <p className="text-muted-foreground text-xs">
                        {t(
                          "Higher numbers are picked first; the active profile is always first regardless of this value. Profiles with equal priority take turns leading, spreading quota naturally.",
                        )}
                      </p>
                    </div>
                    <Input
                      id="p-priority"
                      type="number"
                      className="w-24 shrink-0"
                      value={priority}
                      onChange={(e) => setPriority(e.target.value)}
                    />
                  </div>
                  <div className="flex items-center justify-between gap-4 border-t pt-3">
                    <div className="grid gap-0.5">
                      <Label className="text-sm">{t("Excluded from pool")}</Label>
                      <p className="text-muted-foreground text-xs">
                        {t(
                          "When enabled, this profile is never used as a failover target (Agents / tasks can still pin it explicitly). Suited to expensive profiles reserved for one specific Agent that should not be burned through by other agents' failures.",
                        )}
                      </p>
                    </div>
                    <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label={t("Excluded from pool")} />
                  </div>
                  <div className="flex items-center justify-between gap-4 border-t pt-3">
                    <div className="grid gap-0.5">
                      <Label className="text-sm">{t("Streaming output")}</Label>
                      <p className="text-muted-foreground text-xs">
                        {t(
                          "On (default) streams over SSE, with live in-flight progress and real-time token counting. Off uses true non-streaming (stream:false, returning the full response in one shot) — a way around gateways with broken SSE implementations (empty frames / dropped thinking-field frames), at the cost of losing live in-flight progress.",
                        )}
                      </p>
                    </div>
                    <Switch checked={streaming} onCheckedChange={setStreaming} aria-label={t("Streaming output")} />
                  </div>
                </div>

                <div className="grid gap-3 sm:grid-cols-2">
                  <div className="grid gap-1.5">
                    <Label htmlFor="p-rps">{t("Rate limit per second")}</Label>
                    <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
                  </div>
                  <div className="grid gap-1.5">
                    <Label htmlFor="p-rpm">{t("Rate limit per minute")}</Label>
                    <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
                  </div>
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="p-proxy">{t("Proxy (optional)")}</Label>
                  <Input
                    id="p-proxy"
                    className="font-mono text-xs"
                    placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
                    value={proxy}
                    onChange={(e) => setProxy(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    {t(
                      "Only outbound LLM requests go through this proxy; http/https/socks5 are supported, optionally with credentials (e.g. socks5://user:pass@host:port — URL-encode the password if it contains special characters). Leave empty for no proxy (direct connection).",
                    )}
                  </p>
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="p-session-header">{t("Custom session header (optional)")}</Label>
                  <Input
                    id="p-session-header"
                    className="font-mono text-xs"
                    placeholder={t("e.g. x-session-id (empty = not sent)")}
                    value={sessionHeaderKey}
                    onChange={(e) => setSessionHeaderKey(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    {rich(
                      t(
                        "Once a header name is set, every request carries this HTTP header, with the value filled in automatically as the <b>current session's session id</b> (chat sessions like conv-12, workers like exp3-worker-i87). Meant for gateways that do prompt caching / sticky routing on a session-id header; stable across turns of one session, distinct across sessions. Leave empty to not send it.",
                      ),
                    )}
                  </p>
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="p-user-agent">{t("Custom User-Agent (optional)")}</Label>
                  <Input
                    id="p-user-agent"
                    className="font-mono text-xs"
                    placeholder={t("e.g. my-app/1.0 (empty = default UA)")}
                    value={userAgent}
                    onChange={(e) => setUserAgent(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    {rich(
                      t(
                        "Overrides the <b>User-Agent</b> header on every outbound LLM request. Useful for gateways that route or allow-list by User-Agent. Leave empty to keep the default User-Agent unchanged.",
                      ),
                    )}
                  </p>
                </div>

                <ProfileRetryFields value={retry} onChange={setRetry} />
              </div>
            </CollapsibleContent>
          </Collapsible>
        </div>

        <div className="flex gap-2 border-t bg-background/95 px-5 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? t("Testing…") : t("Test connection")}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? t("Create") : t("Save")}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}
