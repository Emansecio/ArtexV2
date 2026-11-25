"""Iterative Bug-Hunt & Auto-Fix Workflow for ArtexV2.

Runs repeated rounds of:
  1. Parallel read-only bug hunting across all repository layers (or custom scope).
  2. Automatic fixing + targeted verification (`go test -race`, `tsc`, `test:i18n`).
  3. Re-audit on the updated codebase until a round returns zero real bugs
     (or `max_rounds` is reached as a safety cap).

Usage via `run_workflow`:
  run_workflow(
      ScriptPath="/Users/thiagomacbookpro/ArtexV2/workflows/bug_hunt.py",
      Description="Iterative bug-hunt & auto-fix loop until zero bugs remain",
      Args={
          "max_rounds": 5,
          "severity_floor": "medium",  # "critical" | "high" | "medium"
          "run_final_suite": True,
      },
  )
"""

import json

# ---- Configuration from Args ----
max_rounds = int(args.get("max_rounds", 5))
severity_floor = str(args.get("severity_floor", "medium")).lower()
run_final_suite = bool(args.get("run_final_suite", True))
custom_scope = args.get("scope")  # optional string overriding default 4-layer fan-out

SEVERITY_RANK = {"critical": 3, "high": 2, "medium": 1}
min_rank = SEVERITY_RANK.get(severity_floor, 1)

if custom_scope:
    AREAS = [
        {"name": "Custom Scope", "paths": str(custom_scope)},
    ]
else:
    AREAS = [
        {
            "name": "Runtime & LLM",
            "paths": "norma/harness/, norma/llm/, norma/compaction/, norma/noa/, norma/noaadapter/, agent/, llmpool/",
        },
        {
            "name": "Database & Cache",
            "paths": "db/",
        },
        {
            "name": "Server, Streaming, Proxy & Intercept",
            "paths": "server/, traffic/, intercept/",
        },
        {
            "name": "Frontend Next.js",
            "paths": "web/src/",
        },
    ]

# ---- JSON Schemas ----
FINDINGS_SCHEMA = {
    "type": "array",
    "items": {
        "type": "object",
        "properties": {
            "id": {"type": "string"},
            "severity": {"type": "string", "enum": ["critical", "high", "medium"]},
            "file": {"type": "string"},
            "line": {"type": "integer"},
            "symbol": {"type": "string"},
            "description": {"type": "string"},
            "fix": {"type": "string"},
        },
        "required": ["id", "severity", "file", "line", "description", "fix"],
    },
}

FIX_RESULT_SCHEMA = {
    "type": "object",
    "properties": {
        "fixed": {
            "type": "array",
            "items": {"type": "string"},
        },
        "false_positives": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "reason": {"type": "string"},
                },
                "required": ["id", "reason"],
            },
        },
        "test_passed": {"type": "boolean"},
        "test_summary": {"type": "string"},
    },
    "required": ["fixed", "false_positives", "test_passed", "test_summary"],
}

VERIFY_SCHEMA = {
    "type": "object",
    "properties": {
        "passed": {"type": "boolean"},
        "summary": {"type": "string"},
        "failures": {
            "type": "array",
            "items": {"type": "string"},
        },
    },
    "required": ["passed", "summary", "failures"],
}


def build_review_prompt(area, round_num, prev_fixes, dismissed_ids):
    prev_ctx = ""
    if round_num > 1 and prev_fixes:
        prev_ctx = (
            f"\n\nContext from Round {round_num - 1}:\n"
            f"- Fixes just applied: {json.dumps(prev_fixes, ensure_ascii=False)}\n"
            f"- Check specifically for regressions introduced by those fixes AND any remaining bugs.\n"
        )
    if dismissed_ids:
        prev_ctx += (
            f"- Previously verified as false positives / non-bugs (do NOT re-report unless you have new proof): "
            f"{json.dumps(dismissed_ids, ensure_ascii=False)}\n"
        )

    return f"""Round {round_num} Bug Hunt — {area["name"]} ({area["paths"]})

You are a Principal Systems Engineer & Security Auditor performing a STRICTLY READ-ONLY audit of:
  {area["paths"]}
{prev_ctx}
Rules:
1. Report ONLY REAL, concrete, verified bugs that can put the app's functionality, security,
   data integrity, or stability at risk: data races, deadlocks, goroutine/memory/FD leaks,
   silent data loss, wrong ordering, broken error handling, security/auth/scope bypasses,
   invalid API payloads (HTTP 400), cache staleness, or severe concrete performance bottlenecks.
2. ZERO style nitpicks, ZERO speculative or generic advice, ZERO unverified guesses.
   You MUST read the exact file and line before reporting any item.
3. Minimum severity to report: {severity_floor.upper()}.
4. If you find NO real bugs at or above {severity_floor.upper()}, return an empty array `[]`.
5. CRITICAL: You MUST end your turn by invoking the `finish` tool with the JSON array matching
   the schema. Never end with plain text without calling `finish`.

Return the JSON array matching the required schema via the `finish` tool."""


def build_fix_prompt(round_num, bugs):
    return f"""Round {round_num} Auto-Fix — Fix all {len(bugs)} verified bugs

Fix every valid bug listed below in `/Users/thiagomacbookpro/ArtexV2` surgically and without regressions.
If any item turns out to be a false positive after reading the exact code, do NOT apply a broken edit —
record it in `false_positives` with a concise technical reason.

Bugs to fix:
{json.dumps(bugs, indent=2, ensure_ascii=False)}

Requirements:
1. Read each target file before editing.
2. Apply clean, minimal, idiomatic fixes.
3. Run targeted verification for every touched layer:
   - Export `PATH="$HOME/.local/go/bin:/opt/homebrew/bin:$PATH"` before running Go commands.
   - Run `go test -race -count=1` on every modified Go package (and inside `norma/` if any `norma/` file changed).
   - Run `cd web && npx tsc --noEmit && npm run test:i18n` if any `web/` file changed.
4. Do NOT leave compilation or test failures behind.

Return the JSON summary matching the required schema."""


def build_final_verify_prompt(round_num):
    return f"""Final Verification after Round {round_num} (Zero Bugs Found)

Run the full verification suite in `/Users/thiagomacbookpro/ArtexV2`:
1. `export PATH="$HOME/.local/go/bin:/opt/homebrew/bin:$PATH"`
2. `go test -race -count=1 -timeout 90s ./llmpool/... ./agent/... ./db/... ./intercept/... ./traffic/...`
3. `go test -race -count=1 -timeout 120s ./server/...`
4. `(cd norma && go test -race -count=1 -timeout 60s ./agentcore ./compaction ./coordinator ./harness ./hook ./llm ./mcp ./memory ./noa ./permission ./plan ./skill ./subagent ./tool ./transcript)`
5. `(cd web && npx tsc --noEmit && npm run test:i18n)`

Report whether all checks passed and list any exact failures."""


# ---- Main Iterative Loop ----
history = []
prev_fixes = []
dismissed_ids = []
status = "MAX_ROUNDS_REACHED"

log(f"Starting iterative bug-hunt workflow (max_rounds={max_rounds}, severity_floor={severity_floor})")

async def run_reviewer_with_retry(area, round_num, prev_fixes, dismissed_ids):
    last_err = None
    for attempt in (1, 2):
        try:
            suffix = "" if attempt == 1 else " (retry)"
            return await agent(
                build_review_prompt(area, round_num, prev_fixes, dismissed_ids),
                schema=FINDINGS_SCHEMA,
                role=f"R{round_num} Audit: {area['name']}{suffix}",
                type_name="research",
            )
        except Exception as exc:
            last_err = exc
            log(f"Round {round_num} [{area['name']}] attempt {attempt} failed: {exc}")
    raise last_err


for round_num in range(1, max_rounds + 1):
    phase(f"Round {round_num} — Review")
    log(f"Round {round_num}: fanning out {len(AREAS)} read-only reviewer(s)...")

    area_results = await parallel(
        AREAS,
        lambda a: run_reviewer_with_retry(a, round_num, prev_fixes, dismissed_ids),
    )

    # Flatten, filter by severity_floor, and deduplicate by (file, line)
    seen = set()
    round_bugs = []
    for area, items in zip(AREAS, area_results):
        for item in items or []:
            sev = str(item.get("severity", "")).lower()
            if SEVERITY_RANK.get(sev, 0) < min_rank:
                continue
            key = (item.get("file", ""), int(item.get("line", 0)))
            if key in seen:
                continue
            seen.add(key)
            item["area"] = area["name"]
            round_bugs.append(item)

    log(f"Round {round_num}: found {len(round_bugs)} verified bug(s) >= {severity_floor}")

    if len(round_bugs) == 0:
        if run_final_suite:
            phase(f"Round {round_num} — Final Verification")
            verify = await agent(
                build_final_verify_prompt(round_num),
                schema=VERIFY_SCHEMA,
                role=f"R{round_num} Final Test Suite",
                type_name="self",
            )
            log(f"Final verification passed={verify['passed']}: {verify['summary']}")
            if verify["passed"]:
                status = "CLEAN"
                history.append({"round": round_num, "found": 0, "fixed": 0, "verified": True})
                break
            # Test suite failed even though reviewers reported 0 bugs -> fix test failures next!
            round_bugs = [
                {
                    "id": f"r{round_num}-test-{i+1}",
                    "severity": "high",
                    "file": "test-suite",
                    "line": 0,
                    "symbol": "verification",
                    "description": fail,
                    "fix": "Diagnose and fix this test/typecheck failure without weakening assertions.",
                }
                for i, fail in enumerate(verify.get("failures") or [verify["summary"]])
            ]
        else:
            status = "CLEAN"
            history.append({"round": round_num, "found": 0, "fixed": 0, "verified": True})
            break

    phase(f"Round {round_num} — Fix ({len(round_bugs)} bugs)")
    fix_res = await agent(
        build_fix_prompt(round_num, round_bugs),
        schema=FIX_RESULT_SCHEMA,
        role=f"R{round_num} Fixer ({len(round_bugs)} bugs)",
        type_name="self",
    )

    fixed_list = fix_res.get("fixed") or []
    fp_list = fix_res.get("false_positives") or []
    for fp in fp_list:
        dismissed_ids.append(f"{fp.get('id')}: {fp.get('reason')}")

    log(
        f"Round {round_num}: fixed={len(fixed_list)}, false_positives={len(fp_list)}, "
        f"tests_passed={fix_res.get('test_passed')} ({fix_res.get('test_summary')})"
    )

    history.append(
        {
            "round": round_num,
            "found": len(round_bugs),
            "fixed": len(fixed_list),
            "false_positives": len(fp_list),
            "test_passed": fix_res.get("test_passed"),
        }
    )
    prev_fixes = fixed_list

    # Stop if every reported finding was a false positive and tests already pass
    if len(fixed_list) == 0 and fix_res.get("test_passed"):
        status = "CLEAN_NO_ACTIONABLE_BUGS"
        log(f"Round {round_num}: all findings were false positives and tests pass. Stopping cleanly.")
        break

phase("Summary")
summary = {
    "status": status,
    "rounds_executed": len(history),
    "history": history,
}
log(f"Workflow finished with status={status} after {len(history)} round(s).")
print(json.dumps(summary, indent=2, ensure_ascii=False))
