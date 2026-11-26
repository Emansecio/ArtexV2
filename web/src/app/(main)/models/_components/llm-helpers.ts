import * as React from "react";

import { t } from "@/i18n";
import type { LLMPoolMember, LLMProfile, OAuthProviderId, OAuthProviderStatus } from "@/lib/types";

export type ProfileFormat =
  | "chatgpt"
  | "anthropic-oauth"
  | "antigravity-cli"
  | "devin"
  | "opencode-zen"
  | "opencode-go"
  | "anthropic"
  | "openai"
  | "openai-responses"
  | "ollama";

export const NONE = "none";
export const fromStore = (v?: string) => (v ? v : NONE);
export const toStore = (v: string) => (v === NONE ? "" : v);

export const OLLAMA_DEFAULT_BASE_URL = "http://127.0.0.1:11434/v1";
export const OPENCODE_ZEN_BASE_URL = "https://opencode.ai/zen/v1";
export const OPENCODE_GO_BASE_URL = "https://opencode.ai/zen/go/v1";

export interface ProviderMeta {
  id: ProfileFormat;
  label: string;
  shortLabel: string;
  desc: string;
  modelPlaceholder: string;
  baseUrlPlaceholder: string;
  keyless?: boolean;
  oauthProvider?: OAuthProviderId;
  keyPageUrl?: string;
}

export function getProviders(): ProviderMeta[] {
  return [
    {
      id: "chatgpt",
      label: t("Codex / ChatGPT (OAuth)"),
      shortLabel: "Codex / ChatGPT",
      desc: t("Sign in with ChatGPT Plus or Pro subscription"),
      modelPlaceholder: "gpt-5.4",
      baseUrlPlaceholder: "https://api.openai.com/v1",
      oauthProvider: "chatgpt",
    },
    {
      id: "anthropic-oauth",
      label: t("Claude Code (OAuth)"),
      shortLabel: "Claude Code",
      desc: t("Sign in with Claude Pro or Max subscription"),
      modelPlaceholder: "claude-opus-4-8",
      baseUrlPlaceholder: "https://api.anthropic.com",
      oauthProvider: "anthropic",
    },
    {
      id: "antigravity-cli",
      label: t("Antigravity CLI (OAuth)"),
      shortLabel: "Antigravity CLI",
      desc: t("Google Antigravity CLI (Gemini 4 & Claude via ACP)"),
      modelPlaceholder: "gemini-4-carbon",
      baseUrlPlaceholder: "acp://antigravity-cli",
      oauthProvider: "antigravity-cli",
    },
    {
      id: "devin",
      label: t("Devin (OAuth)"),
      shortLabel: "Devin",
      desc: t("Cognition Devin Cascade API (SWE-1.6, Claude, Gemini)"),
      modelPlaceholder: "swe-1-6",
      baseUrlPlaceholder: "https://server.codeium.com",
      oauthProvider: "devin",
    },
    {
      id: "opencode-zen",
      label: "OpenCode Zen",
      shortLabel: "OpenCode Zen",
      desc: t("Pay-as-you-go OpenCode gateway (Claude, GPT, Grok, open models)"),
      modelPlaceholder: "claude-sonnet-4-6",
      baseUrlPlaceholder: OPENCODE_ZEN_BASE_URL,
      keyPageUrl: "https://opencode.ai/auth",
    },
    {
      id: "opencode-go",
      label: "OpenCode Go",
      shortLabel: "OpenCode Go",
      desc: t("OpenCode Go subscription (GLM, Kimi, DeepSeek, Qwen)"),
      modelPlaceholder: "deepseek-v3.2",
      baseUrlPlaceholder: OPENCODE_GO_BASE_URL,
      keyPageUrl: "https://opencode.ai/auth",
    },
    {
      id: "anthropic",
      label: "Anthropic (API Key)",
      shortLabel: "Anthropic",
      desc: t("Claude Messages API with native thinking"),
      modelPlaceholder: "claude-opus-4-8",
      baseUrlPlaceholder: "https://api.anthropic.com",
      keyPageUrl: "https://console.anthropic.com/settings/keys",
    },
    {
      id: "openai",
      label: "OpenAI (Chat Completions)",
      shortLabel: "OpenAI Chat",
      desc: t("OpenAI & compatible gateways (/chat/completions)"),
      modelPlaceholder: "gpt-5.4",
      baseUrlPlaceholder: "https://api.openai.com/v1",
      keyPageUrl: "https://platform.openai.com/api-keys",
    },
    {
      id: "openai-responses",
      label: "OpenAI (Responses API)",
      shortLabel: "Responses API",
      desc: t("OpenAI Responses API (/responses)"),
      modelPlaceholder: "o3",
      baseUrlPlaceholder: "https://api.openai.com/v1",
      keyPageUrl: "https://platform.openai.com/api-keys",
    },
    {
      id: "ollama",
      label: t("Ollama (local, no key needed)"),
      shortLabel: "Ollama",
      desc: t("Local self-hosted models, no API key required"),
      modelPlaceholder: "llama3.2",
      baseUrlPlaceholder: OLLAMA_DEFAULT_BASE_URL,
      keyless: true,
    },
  ];
}

export function providerMeta(format?: string): ProviderMeta {
  const list = getProviders();
  return list.find((p) => p.id === format) ?? list.find((p) => p.id === "anthropic")!;
}

export function normalizeFormat(format?: string): ProfileFormat {
  const found = getProviders().find((p) => p.id === format);
  return found ? found.id : "anthropic";
}

export function oauthProviderOf(format?: string): OAuthProviderId | null {
  return providerMeta(format).oauthProvider ?? null;
}

export function getThinkingTypes(): { value: string; label: string }[] {
  return [
    { value: NONE, label: t("Not sent (default)") },
    { value: "disabled", label: t("Disabled") },
    { value: "enabled", label: t("Enabled") },
  ];
}

export function getEffortLevels(): { value: string; label: string }[] {
  return [
    { value: NONE, label: t("Default") },
    { value: "low", label: "low" },
    { value: "medium", label: "medium" },
    { value: "high", label: "high" },
    { value: "xhigh", label: "xhigh" },
    { value: "max", label: "max" },
  ];
}

export const CONTEXT_TOKEN_PRESETS = [128_000, 200_000, 256_000, 400_000, 1_000_000, 2_000_000] as const;
export const OUTPUT_TOKEN_PRESETS = [0, 8_192, 16_384, 32_768, 64_000, 128_000] as const;

export function formatTokensShort(tokens: number): string {
  if (!Number.isFinite(tokens) || tokens <= 0) return t("Auto");
  if (tokens >= 1_000_000) {
    return `${Number((tokens / 1_000_000).toFixed(2))}M`;
  }
  if (tokens >= 1_000) {
    return `${Number((tokens / 1_000).toFixed(1))}K`;
  }
  return String(Math.round(tokens));
}

export function formatContextK(k?: number): string {
  if (!k || k <= 0) return "";
  if (k >= 1000) {
    return `${Number((k / 1000).toFixed(2))}M`;
  }
  return `${k}K`;
}

export function getMaxTokensFields(): { value: string; label: string }[] {
  return [
    { value: NONE, label: t("max_tokens (default)") },
    { value: "max_completion_tokens", label: "max_completion_tokens" },
  ];
}

export function getMaxTokensFieldHint(format: string): string {
  switch (format) {
    case "openai":
      return t(
        "Which key carries the cap. max_tokens is the default and the vast majority of compatible gateways only accept it; OpenAI's official reasoning models (the o series / GPT-5) conversely only accept max_completion_tokens and fail outright with unsupported_parameter when they receive max_tokens.",
      );
    case "anthropic":
      return t("Only selectable for the openai format. Anthropic's field name is fixed as max_tokens.");
    case "openai-responses":
      return t("Only selectable for the openai format. The Responses API's field name is fixed as max_output_tokens.");
    case "ollama":
      return t("Only selectable for the openai format. Ollama's field name is fixed as max_tokens.");
    default:
      return "";
  }
}

/** Renders a translated string, turning <b>…</b> spans into bold elements. */
export function rich(s: string): React.ReactNode[] {
  return s.split(/<b>(.*?)<\/b>/).map((part, i) => (i % 2 === 1 ? React.createElement("b", { key: i }, part) : part));
}

export function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return t("{n}s", { n: secs });
  return t("{n}min", { n: Math.ceil(secs / 60) });
}

export type Health = {
  state: "ok" | "degraded" | "tripped" | "nokey";
  label: string;
  cls: string;
  dotCls: string;
  hint?: string;
};

export type ProviderStatusInfo = {
  state: "connected" | "disconnected" | "error";
  label: string;
  hint: string;
  dotCls: string;
};

export function providerConnectionState(
  prov: ProviderMeta,
  profiles: LLMProfile[],
  healthMap: Map<string, LLMPoolMember>,
  oauthMap: Map<string, OAuthProviderStatus>,
  ollamaStatus?: { state: "connected" | "disconnected" | "error"; detail?: string } | null,
): ProviderStatusInfo {
  const matching = profiles.filter((p) => p.format === prov.id);
  const poolErr = matching
    .map((p) => healthMap.get(p.id))
    .find((m) => m && (m.state === "tripped" || m.state === "degraded"));

  if (prov.oauthProvider) {
    const st = oauthMap.get(prov.oauthProvider);
    if (st?.state === "error" || (st?.last_error && !st.connected)) {
      return {
        state: "error",
        label: t("Error"),
        hint: st?.last_error || t("OAuth connection error"),
        dotCls: "bg-amber-500 ring-2 ring-amber-500/25",
      };
    }
    if (st?.connected) {
      if (poolErr) {
        return {
          state: "error",
          label: t("Error"),
          hint: poolErr.last_error || t("Provider degraded or tripped in pool"),
          dotCls: "bg-amber-500 ring-2 ring-amber-500/25",
        };
      }
      return {
        state: "connected",
        label: t("Connected"),
        hint: st.account_hint ? `${t("Connected")} (${st.account_hint})` : t("Connected"),
        dotCls: "bg-emerald-500 ring-2 ring-emerald-500/25",
      };
    }
    return {
      state: "disconnected",
      label: t("Not signed in"),
      hint: t("Sign in with OAuth to enable this profile"),
      dotCls: "bg-red-500 ring-2 ring-red-500/20",
    };
  }

  if (prov.id === "ollama") {
    if (ollamaStatus?.state === "error") {
      return {
        state: "error",
        label: t("Error"),
        hint: ollamaStatus.detail || t("Ollama error"),
        dotCls: "bg-amber-500 ring-2 ring-amber-500/25",
      };
    }
    if (ollamaStatus?.state === "connected") {
      if (poolErr) {
        return {
          state: "error",
          label: t("Error"),
          hint: poolErr.last_error || t("Provider degraded or tripped in pool"),
          dotCls: "bg-amber-500 ring-2 ring-amber-500/25",
        };
      }
      return {
        state: "connected",
        label: t("Connected"),
        hint: ollamaStatus.detail || t("Ollama online"),
        dotCls: "bg-emerald-500 ring-2 ring-emerald-500/25",
      };
    }
    return {
      state: "disconnected",
      label: t("Disconnected"),
      hint: ollamaStatus?.detail || t("Ollama offline"),
      dotCls: "bg-red-500 ring-2 ring-red-500/20",
    };
  }

  const configured = matching.filter((p) => Boolean(p.api_key_hint));
  if (configured.length === 0) {
    return {
      state: "disconnected",
      label: t("Disconnected"),
      hint: t("No API Key set; cannot be called"),
      dotCls: "bg-red-500 ring-2 ring-red-500/20",
    };
  }
  if (poolErr) {
    return {
      state: "error",
      label: t("Error"),
      hint: poolErr.last_error || t("Provider degraded or tripped in pool"),
      dotCls: "bg-amber-500 ring-2 ring-amber-500/25",
    };
  }
  return {
    state: "connected",
    label: t("Connected"),
    hint: t("{count} profile(s) configured", { count: configured.length }),
    dotCls: "bg-emerald-500 ring-2 ring-emerald-500/25",
  };
}

export type OllamaStatus = {
  state: "connected" | "disconnected" | "error";
  detail?: string;
};

function isDefaultLocalOllamaUrl(baseUrl?: string): boolean {
  const raw = (baseUrl ?? "").trim().replace(/\/+$/, "").toLowerCase();
  if (!raw) return true;
  return (
    raw === "http://127.0.0.1:11434" ||
    raw === "http://127.0.0.1:11434/v1" ||
    raw === "http://localhost:11434" ||
    raw === "http://localhost:11434/v1"
  );
}

export function healthOf(
  p: LLMProfile,
  m?: LLMPoolMember,
  oauthMap?: Map<string, OAuthProviderStatus>,
  ollamaStatus?: OllamaStatus | null,
): Health {
  const oauthProv = oauthProviderOf(p.format);
  if (oauthProv) {
    const st = oauthMap?.get(oauthProv);
    if (st?.state === "error" || (st?.last_error && !st.connected)) {
      return {
        state: "degraded",
        label: t("Error"),
        cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
        dotCls: "bg-amber-500",
        hint: st?.last_error || t("OAuth connection error"),
      };
    }
    if (!st?.connected) {
      return {
        state: "nokey",
        label: t("Not signed in"),
        cls: "border-muted-foreground/40 text-muted-foreground",
        dotCls: "bg-muted-foreground/60",
        hint: st?.last_error || t("Sign in with OAuth to enable this profile"),
      };
    }
  } else if (p.format === "ollama") {
    if (ollamaStatus && isDefaultLocalOllamaUrl(p.base_url)) {
      if (ollamaStatus.state === "error") {
        return {
          state: "degraded",
          label: t("Error"),
          cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
          dotCls: "bg-amber-500",
          hint: ollamaStatus.detail || t("Ollama error"),
        };
      }
      if (ollamaStatus.state === "disconnected") {
        return {
          state: "nokey",
          label: t("Disconnected"),
          cls: "border-muted-foreground/40 text-muted-foreground",
          dotCls: "bg-muted-foreground/60",
          hint: ollamaStatus.detail || t("Ollama offline"),
        };
      }
    }
  } else if (!p.api_key_hint) {
    return {
      state: "nokey",
      label: t("No Key configured"),
      cls: "border-muted-foreground/40 text-muted-foreground",
      dotCls: "bg-muted-foreground/60",
      hint: t("No API Key set; cannot be called"),
    };
  }
  if (m?.state === "tripped") {
    return {
      state: "tripped",
      label: m.cooldown_secs > 0 ? t("Tripped · {cooldown}", { cooldown: cooldownText(m.cooldown_secs) }) : t("Tripped"),
      cls: "border-destructive/50 text-destructive",
      dotCls: "bg-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      state: "degraded",
      label: t("Degraded · failed {count} times", { count: m.fails }),
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      dotCls: "bg-amber-500",
      hint: m.last_error,
    };
  }
  return {
    state: "ok",
    label: t("Healthy"),
    cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400",
    dotCls: "bg-emerald-500",
  };
}

export function reasoningSummary(p: LLMProfile): string | null {
  const parts: string[] = [];
  if (p.thinking_type === "enabled") parts.push(t("Enabled"));
  else if (p.thinking_type === "disabled") parts.push(t("Disabled"));
  if (p.reasoning_effort) parts.push(p.reasoning_effort === "off" ? t("off") : p.reasoning_effort);
  return parts.length > 0 ? parts.join(" · ") : null;
}

/** Builds a saveLLMProfile payload preserving all existing fields (blank api_key keeps stored secret). */
export function profileSavePayload(
  p: LLMProfile,
  patch: Partial<{ priority: number; pool_exclude: boolean }>,
) {
  return {
    id: Number(p.id),
    name: p.name,
    format: p.format,
    model: p.model,
    base_url: p.base_url ?? "",
    proxy: p.proxy ?? "",
    api_key: "",
    rate_per_second: p.rate_per_second ?? 0,
    rate_per_minute: p.rate_per_minute ?? 0,
    context_window_k: p.context_window_k ?? 0,
    thinking_type: p.thinking_type ?? "",
    reasoning_effort: p.reasoning_effort ?? "",
    priority: patch.priority ?? p.priority ?? 0,
    pool_exclude: patch.pool_exclude ?? p.pool_exclude ?? false,
    streaming: p.streaming ?? true,
    max_tokens: p.max_tokens ?? 0,
    max_tokens_field: p.format === "openai" ? (p.max_tokens_field ?? "") : "",
    session_header_key: p.session_header_key ?? "",
    user_agent: p.user_agent ?? "",
    retry: p.retry,
  };
}
