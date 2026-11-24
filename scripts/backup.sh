#!/usr/bin/env bash
# Backup the ARTEX PostgreSQL database (and optionally the data directory) with rotation.
#
# Usage:
#   scripts/backup.sh [--dsn DSN] [--config FILE] [--with-data] [--keep N] [--output-dir DIR]
#
#   --dsn          PostgreSQL connection URI (default: $ARTEX_PG_DSN or config.json)
#   --config       config.json path (default: config.json)
#   --with-data    also archive the local data/ directory (worker artifacts, evidence) as a tar.gz
#   --keep N       number of db dumps to keep (default 14; data archives rotate to the same count)
#   --output-dir   where to write (default backups/, relative to the repo root)
#
# Restore:
#   gunzip -c backups/artex-db-<ts>.sql.gz | psql "$ARTEX_PG_DSN"
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { sed -n '2,14p' "$0" | sed 's/^# \?//'; }

KEEP=14
OUTPUT_DIR="backups"
WITH_DATA=0
DSN="${ARTEX_PG_DSN:-}"
CONFIG="config.json"

while [ $# -gt 0 ]; do
  case "$1" in
    --dsn) DSN="${2:?}"; shift 2 ;;
    --config) CONFIG="${2:?}"; shift 2 ;;
    --keep) KEEP="${2:?}"; shift 2 ;;
    --output-dir) OUTPUT_DIR="${2:?}"; shift 2 ;;
    --with-data) WITH_DATA=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$KEEP" in (*[!0-9]*|''|0*) echo "--keep must be a positive integer" >&2; exit 2 ;; esac

command -v pg_dump >/dev/null 2>&1 || { echo "pg_dump not found on PATH (install PostgreSQL client tools)" >&2; exit 1; }

declare -a DUMP_ARGS=()
if [ -n "$DSN" ]; then
  DUMP_ARGS=("$DSN")
elif [ -f "$CONFIG" ] && command -v python3 >/dev/null 2>&1; then
  HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
  while IFS=$'\t' read -r key val; do
    case "$key" in
      DSN)      DSN="$val" ;;
      HOST)     HOST="$val" ;;
      PORT)     PORT="$val" ;;
      USER)     USER="$val" ;;
      PASSWORD) DBPASS="$val" ;;
      DBNAME)   DBNAME="$val" ;;
      SSLMODE)  SSLMODE="$val" ;;
    esac
  done < <(python3 - "$CONFIG" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])).get("database", {})
if d.get("dsn"):
    print("DSN\t" + str(d["dsn"]))
else:
    for k in ("host","port","user","password","dbname","sslmode"):
        if d.get(k) is not None:
            print(k.upper() + "\t" + str(d[k]))
PY
  )
  if [ -n "$DSN" ]; then
    DUMP_ARGS=("$DSN")
  else
    [ -n "$USER" ] && [ -n "$DBNAME" ] || { echo "missing database credentials in $CONFIG (or pass --dsn / ARTEX_PG_DSN)" >&2; exit 1; }
    DUMP_ARGS=(-h "${HOST:-127.0.0.1}" -p "${PORT:-5432}" -U "$USER" "$DBNAME")
    [ -n "$SSLMODE" ] && export PGSSLMODE="$SSLMODE"
    [ -n "$DBPASS" ] && export PGPASSWORD="$DBPASS"
  fi
else
  echo "set ARTEX_PG_DSN, pass --dsn, or provide $CONFIG" >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"
TS=$(date -u +%Y%m%dT%H%M%SZ)
DB_OUT="$OUTPUT_DIR/artex-db-$TS.sql.gz"
if [ -e "$DB_OUT" ]; then
  echo "refusing to overwrite $DB_OUT" >&2
  exit 1
fi

if ! pg_dump "${DUMP_ARGS[@]}" | gzip > "$DB_OUT"; then
  rm -f "$DB_OUT"
  echo "pg_dump failed (is PostgreSQL running and reachable?)" >&2
  exit 1
fi
gzip -t "$DB_OUT"
echo "wrote $DB_OUT ($(du -h "$DB_OUT" | cut -f1))"

if [ "$WITH_DATA" -eq 1 ]; then
  DATA_OUT="$OUTPUT_DIR/artex-data-$TS.tar.gz"
  if ! tar -czf "$DATA_OUT" data 2>"$OUTPUT_DIR/.tar.err"; then
    rm -f "$DATA_OUT"
    echo "data archive failed:" >&2
    head -5 "$OUTPUT_DIR/.tar.err" >&2
    rm -f "$OUTPUT_DIR/.tar.err"
    exit 1
  fi
  rm -f "$OUTPUT_DIR/.tar.err"
  echo "wrote $DATA_OUT ($(du -h "$DATA_OUT" | cut -f1))"
fi

prune() {
  ls -1t "$OUTPUT_DIR"/$1 2>/dev/null | tail -n +$((KEEP + 1)) | while IFS= read -r old; do
    echo "pruning $old"
    rm -f "$old"
  done
}
prune 'artex-db-*.sql.gz'
[ "$WITH_DATA" -eq 1 ] && prune 'artex-data-*.tar.gz'
echo "backup complete ($KEEP newest kept)"
