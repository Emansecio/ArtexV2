import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const ts = require("typescript");
const helpersPath = path.resolve(here, "../src/app/(main)/models/_components/llm-helpers.ts");

function loadThinkingMatcher() {
  const src = fs.readFileSync(helpersPath, "utf8");
  const match = src.match(/export const NONE =[\s\S]*?export function thinkingTypeMatchesDetected[\s\S]*?\n\}/);
  assert.ok(match, "thinkingTypeMatchesDetected helper not found in llm-helpers.ts");
  // Extract only NONE and thinkingTypeMatchesDetected without UI/i18n imports.
  const noneLine = src.match(/export const NONE = .*?;/)?.[0];
  const fnBlock = src.match(/export function thinkingTypeMatchesDetected\([\s\S]*?\n\}/)?.[0];
  assert.ok(noneLine && fnBlock, "failed to extract helper declarations");
  const { outputText } = ts.transpileModule(`${noneLine}\n${fnBlock}\n`, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  });
  const mod = { exports: {} };
  const fn = new Function("exports", "module", outputText);
  fn(mod.exports, mod);
  return mod.exports;
}

test("thinkingTypeMatchesDetected flags enabled/adaptive mismatch while allowing opt-out (R3-FE-1)", () => {
  const { NONE, thinkingTypeMatchesDetected } = loadThinkingMatcher();

  // claude-sonnet-4-5 expects "enabled": "adaptive" must mismatch so "Apply defaults" appears.
  assert.equal(thinkingTypeMatchesDetected("adaptive", "enabled", true), false);
  assert.equal(thinkingTypeMatchesDetected("enabled", "enabled", true), true);
  assert.equal(thinkingTypeMatchesDetected(NONE, "enabled", true), true);
  assert.equal(thinkingTypeMatchesDetected("disabled", "enabled", true), true);

  // claude-opus-4-7 expects "adaptive": "enabled" must mismatch so "Apply defaults" appears.
  assert.equal(thinkingTypeMatchesDetected("enabled", "adaptive", true), false);
  assert.equal(thinkingTypeMatchesDetected("adaptive", "adaptive", true), true);
  assert.equal(thinkingTypeMatchesDetected(NONE, "adaptive", true), true);
  assert.equal(thinkingTypeMatchesDetected("disabled", "adaptive", true), true);

  // Model without thinking support (e.g. claude-3-5-sonnet): only NONE / disabled match.
  assert.equal(thinkingTypeMatchesDetected("enabled", "", true), false);
  assert.equal(thinkingTypeMatchesDetected("adaptive", undefined, true), false);
  assert.equal(thinkingTypeMatchesDetected(NONE, "", true), true);
  assert.equal(thinkingTypeMatchesDetected("disabled", undefined, true), true);

  // Non-authoritative source never flags mismatch.
  assert.equal(thinkingTypeMatchesDetected("adaptive", "enabled", false), true);
  assert.equal(thinkingTypeMatchesDetected("enabled", "", false), true);
});
