"use client";

import * as React from "react";

import {
  ArrowRight,
  Brain,
  Check,
  Database,
  Globe,
  LayoutDashboard,
  Loader2,
  MessageSquare,
  RotateCcw,
  Shield,
  SlidersHorizontal,
  Target,
  X,
} from "lucide-react";

import { ThemeSwitcher } from "@/app/(main)/_components/sidebar/theme-switcher";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { type Locale, LOCALES, locale, setLocale, t } from "@/i18n";
import { ApiError, api } from "@/lib/api";
import { auth } from "@/lib/auth";
import { cn } from "@/lib/utils";

const WELCOME_STORAGE_KEY = "artex.welcome.v2";

type StartRoute = "/models" | "/function/tasks" | "/dashboard" | "/chat";

interface WelcomePrefs {
  operatorName: string;
  headline: string;
  subtitle: string;
  defaultRoute: StartRoute;
  autoEnter: boolean;
}

const DEFAULT_PREFS: WelcomePrefs = {
  operatorName: "",
  headline: "",
  subtitle: "",
  defaultRoute: "/models",
  autoEnter: false,
};

const DESTINATIONS: {
  route: StartRoute;
  label: string;
  desc: string;
  icon: React.ComponentType<{ className?: string }>;
}[] = [
  {
    route: "/models",
    label: t("Models & Fallbacks"),
    desc: t("Providers, OAuth, reasoning & failover"),
    icon: Brain,
  },
  {
    route: "/function/tasks",
    label: t("Tasks"),
    desc: t("Launch and monitor security runs"),
    icon: Target,
  },
  {
    route: "/dashboard",
    label: t("Dashboard"),
    desc: t("Live overview, findings & usage"),
    icon: LayoutDashboard,
  },
  {
    route: "/chat",
    label: t("Chat"),
    desc: t("Direct interactive agent sessions"),
    icon: MessageSquare,
  },
];

function loadWelcomePrefs(): WelcomePrefs {
  if (typeof window === "undefined") return DEFAULT_PREFS;
  try {
    const raw = window.localStorage.getItem(WELCOME_STORAGE_KEY);
    if (!raw) return DEFAULT_PREFS;
    const parsed = JSON.parse(raw) as Partial<WelcomePrefs>;
    const validRoute = DESTINATIONS.some((d) => d.route === parsed.defaultRoute)
      ? (parsed.defaultRoute as StartRoute)
      : DEFAULT_PREFS.defaultRoute;
    return {
      operatorName: typeof parsed.operatorName === "string" ? parsed.operatorName : "",
      headline: typeof parsed.headline === "string" ? parsed.headline : "",
      subtitle: typeof parsed.subtitle === "string" ? parsed.subtitle : "",
      defaultRoute: validRoute,
      autoEnter: Boolean(parsed.autoEnter),
    };
  } catch {
    return DEFAULT_PREFS;
  }
}

function saveWelcomePrefs(prefs: WelcomePrefs) {
  try {
    window.localStorage.setItem(WELCOME_STORAGE_KEY, JSON.stringify(prefs));
  } catch {
    /* ignore storage quota errors */
  }
}

type AuthMode = "local" | "login" | "setup";

export default function WelcomePage() {
  const [prefs, setPrefs] = React.useState<WelcomePrefs>(DEFAULT_PREFS);
  const [hydrated, setHydrated] = React.useState(false);
  const [customizing, setCustomizing] = React.useState(false);
  const [entering, setEntering] = React.useState(false);
  const [backendOk, setBackendOk] = React.useState<boolean | null>(null);
  const [version, setVersion] = React.useState("");
  const [error, setError] = React.useState("");
  const [authMode, setAuthMode] = React.useState<AuthMode>("local");
  const passwordInitializedRef = React.useRef<boolean | null>(null);
  const [username, setUsername] = React.useState("ARTEX");
  const [password, setPassword] = React.useState("");
  const [confirmPassword, setConfirmPassword] = React.useState("");
  const [setupToken, setSetupToken] = React.useState("");

  const enterWorkspace = React.useCallback(async (target: StartRoute) => {
    setEntering(true);
    setError("");
    try {
      const { token } = await api.localLogin();
      auth.setToken(token);
      window.location.replace(target);
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        let initialized = passwordInitializedRef.current;
        if (initialized === null) {
          try {
            const st = await api.authStatus();
            initialized = st.initialized;
            passwordInitializedRef.current = initialized;
          } catch {
            initialized = true;
          }
        }
        setAuthMode(initialized ? "login" : "setup");
        setError(e.message);
      } else {
        setError((e as Error).message || t("Unable to connect to the backend service"));
      }
      setEntering(false);
    }
  }, []);

  async function handlePasswordLogin(e: React.FormEvent) {
    e.preventDefault();
    setEntering(true);
    setError("");
    try {
      const { token } = await api.login(username.trim(), password);
      auth.setToken(token);
      window.location.replace(prefs.defaultRoute);
    } catch (err) {
      if (err instanceof ApiError && err.status === 403 && /not initialized/i.test(err.message)) {
        passwordInitializedRef.current = false;
        setAuthMode("setup");
      }
      setError((err as Error).message || t("Unable to connect to the backend service"));
      setEntering(false);
    }
  }

  async function handlePasswordInit(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (password.length < 12) {
      setError(t("Password must be at least 12 characters"));
      return;
    }
    if (password !== confirmPassword) {
      setError(t("The two passwords do not match"));
      return;
    }
    setEntering(true);
    try {
      const { token } = await api.authInit(password, setupToken.trim());
      auth.setToken(token);
      window.location.replace(prefs.defaultRoute);
    } catch (err) {
      if (err instanceof ApiError && err.status === 403 && /already set/i.test(err.message)) {
        passwordInitializedRef.current = true;
        setAuthMode("login");
      }
      setError((err as Error).message || t("Unable to connect to the backend service"));
      setEntering(false);
    }
  }

  React.useEffect(() => {
    const loaded = loadWelcomePrefs();
    setPrefs(loaded);
    setHydrated(true);

    const existingToken = auth.getToken();
    if (existingToken) {
      auth.setToken(existingToken);
      window.location.replace(loaded.defaultRoute);
      return;
    }

    let cancelled = false;
    void Promise.allSettled([api.health(), api.authStatus()]).then(([healthRes, statusRes]) => {
      if (cancelled) return;
      const ok = healthRes.status === "fulfilled";
      setBackendOk(ok);
      if (ok) {
        setVersion((healthRes.value.version ?? "").replace(/^v(?=\d)/, ""));
      }
      let initialized: boolean | null = null;
      if (statusRes.status === "fulfilled") {
        initialized = Boolean(statusRes.value.initialized);
        passwordInitializedRef.current = initialized;
        if (initialized) {
          setAuthMode("login");
          return;
        }
      }
      if (ok && loaded.autoEnter) {
        void enterWorkspace(loaded.defaultRoute);
      }
    });

    return () => {
      cancelled = true;
    };
  }, [enterWorkspace]);

  function updatePrefs(patch: Partial<WelcomePrefs>) {
    setPrefs((prev) => {
      const next = { ...prev, ...patch };
      saveWelcomePrefs(next);
      return next;
    });
  }

  const displayHeadline =
    prefs.headline.trim() ||
    (prefs.operatorName.trim()
      ? t("Welcome back, {name}", { name: prefs.operatorName.trim() })
      : t("Welcome to ARTEX"));

  const displaySubtitle =
    prefs.subtitle.trim() ||
    t("Local autonomous security testing console. Your models, tasks, and evidence stay on this machine.");

  const selectedDest = DESTINATIONS.find((d) => d.route === prefs.defaultRoute) ?? DESTINATIONS[0];

  return (
    <div className="flex min-h-dvh flex-col bg-background text-foreground">
      {/* Top bar — equal edges */}
      <header className="mx-auto flex w-full max-w-6xl items-center justify-between px-6 py-5 sm:px-10">
        <div className="flex items-center gap-2.5">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={28} height={28} className="size-7 shrink-0" />
          <span className="font-semibold text-base tracking-tight">ARTEX</span>
          <span className="rounded-md border bg-muted/60 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground">
            {version ? `v${version}` : "v2 local"}
          </span>
        </div>

        <div className="flex items-center gap-2">
          <Select value={locale} onValueChange={(v) => v && setLocale(v as Locale)}>
            <SelectTrigger size="sm" className="h-8 w-auto gap-1.5 text-xs" aria-label={t("Language")}>
              <Globe className="size-3.5 text-muted-foreground" />
              <SelectValue />
            </SelectTrigger>
            <SelectContent align="end">
              <SelectGroup>
                {LOCALES.map((l) => (
                  <SelectItem key={l.code} value={l.code} className="text-xs">
                    {l.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          <Button
            type="button"
            size="sm"
            variant={customizing ? "secondary" : "ghost"}
            className="gap-1.5 text-xs"
            onClick={() => setCustomizing((v) => !v)}
          >
            {customizing ? <X className="size-3.5" /> : <SlidersHorizontal className="size-3.5" />}
            {customizing ? t("Done") : t("Customize")}
          </Button>
          <ThemeSwitcher />
        </div>
      </header>

      {/* Main single-screen hero */}
      <main className="mx-auto flex w-full max-w-6xl flex-1 items-center px-6 py-8 sm:px-10">
        <div className="grid w-full grid-cols-1 items-center gap-10 lg:grid-cols-12 lg:gap-12">
          {/* Left column: Hero copy + instant entry + quick destinations */}
          <section className="flex flex-col gap-7 lg:col-span-7 animate-in fade-in-0 slide-in-from-bottom-2 duration-300">
            <div className="inline-flex w-fit items-center gap-2 rounded-full border bg-card px-3 py-1 text-xs text-muted-foreground">
              <span
                className={cn(
                  "inline-block size-1.5 rounded-full",
                  backendOk === true
                    ? "bg-emerald-500"
                    : backendOk === false
                      ? "bg-destructive"
                      : "bg-muted-foreground/50",
                )}
              />
              {backendOk === false
                ? t("Backend offline")
                : authMode === "login"
                  ? t("Password sign-in")
                  : authMode === "setup"
                    ? t("Set password and sign in")
                    : t("Local mode · No password required")}
            </div>

            <div className="space-y-3">
              <h1 className="font-semibold text-3xl tracking-tight sm:text-4xl lg:text-5xl" suppressHydrationWarning>
                {hydrated ? displayHeadline : t("Welcome to ARTEX")}
              </h1>
              <p className="max-w-xl text-base leading-relaxed text-muted-foreground" suppressHydrationWarning>
                {hydrated
                  ? displaySubtitle
                  : t("Local autonomous security testing console. Your models, tasks, and evidence stay on this machine.")}
              </p>
            </div>

            {/* Primary action */}
            {authMode === "local" ? (
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  size="lg"
                  disabled={entering || backendOk === false}
                  onClick={() => void enterWorkspace(prefs.defaultRoute)}
                  className="h-11 gap-2 px-6 transition-transform duration-150 hover:-translate-y-0.5 active:translate-y-0"
                >
                  {entering ? <Loader2 className="size-4 animate-spin" /> : null}
                  {entering ? t("Opening workspace…") : t("Open workspace")}
                  {!entering && <ArrowRight className="size-4" />}
                </Button>
                <Button
                  type="button"
                  size="lg"
                  variant="outline"
                  className="h-11 gap-2 px-5 transition-transform duration-150 hover:-translate-y-0.5 active:translate-y-0"
                  onClick={() => setCustomizing((v) => !v)}
                >
                  <SlidersHorizontal className="size-4" />
                  {t("Customize welcome")}
                </Button>
              </div>
            ) : authMode === "login" ? (
              <form
                onSubmit={(e) => void handlePasswordLogin(e)}
                className="grid max-w-md gap-3.5 rounded-2xl border bg-card p-5 shadow-xs"
              >
                <div className="grid gap-1.5">
                  <Label htmlFor="w-username">{t("Username")}</Label>
                  <Input
                    id="w-username"
                    autoComplete="username"
                    required
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="w-password">{t("Password")}</Label>
                  <Input
                    id="w-password"
                    type="password"
                    autoComplete="current-password"
                    required
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                  />
                </div>
                <div className="flex flex-wrap items-center gap-2.5 pt-1">
                  <Button type="submit" disabled={entering || backendOk === false} className="gap-2 px-5">
                    {entering ? <Loader2 className="size-4 animate-spin" /> : null}
                    {entering ? t("Signing in...") : t("Sign in")}
                    {!entering && <ArrowRight className="size-4" />}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    className="gap-2"
                    onClick={() => setCustomizing((v) => !v)}
                  >
                    <SlidersHorizontal className="size-4" />
                    {t("Customize welcome")}
                  </Button>
                </div>
              </form>
            ) : (
              <form
                onSubmit={(e) => void handlePasswordInit(e)}
                className="grid max-w-md gap-3.5 rounded-2xl border bg-card p-5 shadow-xs"
              >
                <p className="text-muted-foreground text-xs leading-relaxed">
                  {t(
                    "First time using ARTEX? Enter the setup token from the server log, then set a login password for the account (at least 12 characters)",
                  )}
                </p>
                <div className="grid gap-1.5">
                  <Label htmlFor="w-setup-token">{t("Setup token")}</Label>
                  <Input
                    id="w-setup-token"
                    autoComplete="off"
                    required
                    value={setupToken}
                    onChange={(e) => setSetupToken(e.target.value)}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="w-new-password">{t("New password")}</Label>
                  <Input
                    id="w-new-password"
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={12}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="w-confirm-password">{t("Confirm password")}</Label>
                  <Input
                    id="w-confirm-password"
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={12}
                    value={confirmPassword}
                    onChange={(e) => setConfirmPassword(e.target.value)}
                  />
                </div>
                <div className="flex flex-wrap items-center gap-2.5 pt-1">
                  <Button type="submit" disabled={entering || backendOk === false} className="gap-2 px-5">
                    {entering ? <Loader2 className="size-4 animate-spin" /> : null}
                    {entering ? t("Signing in...") : t("Set password and sign in")}
                    {!entering && <ArrowRight className="size-4" />}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    className="gap-2"
                    onClick={() => setCustomizing((v) => !v)}
                  >
                    <SlidersHorizontal className="size-4" />
                    {t("Customize welcome")}
                  </Button>
                </div>
              </form>
            )}

            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}

            {/* Quick-start destination picker */}
            <div className="space-y-2.5 pt-2">
              <p className="font-medium text-xs uppercase tracking-wider text-muted-foreground">
                {t("Start directly in")}
              </p>
              <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
                {DESTINATIONS.map((dest) => {
                  const Icon = dest.icon;
                  const active = prefs.defaultRoute === dest.route;
                  return (
                    <button
                      key={dest.route}
                      type="button"
                      disabled={entering}
                      onClick={() => {
                        updatePrefs({ defaultRoute: dest.route });
                        if (!customizing && authMode === "local") void enterWorkspace(dest.route);
                      }}
                      className={cn(
                        "group flex items-start gap-3 rounded-xl border bg-card p-3.5 text-left transition-all duration-200 hover:-translate-y-0.5 hover:border-foreground/30 hover:shadow-xs active:translate-y-0",
                        active && "border-foreground/40 ring-1 ring-foreground/15",
                      )}
                    >
                      <div
                        className={cn(
                          "mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border transition-colors",
                          active
                            ? "border-foreground/20 bg-foreground text-background"
                            : "bg-muted/60 text-muted-foreground group-hover:text-foreground",
                        )}
                      >
                        <Icon className="size-4" />
                      </div>
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center justify-between gap-2">
                          <span className="truncate font-medium text-sm">{dest.label}</span>
                          {active && <Check className="size-3.5 shrink-0 text-foreground" />}
                        </div>
                        <p className="mt-0.5 line-clamp-1 text-muted-foreground text-xs">{dest.desc}</p>
                      </div>
                    </button>
                  );
                })}
              </div>
            </div>
          </section>

          {/* Right column: Live environment card OR Inline Welcome Configurator */}
          <section className="lg:col-span-5">
            {customizing ? (
              <div className="grid gap-4 rounded-2xl border bg-card p-6 shadow-xs animate-in fade-in-0 slide-in-from-bottom-1 duration-200">
                <div className="flex items-center justify-between gap-2 border-b pb-3">
                  <div>
                    <h2 className="font-semibold text-base tracking-tight">{t("Customize welcome")}</h2>
                    <p className="text-muted-foreground text-xs">{t("Changes are saved locally in this browser.")}</p>
                  </div>
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    className="gap-1.5 text-xs text-muted-foreground"
                    onClick={() => {
                      setPrefs(DEFAULT_PREFS);
                      saveWelcomePrefs(DEFAULT_PREFS);
                    }}
                  >
                    <RotateCcw className="size-3.5" /> {t("Reset")}
                  </Button>
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="w-operator">{t("Operator name")}</Label>
                  <Input
                    id="w-operator"
                    placeholder={t("e.g. Thiago")}
                    value={prefs.operatorName}
                    onChange={(e) => updatePrefs({ operatorName: e.target.value })}
                  />
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="w-headline">{t("Custom headline (optional)")}</Label>
                  <Input
                    id="w-headline"
                    placeholder={t("Welcome to ARTEX")}
                    value={prefs.headline}
                    onChange={(e) => updatePrefs({ headline: e.target.value })}
                  />
                </div>

                <div className="grid gap-1.5">
                  <Label htmlFor="w-subtitle">{t("Subtitle (optional)")}</Label>
                  <Textarea
                    id="w-subtitle"
                    rows={2}
                    placeholder={t(
                      "Local autonomous security testing console. Your models, tasks, and evidence stay on this machine.",
                    )}
                    value={prefs.subtitle}
                    onChange={(e) => updatePrefs({ subtitle: e.target.value })}
                  />
                </div>

                <div className="flex items-center justify-between gap-4 rounded-xl border bg-muted/30 p-3.5">
                  <div className="grid gap-0.5">
                    <Label htmlFor="w-auto" className="text-sm font-medium">
                      {t("Skip welcome on next visit")}
                    </Label>
                    <p className="text-muted-foreground text-xs">
                      {t("Open {page} automatically when starting ARTEX.", { page: selectedDest.label })}
                    </p>
                  </div>
                  <Switch
                    id="w-auto"
                    checked={prefs.autoEnter}
                    onCheckedChange={(v) => updatePrefs({ autoEnter: v })}
                  />
                </div>

                <Button type="button" onClick={() => setCustomizing(false)} className="w-full">
                  {t("Save preferences")}
                </Button>
              </div>
            ) : (
              <div
                className="relative overflow-hidden rounded-2xl border bg-card p-6 shadow-xs"
                style={{
                  backgroundImage:
                    "radial-gradient(color-mix(in oklab, var(--foreground) 10%, transparent) 1px, transparent 1px)",
                  backgroundSize: "18px 18px",
                }}
              >
                <div className="relative grid gap-5 rounded-xl border bg-background/95 p-5 backdrop-blur-xs">
                  <div className="flex items-center justify-between gap-2 border-b pb-3.5">
                    <div className="flex items-center gap-2">
                      <span className="flex size-7 items-center justify-center rounded-md bg-foreground text-background">
                        <Shield className="size-3.5" />
                      </span>
                      <div>
                        <p className="font-medium text-sm leading-none">
                          {prefs.operatorName.trim() ? `${prefs.operatorName.trim()} · Local` : "ARTEX Local"}
                        </p>
                        <p className="mt-1 font-mono text-[11px] text-muted-foreground">127.0.0.1 · zero-docker</p>
                      </div>
                    </div>
                    <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-0.5 font-medium text-[11px] text-emerald-600 dark:text-emerald-400">
                      <span className="size-1.5 rounded-full bg-emerald-500" />
                      {t("Ready")}
                    </span>
                  </div>

                  <div className="grid gap-3">
                    <div className="flex items-center gap-3 rounded-lg border bg-card px-3.5 py-2.5">
                      <Brain className="size-4 shrink-0 text-muted-foreground" />
                      <div className="min-w-0 flex-1">
                        <p className="font-medium text-xs">{t("Models & Fallbacks")}</p>
                        <p className="truncate text-muted-foreground text-[11px]">
                          {t("Codex OAuth · Claude Code · OpenCode · Ollama")}
                        </p>
                      </div>
                    </div>
                    <div className="flex items-center gap-3 rounded-lg border bg-card px-3.5 py-2.5">
                      <Database className="size-4 shrink-0 text-muted-foreground" />
                      <div className="min-w-0 flex-1">
                        <p className="font-medium text-xs">{t("Local PostgreSQL store")}</p>
                        <p className="truncate text-muted-foreground text-[11px]">
                          {t("Encrypted keys & OAuth refresh tokens on disk")}
                        </p>
                      </div>
                    </div>
                    <div className="flex items-center gap-3 rounded-lg border bg-card px-3.5 py-2.5">
                      <Target className="size-4 shrink-0 text-muted-foreground" />
                      <div className="min-w-0 flex-1">
                        <p className="font-medium text-xs">{t("Default launch page")}</p>
                        <p className="truncate font-mono text-muted-foreground text-[11px]">{selectedDest.route}</p>
                      </div>
                    </div>
                  </div>
                </div>
              </div>
            )}
          </section>
        </div>
      </main>

      <footer className="mx-auto flex w-full max-w-6xl items-center justify-between px-6 py-4 text-muted-foreground text-xs sm:px-10">
        <span>{t("ARTEX · Local autonomous security workspace")}</span>
        <span className="font-mono">localhost</span>
      </footer>
    </div>
  );
}
