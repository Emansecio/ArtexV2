#!/usr/bin/env bash
# ARTEX native install script (macOS / Linux): configures PostgreSQL, builds the
# self-contained binary (embedded UI, Go security toolbox, default skills & wordlists),
# and starts ARTEX.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }
# json_str prints $1 as a JSON string literal: backslash, double quote and the
# control characters read -r can return are escaped.
json_str(){
  local s="$1"
  s="${s//\\/\\\\}"; s="${s//\"/\\\"}"
  s="${s//$'\t'/\\t}"; s="${s//$'\r'/\\r}"
  printf '"%s"' "$s"
}

setup_homebrew_pg(){
  command -v brew >/dev/null 2>&1 || die "Homebrew not found; install PostgreSQL manually or install Homebrew first"
  if ! brew list postgresql@17 >/dev/null 2>&1; then
    info "installing postgresql@17 via Homebrew..."
    brew install postgresql@17
  fi
  local pg_bin
  pg_bin="$(brew --prefix postgresql@17)/bin"
  export PATH="$pg_bin:$PATH"
  info "starting postgresql@17 service..."
  brew services start postgresql@17
  # Wait briefly for Postgres to accept connections.
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break
    sleep 1
  done
  pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 || die "PostgreSQL did not become ready on 127.0.0.1:5432"

  local pw
  pw="$(ask 'Password for local PostgreSQL role artex (Enter for random)' "$(rand)")"
  if ! psql -h 127.0.0.1 -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='artex'" | grep -q 1; then
    psql -h 127.0.0.1 -d postgres -v ON_ERROR_STOP=1 -v pw="$pw" -c "CREATE ROLE artex WITH LOGIN PASSWORD :'pw' SUPERUSER;" >/dev/null
  else
    psql -h 127.0.0.1 -d postgres -v ON_ERROR_STOP=1 -v pw="$pw" -c "ALTER ROLE artex WITH PASSWORD :'pw';" >/dev/null
  fi
  if ! psql -h 127.0.0.1 -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='artex'" | grep -q 1; then
    createdb -h 127.0.0.1 -O artex artex
  fi
  ok "local PostgreSQL ready (artex@127.0.0.1:5432/artex)"
  DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable
}

echo "=============================="
echo "  ARTEX Native Install"
echo "=============================="
echo "Database setup:"
echo "  1) Connect to an existing PostgreSQL"
if [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "  2) Install & start local PostgreSQL 17 via Homebrew"
fi

case "$(ask 'Choose' 1)" in
  2)
    [ "$(uname -s)" = "Darwin" ] || die "Homebrew PostgreSQL option is only available on macOS"
    setup_homebrew_pg ;;
  *)
    DB_HOST="$(ask 'Database host' 127.0.0.1)"
    DB_PORT="$(ask 'Port' 5432)"
    DB_USER="$(ask 'User' artex)"
    DB_PASS="$(ask 'Password' '')"
    DB_NAME="$(ask 'Database name' artex)"
    DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
esac

# generate config.json (string values JSON-escaped: a password may contain " or \)
case "$DB_PORT" in (*[!0-9]*|'') die "port must be a number: $DB_PORT" ;; esac
cat > config.json <<JSON
{
  "database": {
    "host": $(json_str "$DB_HOST"),
    "port": ${DB_PORT},
    "user": $(json_str "$DB_USER"),
    "password": $(json_str "$DB_PASS"),
    "dbname": $(json_str "$DB_NAME"),
    "sslmode": $(json_str "$DB_SSL")
  }
}
JSON
ok "config.json generated"

# go environment check
command -v go >/dev/null 2>&1 || die "Go not detected; please install Go (>=1.26) first: https://go.dev/dl/"
ok "Go: $(go version)"

if [ "${ARTEX_SKIP_TOOLBOX:-0}" != "1" ] && [ -x scripts/fetch-toolbox.sh ]; then
  info "preparing embedded Go security toolbox..."
  ./scripts/fetch-toolbox.sh "$(go env GOOS)/$(go env GOARCH)"
fi

# embedding the frontend requires node to produce the static assets
if command -v npm >/dev/null 2>&1; then
  info "building frontend static assets..."
  ( cd web && npm ci --ignore-scripts && npm run build:static )
  rm -rf server/webui/dist && cp -r web/out server/webui/dist
  info "compiling embedded single binary..."
  CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
else
  warn "npm not detected: building a backend WITHOUT the embedded frontend (run npm run dev separately for the frontend)"
  CGO_ENABLED=0 go build -trimpath -o artex ./cmd/artex
fi
ok "build complete → ./artex"

info "starting... (Ctrl-C to exit)"
exec ./start.sh
