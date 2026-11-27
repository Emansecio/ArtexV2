import { useSyncExternalStore } from "react";

export type StreamDeltaKind = "thinking" | "text";

export interface StreamDelta {
  kind: StreamDeltaKind;
  text: string;
}

export interface StreamDeltaClear {
  key: string;
  kind: StreamDeltaKind | "all";
}

// activityClearsDeltaKind returns whether a committed activity of `activityKind`
// supersedes an in-flight streaming delta of `deltaKind`. On a thinking -> text
// transition, the first text delta often arrives before the persisted thinking
// activity; clearing by kind prevents the thinking commit from wiping live text.
export function activityClearsDeltaKind(activityKind: string, deltaKind: StreamDeltaKind): boolean {
  if (activityKind === "tool_use" || activityKind === "result") return true;
  if (activityKind === "thinking") return deltaKind === "thinking";
  if (activityKind === "text") return deltaKind === "text";
  return false;
}

// clearKindForActivity maps a committed activity kind to the delta-store clear
// scope, or null when that activity does not clear streaming deltas.
export function clearKindForActivity(activityKind: string): StreamDeltaKind | "all" | null {
  if (activityKind === "tool_use" || activityKind === "result") return "all";
  if (activityKind === "thinking") return "thinking";
  if (activityKind === "text") return "text";
  return null;
}

// clearsForFreshActivities computes delta-store clears only for activities
// strictly newer than seenCursor (seq > seenCursor), so stale in-flight
// catch-up or fallback-poll responses never wipe newer live streaming deltas.
export function clearsForFreshActivities(
  key: string,
  items: readonly { seq: number; kind: string }[],
  seenCursor: number,
): StreamDeltaClear[] {
  const clears: StreamDeltaClear[] = [];
  for (const a of items) {
    if (a.seq <= seenCursor) continue;
    const clearKind = clearKindForActivity(a.kind);
    if (clearKind) clears.push({ key, kind: clearKind });
  }
  return clears;
}

const deltas = new Map<string, StreamDelta>();
const listeners = new Map<string, Set<() => void>>();

function notify(key: string) {
  const subs = listeners.get(key);
  if (!subs) return;
  for (const fn of subs) fn();
}

// appendStreamDeltas applies clears and incoming deltas, then notifies subscribed
// components synchronously. Callers already batch WebSocket frames per animation
// frame, so notifying directly avoids an extra 1-frame (16.7ms) rAF hop.
export function appendStreamDeltas(
  clears: Iterable<string | StreamDeltaClear>,
  incoming: readonly { key: string; kind: StreamDeltaKind; text: string }[],
) {
  const touched = new Set<string>();
  for (const entry of clears) {
    const key = typeof entry === "string" ? entry : entry.key;
    const kind = typeof entry === "string" ? "all" : entry.kind;
    const cur = deltas.get(key);
    if (cur && (kind === "all" || cur.kind === kind)) {
      deltas.delete(key);
      touched.add(key);
    }
  }
  for (const d of incoming) {
    const cur = deltas.get(d.key);
    deltas.set(
      d.key,
      cur && cur.kind === d.kind ? { kind: d.kind, text: cur.text + d.text } : { kind: d.kind, text: d.text },
    );
    touched.add(d.key);
  }
  for (const k of touched) notify(k);
}

export function clearStreamDelta(key: string) {
  if (deltas.delete(key)) notify(key);
}

export function clearAllStreamDeltas() {
  const keys = [...deltas.keys()];
  deltas.clear();
  for (const k of keys) notify(k);
}

export function useStreamDelta(key: string | null | undefined): StreamDelta | null {
  return useSyncExternalStore(
    (cb) => {
      if (!key) return () => {};
      let subs = listeners.get(key);
      if (!subs) {
        subs = new Set();
        listeners.set(key, subs);
      }
      subs.add(cb);
      return () => {
        subs?.delete(cb);
        if (subs && subs.size === 0) listeners.delete(key);
      };
    },
    () => (key ? (deltas.get(key) ?? null) : null),
    () => null,
  );
}
