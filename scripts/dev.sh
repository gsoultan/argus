#!/usr/bin/env bash
#
# Argus local development runner.
#
# Starts every service that exists in the tree and streams their logs into one
# prefixed, colourised feed. Ctrl-C tears the whole thing down, including any
# grandchildren — a half-dead Vite holding port 5273 is the most annoying way to
# start a morning.
#
# Services are DETECTED, not assumed. Today that is the web UI; when
# cmd/argus-control and cmd/argus-gateway land they are picked up automatically
# with no change here.
#
#   ./scripts/dev.sh              run everything present
#   ./scripts/dev.sh web          run only named services
#   ./scripts/dev.sh --no-deps    skip the Postgres/MinIO containers
#   ./scripts/dev.sh --clean      reinstall web deps first
#   ./scripts/dev.sh --force      kill stale port holders without asking
#   ./scripts/dev.sh --help
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

# ── Service registry ─────────────────────────────────────────────────────────
#
# name | port | colour | working dir | detect path | command
#
# `detect path` is what must exist for the service to be considered present.
# Adding a service is one line; nothing else in this script needs to change.

SERVICES=(
  "web|5273|$MAGENTA|web|web/package.json|bun run dev"
  "control|8080|$BLUE|.|cmd/argus-control|go run ./cmd/argus-control --dev"
  "gateway|2222|$GREEN|.|cmd/argus-gateway|go run ./cmd/argus-gateway --dev"
)

svc_field() { printf '%s' "$1" | cut -d'|' -f"$2"; }

# ── Argument parsing ─────────────────────────────────────────────────────────

WANTED=()
START_DEPS=1
CLEAN=0
FORCE=0

usage() {
  cat <<EOF
${BOLD}Argus dev runner${RESET}

  ./scripts/dev.sh [services...] [flags]

${BOLD}Services${RESET}   (default: every one detected in the tree)
$(for s in "${SERVICES[@]}"; do
    local_name=$(svc_field "$s" 1); local_detect=$(svc_field "$s" 5)
    if [[ -e "$local_detect" ]]; then mark="${GREEN}present${RESET}"; else mark="${DIM}not built yet${RESET}"; fi
    printf '  %-10s %s\n' "$local_name" "$mark"
  done)

${BOLD}Flags${RESET}
  --no-deps      Don't start Postgres/MinIO containers
  --clean        Reinstall web dependencies before starting
  --force        Kill whatever holds a needed port, without prompting
  -h, --help     This

${BOLD}Notes${RESET}
  The web UI runs against an in-memory mock API until argus-control exists,
  so it is fully usable on its own.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --no-deps) START_DEPS=0; shift ;;
    --clean)   CLEAN=1; shift ;;
    --force)   FORCE=1; shift ;;
    -*)        die "Unknown flag: $1 (try --help)" ;;
    *)         WANTED+=("$1"); shift ;;
  esac
done

# ── Preflight ────────────────────────────────────────────────────────────────

preflight() {
  info "Preflight"
  require_bun
  check_node
  # Only demand Go once there is Go code to run.
  [[ -e cmd/argus-control || -e cmd/argus-gateway ]] && require_go
  return 0
}

# ── Port handling ────────────────────────────────────────────────────────────

port_pid() { lsof -ti:"$1" -sTCP:LISTEN 2>/dev/null | head -1; }

# A stale dev server from a previous run is the single most common reason a
# fresh start "silently does nothing" — the new process exits and the old one
# keeps serving stale code. Always resolve it explicitly.
check_port() {
  local name=$1 port=$2
  local pid; pid=$(port_pid "$port") || true
  [[ -z "$pid" ]] && return 0

  local cmd; cmd=$(ps -p "$pid" -o comm= 2>/dev/null || echo '?')
  warn "Port $port ($name) is held by PID $pid ${DIM}($cmd)${RESET}"

  local reply
  if (( FORCE )); then
    reply=y
  elif [[ ! -t 0 ]]; then
    die "Not a TTY, refusing to kill it. Free port $port, or pass --force."
  else
    read -rp "  Kill it and continue? [Y/n] " reply
  fi

  case "${reply:-y}" in
    [Yy]*|'')
      kill "$pid" 2>/dev/null || true
      for _ in {1..25}; do
        [[ -z "$(port_pid "$port")" ]] && break
        sleep 0.2
      done
      if [[ -n "$(port_pid "$port")" ]]; then
        kill -9 "$pid" 2>/dev/null || true
        sleep 0.3
      fi
      [[ -n "$(port_pid "$port")" ]] && die "Could not free port $port"
      ok "Freed port $port"
      ;;
    *) die "Port $port is in use. Nothing started." ;;
  esac
}

# ── Local dependencies (Postgres, MinIO) ─────────────────────────────────────

# Delegated to scripts/deps.sh, which owns the single definition of local
# infrastructure. Apple container has no `compose`, so there is no compose file.
DEPS_SCRIPT="scripts/deps.sh"

start_deps() {
  # Nothing needs Postgres until a backend exists — don't make a container
  # runtime a prerequisite for working on the UI.
  if [[ ! -e cmd/argus-control ]]; then
    return 0
  fi
  [[ -x "$DEPS_SCRIPT" ]] || { warn "$DEPS_SCRIPT missing, skipping deps"; return 0; }

  # deps.sh reports its own failures with actionable messages; a dead runtime
  # should not stop the UI from coming up.
  "$DEPS_SCRIPT" up || warn "Dependencies unavailable — argus-control may fail to start."
}

# ── Process supervision ──────────────────────────────────────────────────────

declare -a CHILD_PIDS=()
SHUTTING_DOWN=0

# Kill a process and everything it spawned. `bun run dev` execs vite as a child,
# so signalling only the top pid leaves the port bound.
kill_tree() {
  local pid=$1
  local child
  for child in $(pgrep -P "$pid" 2>/dev/null || true); do
    kill_tree "$child"
  done
  kill -TERM "$pid" 2>/dev/null || true
}

cleanup() {
  (( SHUTTING_DOWN )) && return
  SHUTTING_DOWN=1
  (( ${#CHILD_PIDS[@]} )) || return   # nothing ever started
  printf '\n'
  info "Shutting down"

  local pid
  for pid in "${CHILD_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill_tree "$pid"
  done

  # Give them a moment to exit cleanly, then insist.
  local deadline=$((SECONDS + 5))
  while (( SECONDS < deadline )); do
    local alive=0
    for pid in "${CHILD_PIDS[@]:-}"; do
      [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && alive=1
    done
    (( alive )) || break
    sleep 0.2
  done
  for pid in "${CHILD_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null || true
  done

  ok "Stopped"
}
trap cleanup EXIT INT TERM

run_service() {
  local name=$1 color=$2 dir=$3 cmd=$4
  local label; label=$(printf '%-7s' "$name")

  (
    cd "$dir"
    # stdbuf keeps Vite's output flowing instead of sitting in a pipe buffer.
    # It is absent on stock macOS, so fall back to running the command directly.
    if command -v stdbuf >/dev/null 2>&1; then
      stdbuf -oL -eL bash -c "$cmd" 2>&1
    else
      bash -c "$cmd" 2>&1
    fi
  ) | while IFS= read -r line; do
        printf '%s%s%s %s│%s %s\n' "$color" "$label" "$RESET" "$DIM" "$RESET" "$line"
      done &

  CHILD_PIDS+=($!)
}

# ── Main ─────────────────────────────────────────────────────────────────────

printf '\n%s  ARGUS%s %s· local development%s\n\n' "$BOLD$GREEN" "$RESET" "$DIM" "$RESET"

preflight

if (( CLEAN )); then
  info "Reinstalling web dependencies"
  rm -rf web/node_modules
  (cd web && bun install) 2>&1 | sed 's/^/  /'
fi

if [[ ! -d web/node_modules ]]; then
  info "Installing web dependencies"
  (cd web && bun install) 2>&1 | sed 's/^/  /'
fi

# Resolve which services to run.
SELECTED=()
for svc in "${SERVICES[@]}"; do
  name=$(svc_field "$svc" 1)
  detect=$(svc_field "$svc" 5)

  if (( ${#WANTED[@]} )); then
    [[ " ${WANTED[*]} " == *" $name "* ]] || continue
    [[ -e "$detect" ]] || die "Service '$name' requested but $detect does not exist"
  else
    [[ -e "$detect" ]] || continue
  fi
  SELECTED+=("$svc")
done

(( ${#SELECTED[@]} )) || die "No services to run. Try --help."

for svc in "${SELECTED[@]}"; do
  check_port "$(svc_field "$svc" 1)" "$(svc_field "$svc" 2)"
done

(( START_DEPS )) && start_deps

printf '\n'
info "Starting ${#SELECTED[@]} service(s)"
for svc in "${SELECTED[@]}"; do
  name=$(svc_field "$svc" 1)
  port=$(svc_field "$svc" 2)
  color=$(svc_field "$svc" 3)
  dir=$(svc_field "$svc" 4)
  cmd=$(svc_field "$svc" 6)
  printf '  %s%-7s%s %sport %s · %s%s\n' "$color" "$name" "$RESET" "$DIM" "$port" "$cmd" "$RESET"
  run_service "$name" "$color" "$dir" "$cmd"
done

printf '\n  %sWeb UI%s   http://localhost:5273\n' "$BOLD" "$RESET"
if [[ -e cmd/argus-gateway ]]; then
  printf '  %sSSH%s      ssh ops:HOST@localhost -p 2222\n' "$BOLD" "$RESET"
fi
printf '  %sStop%s     Ctrl-C\n\n' "$DIM" "$RESET"

# Surface a crashed service instead of leaving the user staring at a dead feed.
while true; do
  for pid in "${CHILD_PIDS[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      fail "A service exited — shutting the rest down."
      exit 1
    fi
  done
  sleep 1
done
