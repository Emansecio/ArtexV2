#!/usr/bin/env bash
# ARTEX native update script: optionally pulls the latest commits and rebuilds
# the self-contained artex binary (embedded UI, Go security toolbox, skills & wordlists).
# No manual DB migration needed — artex idempotently re-runs schema.sql on every start.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    cat >&2 <<'RECOVER'
[!] git pull could not fast-forward. This happens when the checkout has local
    changes. Refusing to rebuild stale code. Recover, then rerun this script:
      keep your changes:  git stash && git fetch origin && git reset --hard origin/main && git stash pop
      discard them:       git fetch origin && git reset --hard origin/main
RECOVER
    die "aborting: nothing was rebuilt"
  fi
}

update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not detected: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — use ./install.sh instead for a first-time deployment"
  ok "Go: $(go version)"

  if [ "${ARTEX_SKIP_TOOLBOX:-0}" != "1" ] && [ -x scripts/fetch-toolbox.sh ]; then
    info "refreshing embedded Go security toolbox..."
    ./scripts/fetch-toolbox.sh "$(go env GOOS)/$(go env GOARCH)"
  fi

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets..."
    ( cd web && npm ci --ignore-scripts && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling the embedded single binary..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: building a backend WITHOUT the embedded frontend (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -trimpath -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Restart the running artex process to take effect (schema migrates automatically on restart)"
}

echo "=============================="
echo "  ARTEX Native Update"
echo "=============================="
sync_repo
update_local
