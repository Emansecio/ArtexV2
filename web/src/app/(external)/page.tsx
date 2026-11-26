"use client";

import * as React from "react";
import { useRouter } from "next/navigation";

const WELCOME_STORAGE_KEY = "artex.welcome.v2";
const ALLOWED_START_ROUTES = new Set(["/models", "/function/tasks", "/dashboard", "/chat"]);
const FALLBACK_ROUTE = "/function/tasks";

function resolveDefaultRoute(): string {
  try {
    const raw = window.localStorage.getItem(WELCOME_STORAGE_KEY);
    if (!raw) return FALLBACK_ROUTE;
    const parsed = JSON.parse(raw) as { defaultRoute?: unknown };
    if (typeof parsed?.defaultRoute === "string" && ALLOWED_START_ROUTES.has(parsed.defaultRoute)) {
      return parsed.defaultRoute;
    }
  } catch {
    // ignore malformed localStorage
  }
  return FALLBACK_ROUTE;
}

export default function Home() {
  const router = useRouter();
  React.useEffect(() => {
    router.replace(resolveDefaultRoute());
  }, [router]);
  return null;
}
