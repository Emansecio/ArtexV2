import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const ts = require("typescript");
const deltaStorePath = path.resolve(here, "../src/lib/delta-store.ts");

function loadDeltaStoreModule() {
  const src = fs.readFileSync(deltaStorePath, "utf8");
  const withoutReact = src
    .replace(/^import\s+\{[^}]*\}\s+from\s+"react";\s*/m, "")
    .replace(/export function useStreamDelta[\s\S]*$/m, "");
  const instrumented = `${withoutReact}
export function __peek(k: string) { return deltas.get(k) ?? null; }
export function __sub(k: string, cb: () => void) {
  let s = listeners.get(k);
  if (!s) { s = new Set(); listeners.set(k, s); }
  s.add(cb);
  return () => { s?.delete(cb); };
}
`;
  const { outputText } = ts.transpileModule(instrumented, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  });
  const mod = { exports: {} };
  const fn = new Function("exports", "module", outputText);
  fn(mod.exports, mod);
  return mod.exports;
}

test("activityClearsDeltaKind isolates thinking commit from live text deltas (F2)", () => {
  const ds = loadDeltaStoreModule();
  assert.equal(ds.activityClearsDeltaKind("thinking", "thinking"), true);
  assert.equal(ds.activityClearsDeltaKind("thinking", "text"), false);
  assert.equal(ds.activityClearsDeltaKind("text", "text"), true);
  assert.equal(ds.activityClearsDeltaKind("text", "thinking"), false);
  assert.equal(ds.activityClearsDeltaKind("result", "text"), true);
  assert.equal(ds.activityClearsDeltaKind("result", "thinking"), true);
  assert.equal(ds.clearKindForActivity("result"), "all");
  assert.equal(ds.activityClearsDeltaKind("tool_use", "text"), true);
  assert.equal(ds.activityClearsDeltaKind("tool_use", "thinking"), true);
  assert.equal(ds.activityClearsDeltaKind("usage", "text"), false);
});

test("terminal result activity clears live thinking delta (F2 + F7e)", () => {
  const ds = loadDeltaStoreModule();
  ds.clearAllStreamDeltas();

  ds.appendStreamDeltas([], [{ key: "planner:1", kind: "thinking", text: "   " }]);
  assert.deepEqual(ds.__peek("planner:1"), { kind: "thinking", text: "   " });

  ds.appendStreamDeltas([{ key: "planner:1", kind: ds.clearKindForActivity("result") }], []);
  assert.equal(ds.__peek("planner:1"), null);
});

test("appendStreamDeltas preserves live text on late thinking commit and notifies synchronously (F2 + F7d)", () => {
  const ds = loadDeltaStoreModule();
  ds.clearAllStreamDeltas();

  let notifications = 0;
  const unsub = ds.__sub("conv:1", () => {
    notifications++;
  });

  // 1. Model finishes thinking and emits first text delta before thinking activity commits.
  ds.appendStreamDeltas([], [{ key: "conv:1", kind: "text", text: "Hello " }]);
  assert.equal(notifications, 1, "should notify synchronously without extra rAF");
  assert.deepEqual(ds.__peek("conv:1"), { kind: "text", text: "Hello " });

  // 2. Late thinking activity arrives: must NOT wipe the live text delta.
  const clearKind = ds.clearKindForActivity("thinking");
  assert.equal(clearKind, "thinking");
  ds.appendStreamDeltas([{ key: "conv:1", kind: clearKind }], []);
  assert.deepEqual(ds.__peek("conv:1"), { kind: "text", text: "Hello " });
  assert.equal(notifications, 1, "no spurious notification when nothing changed");

  // 3. More text streams in, then text activity commits and clears text delta.
  ds.appendStreamDeltas([], [{ key: "conv:1", kind: "text", text: "world" }]);
  assert.deepEqual(ds.__peek("conv:1"), { kind: "text", text: "Hello world" });
  assert.equal(notifications, 2);

  ds.appendStreamDeltas([{ key: "conv:1", kind: ds.clearKindForActivity("text") }], []);
  assert.equal(ds.__peek("conv:1"), null);
  assert.equal(notifications, 3);

  unsub();
});

test("clearsForFreshActivities skips already-seen activities so stale catch-up/poll cannot wipe live deltas", () => {
  const ds = loadDeltaStoreModule();
  ds.clearAllStreamDeltas();

  // Turn 2 live streaming delta is active after WS already processed seq 11 (tool_use) and 12 (tool_result).
  ds.appendStreamDeltas([], [{ key: "conv:42", kind: "text", text: "Turn 2 streaming..." }]);

  // Late in-flight catch-up response returns already-seen seq 11 (tool_use) while cursor is 12.
  const staleClears = ds.clearsForFreshActivities(
    "conv:42",
    [
      { seq: 11, kind: "tool_use" },
      { seq: 12, kind: "text" },
    ],
    12,
  );
  assert.deepEqual(staleClears, []);
  if (staleClears.length > 0) ds.appendStreamDeltas(staleClears, []);
  assert.deepEqual(ds.__peek("conv:42"), { kind: "text", text: "Turn 2 streaming..." });

  // A genuinely new activity (seq 13 > cursor 12) clears only its matching delta kind.
  const freshClears = ds.clearsForFreshActivities(
    "conv:42",
    [
      { seq: 12, kind: "tool_use" },
      { seq: 13, kind: "thinking" },
    ],
    12,
  );
  assert.deepEqual(freshClears, [{ key: "conv:42", kind: "thinking" }]);
  ds.appendStreamDeltas(freshClears, []);
  assert.deepEqual(ds.__peek("conv:42"), { kind: "text", text: "Turn 2 streaming..." });

  // A genuinely new text activity (seq 14 > cursor 13) clears the text delta.
  const commitClears = ds.clearsForFreshActivities("conv:42", [{ seq: 14, kind: "text" }], 13);
  assert.deepEqual(commitClears, [{ key: "conv:42", kind: "text" }]);
  ds.appendStreamDeltas(commitClears, []);
  assert.equal(ds.__peek("conv:42"), null);
});
