#!/usr/bin/env bash
#
# Shared helpers for the Argus scripts. Sourced, never executed.
#
#   source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
#
# Anything used by more than one script belongs here so the scripts stay short
# enough to actually read.

# Guard against double-sourcing when scripts call each other.
[[ -n "${ARGUS_LIB_LOADED:-}" ]] && return 0
ARGUS_LIB_LOADED=1

# Repo root, regardless of where the script was invoked from.
ARGUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export ARGUS_ROOT

WEB_DIR="$ARGUS_ROOT/web"

# ── Presentation ─────────────────────────────────────────────────────────────

if [[ -t 1 ]] && [[ -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RESET=$'\033[0m'
  RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'
  BLUE=$'\033[34m'; MAGENTA=$'\033[35m'; CYAN=$'\033[36m'
else
  BOLD=''; DIM=''; RESET=''; RED=''; GREEN=''; YELLOW=''; BLUE=''; MAGENTA=''; CYAN=''
fi

info() { printf '%s▸%s %s\n' "$CYAN" "$RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
fail() { printf '%s✗%s %s\n' "$RED" "$RESET" "$*" >&2; }
ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
die()  { fail "$*"; exit 1; }

step() { printf '\n%s%s%s\n' "$BOLD" "$*" "$RESET"; }

banner() {
  printf '\n%s  ARGUS%s %s· %s%s\n\n' "$BOLD$GREEN" "$RESET" "$DIM" "$*" "$RESET"
}

# ── Version checks ───────────────────────────────────────────────────────────

# Dotted-version comparison that does not rely on `sort -V`, which stock macOS
# does not ship.
version_lt() {
  [[ "$1" == "$2" ]] && return 1
  local IFS=.
  local -a a=($1) b=($2)
  local i
  for ((i = 0; i < ${#a[@]} || i < ${#b[@]}; i++)); do
    local x=$((10#${a[i]:-0})) y=$((10#${b[i]:-0}))
    ((x < y)) && return 0
    ((x > y)) && return 1
  done
  return 1
}

require_version() {
  local tool=$1 min=$2 actual=$3
  version_lt "$actual" "$min" && die "$tool $actual is too old — Argus needs >= $min"
  printf '  %-8s %s%s%s\n' "$tool" "$DIM" "$actual" "$RESET"
}

# Minimum versions, in one place.
require_bun() { command -v bun >/dev/null || die "bun is not installed — https://bun.sh"
                require_version bun 1.4.0 "$(bun --version)"; }

require_go()  { command -v go >/dev/null || die "go is not installed"
                require_version go 1.25.0 "$(go version | awk '{print $3}' | tr -d 'go')"; }

# Node is NOT required — Bun runs Vite, tsc and the dev server on its own, and
# the whole toolchain is verified to work with no node on PATH at all. Vite's
# package.json declares an engines constraint, but that is advisory metadata for
# npm, not something Bun enforces.
#
# So this is advisory: if a node exists and is too old, say so, because a stale
# one earlier in PATH is a confusing way to fail. Otherwise stay quiet.
check_node() {
  if ! command -v node >/dev/null 2>&1; then
    printf '  %-8s %s(none — bun provides the runtime)%s\n' "node" "$DIM" "$RESET"
    return 0
  fi
  local actual; actual=$(node --version | tr -d 'v')
  if version_lt "$actual" "22.12.0"; then
    printf '  %-8s %s%s%s %s(older than Vite 8 expects — bun is used regardless)%s\n' \
      "node" "$YELLOW" "$actual" "$RESET" "$DIM" "$RESET"
  else
    printf '  %-8s %s%s%s\n' "node" "$DIM" "$actual" "$RESET"
  fi
}

# ── Project shape ────────────────────────────────────────────────────────────

# The Go backend does not exist yet. Every script that might touch it asks
# first, so a half-built tree never breaks the UI workflow.
has_backend() { [[ -f "$ARGUS_ROOT/go.mod" ]]; }

# ── Container runtime ────────────────────────────────────────────────────────
#
# Apple `container` first, then `docker`. Podman is deliberately not consulted.

ARGUS_RUNTIME=""

detect_runtime() {
  local c
  for c in container docker; do
    command -v "$c" >/dev/null 2>&1 || continue
    ARGUS_RUNTIME="$c"
    return 0
  done
  return 1
}

# Presence is not readiness — name the actual fix when it is not up.
runtime_ready() {
  [[ -n "$ARGUS_RUNTIME" ]] || return 1
  "$ARGUS_RUNTIME" system status >/dev/null 2>&1 && return 0   # Apple container
  "$ARGUS_RUNTIME" info >/dev/null 2>&1 && return 0            # docker
  return 1
}

runtime_start_hint() {
  case "$ARGUS_RUNTIME" in
    container) echo "container system start" ;;
    docker)    echo "open -a Docker" ;;
    *)         echo "brew install --cask container" ;;
  esac
}

# ── Secrets ──────────────────────────────────────────────────────────────────

# Loads development secrets into the environment.
#
# Config files hold only ${VAR} references, so the values have to come from
# somewhere. Locally that is dev/secrets.env, which is gitignored; production
# supplies the same variables from a secret manager or mounted files.
load_secrets() {
  local env_file="$ARGUS_ROOT/dev/secrets.env"
  if [[ -f "$env_file" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "$env_file"
    set +a
    return 0
  fi
  return 1
}
