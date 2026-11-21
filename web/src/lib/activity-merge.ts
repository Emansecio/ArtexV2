import type { Activity } from "./types";

// History and live responses can overlap or arrive out of order. A persisted
// activity's seq is its identity; replay must not duplicate rows or token totals.
export function mergeActivities(current: Activity[], incoming: Activity[]): Activity[] {
  if (incoming.length === 0) return current;
  if (current.length === 0) {
    if (incoming.length === 1) return [incoming[0]];
    const bySeq = new Map<number, Activity>();
    for (const activity of incoming) bySeq.set(activity.seq, activity);
    return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
  }
  // Fast O(K) append path for ordered live SSE deltas past the current tail.
  const lastSeq = current[current.length - 1].seq;
  let prevSeq = lastSeq;
  let allStrictlyAfter = true;
  for (let i = 0; i < incoming.length; i++) {
    const seq = incoming[i].seq;
    if (seq <= prevSeq) {
      allStrictlyAfter = false;
      break;
    }
    prevSeq = seq;
  }
  if (allStrictlyAfter) {
    return current.concat(incoming);
  }

  const bySeq = new Map<number, Activity>();
  for (const activity of current) bySeq.set(activity.seq, activity);
  for (const activity of incoming) bySeq.set(activity.seq, activity);
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}
