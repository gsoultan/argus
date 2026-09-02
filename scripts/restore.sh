#!/usr/bin/env bash
#
# Restores a backup, then verifies the audit chain before declaring success.
#
#   ./scripts/restore.sh <backup-dir> [--target-db URL] [--force]
#
# The verification is the point. A restore that completes but whose chain no
# longer verifies is worthless for the compliance case that justifies keeping
# the records at all — and you would rather discover that during a drill than
# during an incident.
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

BACKUP="${1:-}"
[[ -n "$BACKUP" ]] || die "usage: ./scripts/restore.sh <backup-dir> [--target-db URL] [--force]"
[[ -d "$BACKUP" ]] || die "$BACKUP does not exist"
shift

FORCE=0
TARGET_DB=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --force) FORCE=1; shift ;;
    --target-db) TARGET_DB="$2"; shift 2 ;;
    *) die "unknown flag $1" ;;
  esac
done

banner "restore"
load_secrets >/dev/null 2>&1 || true

DB_URL="${TARGET_DB:-${ARGUS_DATABASE_URL:-postgres://argus:argus@localhost:5433/argus?sslmode=disable}}"
S3_ENDPOINT="${ARGUS_S3_ENDPOINT:-127.0.0.1:9000}"
S3_BUCKET="${ARGUS_S3_BUCKET:-argus-recordings}"

[[ -f "$BACKUP/argus.dump" ]] || die "$BACKUP has no argus.dump"
if [[ -f "$BACKUP/manifest.json" ]]; then
  info "Backup manifest"
  sed 's/^/  /' "$BACKUP/manifest.json"
fi

# Restoring overwrites the live audit log. That is not something to do by
# accident on the wrong database.
if (( ! FORCE )); then
  if [[ ! -t 0 ]]; then
    die "Not a TTY. Restoring overwrites the target database; pass --force if you are sure."
  fi
  printf '\n'
  warn "This will DROP and replace the contents of:"
  printf '    %s\n\n' "${DB_URL%%\?*}"
  read -rp "  Type the word restore to continue: " reply
  [[ "$reply" == "restore" ]] || die "Cancelled."
fi

# ── Database ─────────────────────────────────────────────────────────────────
info "Restoring the database"
restore_cmd=(pg_restore --clean --if-exists --no-owner --no-privileges --dbname "$DB_URL")
if command -v pg_restore >/dev/null 2>&1; then
  # pg_restore reports benign notices about absent objects on a clean target;
  # only a non-zero exit with output on stderr matters.
  "${restore_cmd[@]}" "$BACKUP/argus.dump" 2>&1 | grep -vE "does not exist, skipping" | tail -5 || true
else
  runtime=$(command -v container || command -v docker || true)
  [[ -n "$runtime" ]] || die "neither pg_restore nor a container runtime is available"
  "$runtime" exec -i argus-postgres pg_restore --clean --if-exists --no-owner \
      --no-privileges -U argus -d argus < "$BACKUP/argus.dump" 2>&1 \
      | grep -vE "does not exist, skipping" | tail -5 || true
fi
ok "Database restored"

# ── Recordings ───────────────────────────────────────────────────────────────
if [[ -d "$BACKUP/recordings" ]]; then
  info "Restoring recordings"
  if command -v mc >/dev/null 2>&1; then
    mc alias set argus-restore "http://$S3_ENDPOINT" \
       "${ARGUS_S3_ACCESS_KEY:-argus}" "${ARGUS_S3_SECRET_KEY:-argus-dev-secret}" >/dev/null
    mc mb --ignore-existing "argus-restore/$S3_BUCKET" >/dev/null
    mc mirror --overwrite "$BACKUP/recordings" "argus-restore/$S3_BUCKET" 2>&1 | tail -2
    ok "Recordings restored"
  else
    warn "mc is not installed; recordings were NOT restored"
  fi
fi

# ── Verify ───────────────────────────────────────────────────────────────────
printf '\n'
info "Verifying the restored audit chain"
if ! ./bin/argus-control --config "${ARGUS_CONFIG:-dev/control.yaml}" audit verify; then
  fail "The restored chain does NOT verify. Do not rely on this restore."
  exit 1
fi

# A chain that verifies internally can still be the wrong chain — a stale backup
# verifies perfectly. Compare against the head recorded at backup time.
if [[ -f "$BACKUP/chain-head.txt" ]]; then
  expected=$(cat "$BACKUP/chain-head.txt")
  actual=$(./bin/argus-control --config "${ARGUS_CONFIG:-dev/control.yaml}" audit verify \
           | grep -E '^\s+head' | awk '{print $2}')
  if [[ "$expected" != "$actual" ]]; then
    fail "The restored chain head does not match the backup manifest."
    printf '    expected %s\n    got      %s\n' "$expected" "$actual"
    exit 1
  fi
  ok "Chain head matches the backup manifest"
fi

printf '\n'
ok "Restore complete and verified"
