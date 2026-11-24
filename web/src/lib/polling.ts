import { isWSConnected, onWSConnectionChange, onWSDirty } from "@/lib/ws";

// Background browser tabs must not keep downloading snapshots. A poll that
// returns a promise completes before the next interval starts; focus refreshes
// immediately, without creating a second request while one is still running.
// When the multiplexed WS is live, intervals relax to 30s and incoming WS
// activity triggers debounced revalidation.
export interface VisibleIntervalOpts {
  /** Optional WS topics/channel prefixes that trigger debounced revalidation. */
  topics?: string[];
}

export function setVisibleInterval(
  callback: () => unknown | Promise<unknown>,
  delay: number,
  opts?: VisibleIntervalOpts,
): () => void {
  let stopped = false;
  let running = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let dirtyTimer: ReturnType<typeof setTimeout> | undefined;
  const visible = () => typeof document === "undefined" || document.visibilityState !== "hidden";
  const effectiveDelay = () => (isWSConnected() ? Math.max(delay, 30_000) : delay);
  const clear = () => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };
  const schedule = () => {
    clear();
    if (!stopped && visible()) timer = setTimeout(() => void run(), effectiveDelay());
  };
  const run = async () => {
    clear();
    if (dirtyTimer !== undefined) {
      clearTimeout(dirtyTimer);
      dirtyTimer = undefined;
    }
    if (stopped || running || !visible()) return;
    running = true;
    try {
      await callback();
    } catch {
      // Existing loaders own their error UI; transient polling failures retry.
    } finally {
      running = false;
      schedule();
    }
  };
  const onVisibility = () => {
    clear();
    if (visible()) void run();
  };
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
  const unsubConn = onWSConnectionChange(() => {
    if (!stopped && visible() && !running) schedule();
  });
  const unsubDirty = onWSDirty((ch, topic) => {
    if (stopped || running || !visible() || dirtyTimer !== undefined) return;
    if (opts?.topics && opts.topics.length > 0) {
      const matched = opts.topics.some((t) => topic === t || ch === t || ch.startsWith(`${t}:`));
      if (!matched) return;
    }
    dirtyTimer = setTimeout(() => {
      dirtyTimer = undefined;
      void run();
    }, 1200);
  });
  schedule();
  return () => {
    stopped = true;
    clear();
    if (dirtyTimer !== undefined) clearTimeout(dirtyTimer);
    dirtyTimer = undefined;
    unsubConn();
    unsubDirty();
    if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility);
  };
}
