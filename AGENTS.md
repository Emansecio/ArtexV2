# AGENTS.md — ArtexV2 Engineering Rules & Policies

This document defines the mandatory rules, architectural invariants, security boundaries, and
verification policies for all AI agents and contributors working on **ArtexV2**.

Follow these rules strictly to prevent regressions across the Go backend, `norma/` SDK module,
PostgreSQL layer, Next.js static frontend, Patchright MCP integration, and native macOS app.

---

## 1. Environment & Zero-Docker Policy

- **Zero Docker**: ArtexV2 runs natively without Docker. Never introduce Docker, `docker compose`,
  or container-only assumptions into runtime code, setup scripts, or tests.
- **Local Toolchain `PATH`**: Always ensure Go 1.26+, Homebrew PostgreSQL 17, Node/npm, and
  `~/.local/bin` are in `PATH` when running commands:
  ```bash
  export PATH="$HOME/.local/go/bin:/opt/homebrew/opt/postgresql@17/bin:/opt/homebrew/bin:$HOME/.local/bin:$PATH"
  ```
- **Local Database**: Native Homebrew `postgresql@17` (`ARTEX_PG_DSN="postgres://${USER}@127.0.0.1:5432/artex?sslmode=disable"`).
- **Local Ports**:
  - Production / macOS app (`Artex.app`): HTTP/UI `127.0.0.1:8797`, MITM Proxy `127.0.0.1:8798`.
  - Default CLI fallback: `:8787` / `:8788`.

---

## 2. Repository & Module Boundaries

ArtexV2 contains **two separate Go modules** and a **static-exported Next.js frontend**:

1. **Root Go module (`go.mod`)**: `cmd/artex`, `server`, `agent`, `db`, `oauth`, `llmpool`,
   `traffic`, `guard`, `intercept`, `enrich`, `evidence`, `notify`, `report`, `mcphttp`, etc.
2. **Embedded SDK Go module (`norma/go.mod`)**: `norma/agentcore`, `norma/harness`, `norma/llm`,
   `norma/tool`, `norma/memory`, `norma/permission`, `norma/transcript`.
   - `go test ./...` at the repo root does **NOT** test `norma/`. You must explicitly run
     `(cd norma && go test -race ./...)`.
3. **Frontend (`web/`)**: Next.js 16 + React 19 + Tailwind CSS 4 with `output: "export"` and
   `trailingSlash: true`. Built static assets are synced and pre-compressed into
   `server/webui/dist/` and embedded into `./artex` via `-tags embedui`.
4. **Native macOS Launcher (`desktop/macos/`)**: Swift + `WKWebView` app installed at
   `/Applications/Artex.app`. It executes `/Users/thiagomacbookpro/ArtexV2/artex` directly — it
   does not bundle `./artex`.
5. **Patchright MCP (`tools/patchright-mcp/`)**: Node.js wrapper exposing Patchright's built-in
   MCP server via `patchright-mcp` (symlinked to `~/.local/bin/patchright-mcp`).

---

## 3. Browser Automation Policy (Patchright-First)

- **Patchright is the sole default browser engine**: The built-in `browser` MCP server in
  `db/browser_mcp.go` must use `patchright-mcp` (`tools/patchright-mcp/`).
- **Avoid Playwright**: Do **not** add `@playwright/mcp`, `playwright`, or `playwright-core`
  dependencies or revert `db/browser_mcp.go` back to Playwright. Playwright may only be used as an
  explicit, opt-in fallback if strictly necessary.
- **No CAPTCHA / Anti-Bot Bypass**: Never implement, script, or instruct agents to solve, compute,
  or bypass CAPTCHAs, Turnstile, or human-verification gates.

---

## 4. Security & Authentication Invariants (Non-Negotiable)

Never weaken or bypass any of these invariants:

1. **CORS & WebSocket Origin Restrictions (`server/server.go`, `server/ws_hub.go`)**:
   - Never re-introduce wildcard `Access-Control-Allow-Origin: *` or blanket loopback WebSocket
     acceptance.
   - Only allow same-origin requests and origins explicitly configured in `ARTEX_ALLOWED_ORIGINS`.
2. **Loopback Bypass & Anti-CSRF (`server/auth.go`)**:
   - `authLocal` must block cross-site requests (`Sec-Fetch-Site: cross-site`) and reverse-proxied
     requests carrying `X-Forwarded-For` or `Forwarded`.
   - When an admin password is set, loopback passwordless bypass requires
     `ARTEX_ALLOW_LOCAL_BYPASS=1` on a direct loopback connection.
   - `/api/auth/status` must expose `local_bypass` accurately so `/welcome` reflects local mode.
3. **SSRF & Target Gate (`guard/`, `egress/`, `server/`)**:
   - Keep `IsNonPublicAddr` blocking private, loopback, link-local, carrier-grade NAT, and
     IPv4-mapped IPv6 prefixes.
   - Keep `NormalizeTarget` fail-closed on malformed or out-of-scope targets.
4. **Secrets at Rest (`db/secrets.go`)**:
   - Every new database column storing API keys, OAuth tokens, proxies, credentials, or sensitive
     configs **must** be registered in `secretColumns` and encrypted via AES-256-GCM.
   - Never log raw tokens, cookies, API keys, or decrypted secrets.
5. **Prompt-Injection Boundary**:
   - Keep `UNTRUSTED_OUTPUT` nonce-tagged framing enabled by default (`ARTEX_UNTRUSTED_BOUNDARY`).

---

## 5. Database, Migrations & Concurrency Rules

1. **Sequential Idempotent Migrations (`db/migrations/`)**:
   - Every schema change requires a new numbered migration (`0017_<name>.sql`, etc.) using
     idempotent DDL (`IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`).
   - Always update `db/schema.sql` to match the final schema so fresh databases match migrated ones.
   - Never modify already-shipped migration files (`0001`..`0016`).
2. **Graph Cache Coherence (`db/`)**:
   - Always bump `graphVer` *after* transactional mutations to exploration/asset graph tables so
     cached snapshots never serve stale state.
3. **Locking & Pool Hygiene**:
   - Use `lockCompanyScopeRead` (shared lock) for concurrent asset upserts; reserve exclusive locks
     for destructive/scope-redefining operations.
   - Never open a nested DB connection or run queries on the same pool while iterating an open
     `sql.Rows` / `pgx.Rows` (collect rows first, close rows, then run follow-up queries).
4. **Traffic FTS5 Protection (`traffic/traffic.go`)**:
   - Keep `collapseIndexRuns` (capping identical consecutive bytes at 256) before inserting bodies
     into SQLite FTS5 trigram indexes to prevent catastrophic indexing stalls.

---

## 6. Agent Runtime, Streaming, OAuth & LLM Rules

1. **Tool Execution Ordering (`norma/harness/streaming.go`)**:
   - Preserve the FIFO exclusivity barrier between concurrent read-only tools and exclusive/mutating
     tools.
2. **Parallel Tool Call Indexing (`norma/llm/`)**:
   - Every LLM wire adapter (Anthropic, OpenAI, Devin, Antigravity, etc.) **must** propagate the
     per-call `ToolIndex` on `SEToolInputJSON` and `SEToolUseStop` events so parallel tool calls
     never interleave or corrupt arguments.
3. **WebSocket Event Ordering (`server/ws_hub.go`)**:
   - Always emit streaming `delta` events before persisted `activity` events to avoid duplicate or
     out-of-order UI rendering.
4. **Usage Accounting (`server/engine.go`)**:
   - Inside `usageWriteQueue` / usage sinks, use `emitActivitySync` (never recursive `emitActivity`)
     to avoid infinite feedback loops and dropped `llm_usage` rows.
5. **Prompt Caching Stability**:
   - Keep system prompt prefixes and tool definitions deterministic and byte-stable across turns so
     Anthropic, OpenAI, and Devin (`EPHEMERAL=1`) prompt caches remain warm.
6. **Failover Contract (`llmpool/`)**:
   - LLM providers must return `*llm.HTTPStatusError` (with status code and retryability metadata)
     for HTTP failures so `llmpool` rotation and failover work uniformly.
7. **OAuth Manager (`oauth/manager.go`, `oauth/antigravity.go`, `oauth/devin.go`)**:
   - In `loadLocked`, only set `m.loaded[provider] = true` when `err == nil` (never cache transient
     DB errors).
   - Always guard `pl.cb != nil` before invoking pending login callbacks.
   - Keep CLI-based OAuth flows (`agy acp --login`) bounded with a single reaper goroutine and
     proper process cleanup.
   - Ensure `llm.ShutdownAntigravityPool()` remains wired into `cmd/artex/main.go` graceful shutdown.

---

## 7. Frontend & i18n Rules (`web/`)

1. **Static Export Compatibility**:
   - `web/` builds with `next build` static export (`trailingSlash: true`). Never introduce
     server-only Next.js features (Server Actions, runtime `headers()`/`cookies()`, dynamic SSR
     routes without static params).
2. **4-Locale i18n Parity (Mandatory)**:
   - Supported locales: `en`, `pt-BR`, `tr`, `zh-CN` (`web/src/i18n/messages/`).
   - Every new or edited user-facing string **must** be added to all 4 locale files with accurate
     translations (no untranslated English placeholders in `pt-BR`/`tr`/`zh-CN`).
   - Run `npm run sync:locales && npm run test:i18n` after every UI text change.
3. **No Module-Top-Level `t()` Calls**:
   - **Never** evaluate `t("...")` at module initialization time (e.g. top-level `const ITEMS = [{ title: t(...) }]`).
   - `I18nBootScript` loads the active locale asynchronously on boot; module-level `t()` freezes
     English labels until a manual language toggle.
   - Always evaluate `t()` inside React components or render-time factory functions (e.g.
     `getSidebarItems()` in `web/src/navigation/sidebar/sidebar-items.ts`).

---

## 8. Mandatory Verification & Build Checklist

Before considering any change complete (and before committing), run all relevant checks:

```bash
export PATH="$HOME/.local/go/bin:/opt/homebrew/opt/postgresql@17/bin:/opt/homebrew/bin:$HOME/.local/bin:$PATH"

# 1. Backend tests (root module) with race detector
go test -race ./...

# 2. SDK tests (norma/ module) with race detector
(cd norma && go test -race ./...)

# 3. Frontend i18n parity & TypeScript check (whenever web/ is touched)
(cd web && npm run sync:locales && npm run test:i18n && npx tsc --noEmit)

# 4. Full production binary build (when shipping or verifying UI/Go integration)
ARTEX_OUTPUT=./artex ./build.sh
```

### Restarting `/Applications/Artex.app` After Rebuilding

`Artex.app` checks `http://127.0.0.1:8797/api/health` on launch and reuses any already-running
`./artex` process. After rebuilding `./artex`, **always stop the old process first**:

```bash
pkill -TERM -f "/ArtexV2/artex" || true
open /Applications/Artex.app
```

### Deep Multi-Layer Audits

For major refactors or release readiness, run the iterative 4-layer audit workflow in
[`workflows/bug_hunt.py`](workflows/bug_hunt.py) until all layers report `CLEAN`.

---

## 9. Git & Hygiene Rules

- **Never commit secrets or local state**: `secret.key`, `jwt.key`, `.env`, `data/`, `artex`,
  `tools/patchright-mcp/node_modules/`, and temporary artifacts must stay untracked / `.gitignore`d.
- **No silent regressions**: If a test fails, fix the root cause — never skip, weaken, or delete a
  test to make CI green.
- **Keep docs synchronized**: When adding a provider, migration, MCP server, or environment flag,
  update [`README.md`](README.md) and this `AGENTS.md` in the same change.
