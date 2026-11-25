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

// Channels for which the backend actually emits live WS invalidation frames
// (via notifyDirty on activity/status/side events). Pollers subscribed to
// other topics (e.g. "intercept", "tasks", "findings", "stats", "traffic")
// keep their configured cadence instead of relaxing to 30s.
const LIVE_WS_TOPIC_PREFIXES = new Set(["task", "conv", "side"]);

function hasLiveWSInvalidation(topics?: string[]): boolean {
  if (!topics || topics.length === 0) return false;
  return topics.every((t) => {
    const colon = t.indexOf(":");
    const prefix = colon === -1 ? t : t.slice(0, colon);
    return LIVE_WS_TOPIC_PREFIXES.has(prefix);
  });
}

export function setVisibleInterval(
  callback: () => unknown | Promise<unknown>,
  delay: number,
  opts?: VisibleIntervalOpts,
): () => void {
  let stopped = false;
  let running = false;
  let dirtyWhileRunning = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let dirtyTimer: ReturnType<typeof setTimeout> | undefined;
  const hasTopics = Boolean(opts?.topics && opts.topics.length > 0);
  const canRelaxOnWS = hasLiveWSInvalidation(opts?.topics);
  const visible = () => typeof document === "undefined" || document.visibilityState !== "hidden";
  const effectiveDelay = () => (canRelaxOnWS && isWSConnected() ? Math.max(delay, 30_000) : delay);
  const clear = () => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };
  const schedule = () => {
    clear();
    if (!stopped && visible()) timer = setTimeout(() => void run(), effectiveDelay());
  };
  const scheduleDirty = () => {
    if (stopped || !visible() || dirtyTimer !== undefined) return;
    dirtyTimer = setTimeout(() => {
      dirtyTimer = undefined;
      void run();
    }, 1200);
  };
  const run = async () => {
    clear();
    if (dirtyTimer !== undefined) {
      clearTimeout(dirtyTimer);
      dirtyTimer = undefined;
    }
    if (stopped || running || !visible()) return;
    running = true;
    dirtyWhileRunning = false;
    try {
      await callback();
    } catch {
      // Existing loaders own their error UI; transient polling failures retry.
    } finally {
      running = false;
      if (dirtyWhileRunning) {
        dirtyWhileRunning = false;
        scheduleDirty();
      } else {
        schedule();
      }
    }
  };
  const onVisibility = () => {
    clear();
    if (visible()) void run();
  };
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
  const unsubConn = canRelaxOnWS
    ? onWSConnectionChange(() => {
        if (!stopped && visible() && !running) schedule();
      })
    : () => {};
  // Opt-in WS dirty revalidation: only subscribe when explicit topics are given
  // so unrelated pollers do not fire in a wildcard storm on every activity frame.
  const unsubDirty = hasTopics
    ? onWSDirty((ch, topic) => {
        if (stopped || !visible()) return;
        const matched =
          (ch === "global" && topic === undefined) ||
          opts!.topics!.some((t) => topic === t || ch === t || ch.startsWith(`${t}:`));
        if (!matched) return;
        if (running) {
          dirtyWhileRunning = true;
          return;
        }
        scheduleDirty();
      })
    : () => {};
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
