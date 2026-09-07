#!/usr/bin/env bash
#
# Proves a backup can actually be restored.
#
#   ./scripts/drill.sh                     take a fresh backup and drill it
#   ./scripts/drill.sh --keep <dir>        as above, keeping the backup
#   ./scripts/drill.sh --verify <dir>      drill an EXISTING backup, taking none
#
# The --verify form is what you run against last month's backup. Restoring an
# old backup is the only way to know it is still restorable, and it is not
# something to discover during an incident.
#
# An untested backup is a hope, not a control. This takes one, destroys the
# data, restores it, and verifies the audit chain still passes — the whole
# sequence, because each step alone proves nothing:
#
#   a backup that never restores is worthless
#   a restore that leaves a broken chain is worthless for compliance
#   a chain that verifies but is the wrong chain is worse than worthless
#
# Runs against a scratch database, never the live one.
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

banner "backup and restore drill"
load_secrets >/dev/null 2>&1 || true

WORK=""
KEEP=0
TAKE_BACKUP=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    # Explicit rather than positional: a bare directory reads as "drill this
    # backup" but used to mean "write the backup here", which silently
    # overwrote the thing the caller wanted checked.
    --keep)   WORK="$2"; KEEP=1; shift 2 ;;
    --verify) WORK="$2"; KEEP=1; TAKE_BACKUP=0; shift 2 ;;
    -h|--help)
      sed -n '3,21p' "${BASH_SOURCE[0]}" | sed -e 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[[ -n "$WORK" ]] || WORK="$(mktemp -d)/backup"
DRILL_DB="argus_drill"

runtime=$(command -v container || command -v docker || true)
[[ -n "$runtime" ]] || die "a container runtime is required to run the drill"

psql_admin() {
  "$runtime" exec -i argus-postgres psql -U argus -d postgres -tAc "$1"
}

cleanup() {
  psql_admin "DROP DATABASE IF EXISTS $DRILL_DB" >/dev/null 2>&1 || true
  (( KEEP )) || rm -rf "$(dirname "$WORK")" 2>/dev/null || true
}
trap cleanup EXIT

# ── 1. Seed something recognisable ───────────────────────────────────────────
step "1. Recording a marker event"
MARKER="drill-$(date -u +%s)"
./bin/argus-control --config dev/control.yaml audit verify >/dev/null 2>&1 \
  || die "the live chain is already broken; fix that before drilling"
"$runtime" exec -i argus-postgres psql -U argus -d argus -tAc \
  "SELECT count(*) FROM audit_events" | tr -d ' ' > /tmp/drill-count-before
before=$(cat /tmp/drill-count-before)
ok "$before events in the live log"

# ── 2. Back up ───────────────────────────────────────────────────────────────
if (( TAKE_BACKUP )); then
  step "2. Taking a backup"
  ./scripts/backup.sh "$WORK" 2>&1 | grep -E "Chain head|Database|Recordings|complete" | sed 's/^/  /'
else
  step "2. Using the existing backup at $WORK"
  [[ -f "$WORK/manifest.json" ]] && sed 's/^/  /' "$WORK/manifest.json"
fi
[[ -f "$WORK/argus.dump" ]] || die "$WORK has no argus.dump"
[[ -f "$WORK/chain-head.txt" ]] || die "$WORK has no chain-head.txt; it was not produced by backup.sh"
expected_head=$(cat "$WORK/chain-head.txt")

# ── 3. Restore into a scratch database ───────────────────────────────────────
step "3. Restoring into a scratch database"
psql_admin "DROP DATABASE IF EXISTS $DRILL_DB" >/dev/null
psql_admin "CREATE DATABASE $DRILL_DB" >/dev/null
"$runtime" exec -i argus-postgres pg_restore --no-owner --no-privileges \
    -U argus -d "$DRILL_DB" < "$WORK/argus.dump" 2>&1 \
    | grep -vE "does not exist, skipping|already exists" | tail -3 || true
ok "Restored into $DRILL_DB"

# ── 4. Verify the restored chain ─────────────────────────────────────────────
step "4. Verifying the restored chain"
DRILL_CONFIG=$(mktemp).yaml
sed -E "s|^database_url:.*|database_url: \"postgres://argus:argus@localhost:5433/$DRILL_DB?sslmode=disable\"|" \
    dev/control.yaml > "$DRILL_CONFIG"

out=$(./bin/argus-control --config "$DRILL_CONFIG" audit verify 2>&1) || {
  fail "The restored chain does NOT verify:"; echo "$out" | sed 's/^/    /'
  rm -f "$DRILL_CONFIG"; exit 1
}
echo "$out" | sed 's/^/  /'

restored_head=$(echo "$out" | grep -E '^\s+head' | awk '{print $2}')
restored_count=$(echo "$out" | grep -E '^INTACT' | awk '{print $2}')
rm -f "$DRILL_CONFIG"

# ── 5. Confirm it is the right chain, not merely a valid one ─────────────────
step "5. Confirming it is the same chain"
if [[ "$restored_head" != "$expected_head" ]]; then
  fail "Head mismatch — the restore produced a different chain."
  printf '    expected %s\n    got      %s\n' "$expected_head" "$restored_head"
  exit 1
fi
ok "Head matches: $(echo "$restored_head" | cut -c1-24)…"

if (( TAKE_BACKUP )) && (( restored_count < before )); then
  fail "The restore lost records: $before before, $restored_count after."
  exit 1
fi
ok "$restored_count events restored"

# ── 6. Recordings ────────────────────────────────────────────────────────────
step "6. Verifying every backed-up recording"
# Verify all of them, not a sample. Sampling one file out of fifteen turns the
# check into a one-in-fifteen chance of noticing corruption, which is worse than
# no check because it reads as a pass.
# Both formats. Counting only .cast quietly excluded every RDP recording from
# a check whose whole claim is "every backed-up recording".
#
# The list goes to a file rather than an array: this script has to run under
# the bash 3.2 that ships with macOS, which has no mapfile.
RECORDING_LIST=$(mktemp)
find "$WORK/recordings" -type f \
  \( -name '*.cast' -o -name '*.argusrdp' \) 2>/dev/null | sort > "$RECORDING_LIST"
n=$(wc -l < "$RECORDING_LIST" | tr -d ' ')
if (( n == 0 )); then
  warn "No recordings were backed up — this drill did NOT exercise recording recovery"
  warn "Confirm the object store is reachable before relying on this backup."
else
  checked=0
  failed=0
  unknown=0
  # Read on fd 3, not stdin. `container exec -i` reads stdin, and when this
  # loop was fed through stdin it consumed the remaining filenames: the drill
  # checked the first recording, printed "1 recording(s) verified", and passed
  # -- the sampling failure the comment above says it must never do.
  while IFS= read -r cast <&3; do
    id=$(basename "$cast"); id=${id%.cast}; id=${id%.argusrdp}
    # </dev/null as well as fd 3, so this stays correct if the loop is ever
    # rewritten to read from stdin again.
    head=$("$runtime" exec -i argus-postgres psql -U argus -d argus -tAc \
      "SELECT COALESCE(chain_head,'') FROM sessions WHERE id = '$id'" \
      </dev/null 2>/dev/null | tr -d ' \r')
    if [[ -z "$head" ]]; then
      # A recording with no stored head cannot be verified. Counted and
      # reported rather than silently skipped, since a backup made entirely of
      # unverifiable files would otherwise pass.
      unknown=$((unknown + 1))
      continue
    fi
    if ./bin/argus-verify "$cast" "$head" >/dev/null 2>&1; then
      checked=$((checked + 1))
    else
      failed=$((failed + 1))
      fail "  $id does not verify against its stored chain head"
    fi
  done 3< "$RECORDING_LIST"
  rm -f "$RECORDING_LIST"

  # The loop must have accounted for every file. If it did not, something ate
  # the iteration and the counts below describe a subset -- which is the one
  # way this check can lie while looking like it passed.
  if (( checked + failed + unknown != n )); then
    fail "The recording check only reached $(( checked + failed + unknown )) of $n file(s)."
    fail "Its result describes a sample, not the backup. Treat this drill as failed."
    exit 1
  fi

  if (( failed > 0 )); then
    fail "$failed of $n recording(s) are corrupt — this backup is not usable as evidence."
    exit 1
  fi
  if (( checked == 0 )); then
    fail "None of the $n recording(s) could be verified ($unknown had no stored chain head)."
    fail "A backup nothing can vouch for must not be reported as good."
    exit 1
  fi
  ok "$checked recording(s) verified against their stored chain heads"
  (( unknown > 0 )) && warn "$unknown recording(s) had no stored chain head and were not verified"
fi

printf '\n'
ok "DRILL PASSED — this backup restores and the chain verifies"
(( KEEP )) && printf '  Backup kept at %s\n' "$WORK"
printf '\n'
