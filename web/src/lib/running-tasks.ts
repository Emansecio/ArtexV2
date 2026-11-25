"use client";

import * as React from "react";

import { api } from "@/lib/api";
import { setVisibleInterval } from "@/lib/polling";

let cachedRunning = 0;
let stopSharedPoll: (() => void) | null = null;
const listeners = new Set<() => void>();

async function refreshRunningCount() {
  try {
    const res = await api.tasks();
    const count = res.tasks.filter((task) => task.status === "running").length;
    if (count !== cachedRunning) {
      cachedRunning = count;
      for (const notify of listeners) notify();
    }
  } catch {
    /* transient poll errors ignored */
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  if (listeners.size === 1) {
    void refreshRunningCount();
    stopSharedPoll = setVisibleInterval(refreshRunningCount, 8000, { topics: ["tasks"] });
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && stopSharedPoll) {
      stopSharedPoll();
      stopSharedPoll = null;
    }
  };
}

export function useRunningTasksCount(): number {
  return React.useSyncExternalStore(
    subscribe,
    () => cachedRunning,
    () => 0,
  );
}
