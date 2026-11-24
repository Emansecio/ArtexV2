import { useSyncExternalStore } from "react";

export interface StreamDelta {
  kind: "thinking" | "text";
  text: string;
}

const deltas = new Map<string, StreamDelta>();
const listeners = new Map<string, Set<() => void>>();

function notify(key: string) {
  const subs = listeners.get(key);
  if (!subs) return;
  for (const fn of subs) fn();
}

export function appendStreamDeltas(
  clearKeys: Iterable<string>,
  incoming: readonly { key: string; kind: "thinking" | "text"; text: string }[],
) {
  const touched = new Set<string>();
  for (const k of clearKeys) {
    if (deltas.delete(k)) touched.add(k);
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
