"use client";

import * as React from "react";

import { api } from "@/lib/api";
import { setVisibleInterval } from "@/lib/polling";

let cachedCount = 0;
let stopSharedPoll: (() => void) | null = null;
const listeners = new Set<() => void>();

async function refreshPendingCount() {
  try {
    const list = await api.interceptPending();
    if (list.length !== cachedCount) {
      cachedCount = list.length;
      for (const notify of listeners) notify();
    }
  } catch {
    /* transient poll errors ignored */
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  if (listeners.size === 1) {
    void refreshPendingCount();
    stopSharedPoll = setVisibleInterval(refreshPendingCount, 5000, { topics: ["intercept"] });
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && stopSharedPoll) {
      stopSharedPoll();
      stopSharedPoll = null;
    }
  };
}

export function useInterceptPendingCount(): number {
  return React.useSyncExternalStore(
    subscribe,
    () => cachedCount,
    () => 0,
  );
}
