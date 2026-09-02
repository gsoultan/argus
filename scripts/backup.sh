#!/usr/bin/env bash
#
# Backs up everything Argus cannot be rebuilt without: the audit chain, the
# session index, host key pins, and the recordings themselves.
#
#   ./scripts/backup.sh [output-dir]
#
# Ordering is load-bearing. The database is dumped BEFORE the recordings are
# synced, so a session sealed between the two ends up as a recording with no
# database row — a harmless orphan. Reversing it would produce a database row
# whose recording was never copied, which is silent evidence loss and exactly
# the failure a backup exists to prevent.
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

OUT="${1:-backups/$(date -u +%Y%m%dT%H%M%SZ)}"
CONFIG="${ARGUS_CONFIG:-dev/control.yaml}"

banner "backup"
load_secrets >/dev/null 2>&1 || true

DB_URL="${ARGUS_DATABASE_URL:-postgres://argus:argus@localhost:5433/argus?sslmode=disable}"
S3_ENDPOINT="${ARGUS_S3_ENDPOINT:-127.0.0.1:9000}"
S3_BUCKET="${ARGUS_S3_BUCKET:-argus-recordings}"

mkdir -p "$OUT"
chmod 700 "$OUT"

# ── 1. Record the chain head first ───────────────────────────────────────────
# Captured before the dump so it describes a state the dump is guaranteed to
# include. Restoring and finding a *longer* chain is fine; finding a shorter
# one means the dump lost records.
info "Recording the audit chain head"
if ! ./bin/argus-control --config "$CONFIG" audit verify > "$OUT/chain-before.txt" 2>&1; then
  fail "The audit chain is already broken — backing up a corrupt chain is not useful."
  cat "$OUT/chain-before.txt" | sed 's/^/  /'
  exit 1
fi
grep -E '^\s+head' "$OUT/chain-before.txt" | awk '{print $2}' > "$OUT/chain-head.txt"
ok "Chain head $(cut -c1-16 < "$OUT/chain-head.txt")…"

# ── 2. Database ──────────────────────────────────────────────────────────────
info "Dumping the database"
# Custom format so restore can be parallel and selective.
if command -v pg_dump >/dev/null 2>&1; then
  pg_dump --format=custom --no-owner --no-privileges \
          --file="$OUT/argus.dump" "$DB_URL"
else
  # Fall back to the container's client so a backup never fails for want of a
  # local Postgres install.
  runtime=$(command -v container || command -v docker || true)
  [[ -n "$runtime" ]] || die "neither pg_dump nor a container runtime is available"
  "$runtime" exec argus-postgres pg_dump --format=custom --no-owner --no-privileges \
      -U argus argus > "$OUT/argus.dump"
fi
ok "Database $(du -h "$OUT/argus.dump" | cut -f1)"

# ── 3. Recordings ────────────────────────────────────────────────────────────
info "Syncing recordings from object storage"
mkdir -p "$OUT/recordings"
if command -v mc >/dev/null 2>&1; then
  mc alias set argus-backup "http://$S3_ENDPOINT" \
     "${ARGUS_S3_ACCESS_KEY:-argus}" "${ARGUS_S3_SECRET_KEY:-argus-dev-secret}" >/dev/null
  mc mirror --overwrite "argus-backup/$S3_BUCKET" "$OUT/recordings" 2>&1 | tail -2 || true
elif runtime=$(command -v container || command -v docker) && [[ -n "$runtime" ]]; then
  # Run mc from a container rather than skipping the recordings. A backup that
  # silently omits the evidence is worse than no backup, because it is trusted.
  info "mc is not installed locally; using a container"
  minio_ip=$(container_ip "${ARGUS_MINIO_CONTAINER:-argus-minio}")
  if [[ -z "$minio_ip" ]]; then
    warn "Could not resolve the object store's address; recordings were NOT backed up"
  else
    "$runtime" run --rm --network "${ARGUS_NETWORK:-argus-dev}" \
        -v "$(cd "$OUT" && pwd)/recordings:/backup" \
        --entrypoint /bin/sh minio/mc:latest -c "
          mc alias set s3 http://$minio_ip:9000 \
             ${ARGUS_S3_ACCESS_KEY:-argus} ${ARGUS_S3_SECRET_KEY:-argus-dev-secret} >/dev/null &&
          mc mirror --overwrite s3/$S3_BUCKET /backup
        " 2>&1 | tail -2 || warn "container mc mirror failed; recordings may be incomplete"
  fi
else
  warn "Neither mc nor a container runtime is available; recordings were NOT backed up"
fi
# `find` on a missing directory exits non-zero, and with `set -e` plus
# `pipefail` that would abort the backup before the manifest is written —
# silently truncating it on any host without mc.
mkdir -p "$OUT/recordings"
recordings=$(find "$OUT/recordings" -type f 2>/dev/null | wc -l | tr -d ' ' || echo 0)
ok "Recordings: $recordings file(s)"

# ── 4. Manifest ──────────────────────────────────────────────────────────────
cat > "$OUT/manifest.json" <<JSON
{
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "chain_head": "$(cat "$OUT/chain-head.txt")",
  "recordings": $recordings,
  "database": "argus.dump",
  "argus_version": "$(./bin/argus-control --version 2>/dev/null | awk '{print $2}')",
  "note": "Restore with scripts/restore.sh, then verify the chain before relying on it."
}
JSON

printf '\n'
ok "Backup complete: $OUT"
printf '  %s\n\n' "Verify it restores before you need it: ./scripts/drill.sh $OUT"
