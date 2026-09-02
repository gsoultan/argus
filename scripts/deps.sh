#!/usr/bin/env bash
#
# Local dependencies for Argus: Postgres and MinIO.
#
# Apple's `container` has no `compose` subcommand, so this orchestrates the
# containers directly. It is the single definition of local infrastructure —
# there is deliberately no compose file to drift out of sync with it.
#
# Runtime is auto-detected: Apple `container` first, then `docker`. All three
# share the run/volume/network flag surface this script uses.
#
#   ./scripts/deps.sh up        create and start everything, wait until ready
#   ./scripts/deps.sh down      stop containers, keep data
#   ./scripts/deps.sh reset     stop and DELETE all data
#   ./scripts/deps.sh status    what is running
#   ./scripts/deps.sh logs [postgres|minio]
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

# ── Configuration ────────────────────────────────────────────────────────────
# Dev-only credentials, deliberately obvious so nobody mistakes this for a
# production topology.

NETWORK="argus-dev"

PG_NAME="argus-postgres"
PG_IMAGE="postgres:17-alpine"
PG_VOLUME="argus-pgdata"
PG_PORT="5433"          # 5433 on the host so this never fights a local Postgres
PG_USER="argus"
PG_PASS="argus"
PG_DB="argus"

MINIO_NAME="argus-minio"
MINIO_IMAGE="minio/minio:latest"
MINIO_VOLUME="argus-minio-data"
MINIO_PORT="9000"       # S3 API
MINIO_CONSOLE="9001"    # web console
MINIO_USER="argus"
MINIO_PASS="argus-dev-secret"
MINIO_BUCKETS=(argus-recordings argus-audit-archive)

MC_IMAGE="minio/mc:latest"

# ── Runtime detection ────────────────────────────────────────────────────────

RUNTIME=""

init_runtime() {
  detect_runtime || die "No container runtime found. Install Apple container (brew install --cask container) or Docker."
  RUNTIME="$ARGUS_RUNTIME"
  runtime_ready || die "$RUNTIME is not running. Start it with: $(runtime_start_hint)"
}

rt() { "$RUNTIME" "$@"; }

# ── Helpers ──────────────────────────────────────────────────────────────────

container_state() {
  # Prints running | stopped | absent
  local name=$1 line
  line=$(rt ls -a 2>/dev/null | awk -v n="$name" '$1 == n {print; exit}') || true
  [[ -z "$line" ]] && { echo absent; return; }
  grep -qw running <<<"$line" && echo running || echo stopped
}

ensure_network() {
  rt network ls 2>/dev/null | awk '{print $1}' | grep -qx "$NETWORK" && return 0
  rt network create "$NETWORK" >/dev/null
  ok "Created network $NETWORK"
}

ensure_volume() {
  local name=$1
  rt volume ls 2>/dev/null | awk '{print $1}' | grep -qx "$name" && return 0
  rt volume create "$name" >/dev/null
  ok "Created volume $name"
}

wait_for() {
  local label=$1 timeout=$2; shift 2
  local deadline=$((SECONDS + timeout))
  printf '  waiting for %s' "$label"
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1; then
      printf ' %s✓%s\n' "$GREEN" "$RESET"
      return 0
    fi
    printf '.'
    sleep 1
  done
  printf ' %s✗%s\n' "$RED" "$RESET"
  return 1
}

# ── Services ─────────────────────────────────────────────────────────────────

start_postgres() {
  case "$(container_state "$PG_NAME")" in
    running) ok "Postgres already running"; return 0 ;;
    stopped) rt start "$PG_NAME" >/dev/null; ok "Restarted Postgres"; return 0 ;;
  esac

  ensure_volume "$PG_VOLUME"
  info "Starting Postgres ($PG_IMAGE)"

  # PGDATA must be a SUBDIRECTORY of the mount point. Apple container volumes
  # are formatted filesystems containing lost+found, and initdb refuses to
  # initialise into a non-empty directory.
  rt run -d --name "$PG_NAME" --network "$NETWORK" \
    -e POSTGRES_USER="$PG_USER" \
    -e POSTGRES_PASSWORD="$PG_PASS" \
    -e POSTGRES_DB="$PG_DB" \
    -e PGDATA=/var/lib/postgresql/data/pgdata \
    -v "$PG_VOLUME:/var/lib/postgresql/data" \
    -p "$PG_PORT:5432" \
    "$PG_IMAGE" >/dev/null

  wait_for "Postgres" 60 rt exec "$PG_NAME" pg_isready -U "$PG_USER" -d "$PG_DB" \
    || die "Postgres did not become ready. Check: ./scripts/deps.sh logs postgres"
}

start_minio() {
  case "$(container_state "$MINIO_NAME")" in
    running) ok "MinIO already running" ;;
    stopped) rt start "$MINIO_NAME" >/dev/null; ok "Restarted MinIO" ;;
    *)
      ensure_volume "$MINIO_VOLUME"
      info "Starting MinIO ($MINIO_IMAGE)"
      rt run -d --name "$MINIO_NAME" --network "$NETWORK" \
        -e MINIO_ROOT_USER="$MINIO_USER" \
        -e MINIO_ROOT_PASSWORD="$MINIO_PASS" \
        -v "$MINIO_VOLUME:/data" \
        -p "$MINIO_PORT:9000" \
        -p "$MINIO_CONSOLE:9001" \
        "$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
      ;;
  esac

  wait_for "MinIO" 60 curl -fsS "http://127.0.0.1:$MINIO_PORT/minio/health/live" \
    || die "MinIO did not become ready. Check: ./scripts/deps.sh logs minio"
}

# Apple container does not run a DNS server for user-defined networks, so
# container-to-container name resolution fails. (`container system dns create`
# would fix it but needs sudo, which a dev script has no business demanding.)
# Addressing by IP works identically on Apple container and Docker.
container_ip() {
  local name=$1
  case "$RUNTIME" in
    container)
      rt inspect "$name" 2>/dev/null | python3 -c '
import json, sys
try:
    nets = json.load(sys.stdin)[0].get("status", {}).get("networks", [])
    print(nets[0]["ipv4Address"].split("/")[0] if nets else "")
except Exception:
    print("")' 2>/dev/null
      ;;
    docker)
      rt inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name" 2>/dev/null
      ;;
  esac
}

# Session recordings need their bucket to exist before the first write, so this
# runs on every `up` — mc mb --ignore-existing is idempotent.
create_buckets() {
  info "Ensuring buckets"

  local ip; ip=$(container_ip "$MINIO_NAME")
  if [[ -z "$ip" ]]; then
    warn "Could not determine MinIO's address — skipping bucket creation."
    warn "Create them at http://localhost:$MINIO_CONSOLE (${MINIO_USER}/${MINIO_PASS})"
    return 0
  fi

  local script="mc alias set local http://$ip:9000 $MINIO_USER $MINIO_PASS"
  local b
  for b in "${MINIO_BUCKETS[@]}"; do
    script+=" && mc mb --ignore-existing local/$b"
  done

  local out
  if out=$(rt run --rm --network "$NETWORK" --entrypoint /bin/sh "$MC_IMAGE" -c "$script" 2>&1); then
    for b in "${MINIO_BUCKETS[@]}"; do ok "bucket $b"; done
  else
    warn "Could not create buckets automatically:"
    printf '%s\n' "$out" | tail -3 | sed 's/^/    /'
    warn "Create them at http://localhost:$MINIO_CONSOLE (${MINIO_USER}/${MINIO_PASS})"
  fi
}

# ── Commands ─────────────────────────────────────────────────────────────────

cmd_up() {
  init_runtime
  info "Using ${BOLD}$RUNTIME${RESET}"
  ensure_network
  start_postgres
  start_minio
  create_buckets
  printf '\n'
  ok "Dependencies ready"
  cat <<EOF

  ${BOLD}Postgres${RESET}  postgres://$PG_USER:$PG_PASS@localhost:$PG_PORT/$PG_DB
  ${BOLD}MinIO${RESET}     http://localhost:$MINIO_PORT  ${DIM}(console :$MINIO_CONSOLE — $MINIO_USER / $MINIO_PASS)${RESET}

EOF
}

cmd_down() {
  init_runtime
  local name
  for name in "$PG_NAME" "$MINIO_NAME"; do
    if [[ "$(container_state "$name")" == running ]]; then
      rt stop "$name" >/dev/null 2>&1 || true
      ok "Stopped $name"
    fi
  done
  info "Data volumes kept. Use 'reset' to delete them."
}

cmd_reset() {
  init_runtime

  if [[ -t 0 ]]; then
    read -rp "  Delete ALL local Argus database and recording data? [y/N] " reply
    [[ "${reply:-n}" =~ ^[Yy] ]] || die "Cancelled."
  fi

  local name
  for name in "$PG_NAME" "$MINIO_NAME"; do
    rt rm -f "$name" >/dev/null 2>&1 || true
  done
  for name in "$PG_VOLUME" "$MINIO_VOLUME"; do
    rt volume rm "$name" >/dev/null 2>&1 || true
  done
  rt network rm "$NETWORK" >/dev/null 2>&1 || true
  ok "Local data wiped"
}

cmd_status() {
  init_runtime
  printf '\n  %-18s %-10s %s\n' "SERVICE" "STATE" "ENDPOINT"
  printf '  %-18s %-10s %s\n' "$PG_NAME" "$(container_state "$PG_NAME")" "localhost:$PG_PORT"
  printf '  %-18s %-10s %s\n' "$MINIO_NAME" "$(container_state "$MINIO_NAME")" "localhost:$MINIO_PORT"
  printf '\n'
}

cmd_logs() {
  init_runtime
  local which=${1:-}
  case "$which" in
    postgres|pg) rt logs "$PG_NAME" ;;
    minio)       rt logs "$MINIO_NAME" ;;
    *)           die "Usage: ./scripts/deps.sh logs [postgres|minio]" ;;
  esac
}

case "${1:-up}" in
  up)     cmd_up ;;
  down)   cmd_down ;;
  reset)  cmd_reset ;;
  status) cmd_status ;;
  logs)   shift; cmd_logs "$@" ;;
  -h|--help|help)
    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^#\ \?//'
    ;;
  *) die "Unknown command: $1 (try --help)" ;;
esac
