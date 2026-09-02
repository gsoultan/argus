#!/usr/bin/env bash
#
# Everything CI would run, runnable locally with one command.
#
# Runs every check even when an early one fails, then reports the full tally —
# fixing three problems in one pass beats three round-trips.
#
#   ./scripts/check.sh
#   ./scripts/check.sh web      only the web checks
#   ./scripts/check.sh go       only the Go checks
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

TARGET="${1:-all}"
FAILED=()

run_check() {
  local label=$1; shift
  printf '  %-22s' "$label"
  local out
  if out=$("$@" 2>&1); then
    printf '%s✓%s\n' "$GREEN" "$RESET"
  else
    printf '%s✗%s\n' "$RED" "$RESET"
    printf '%s\n' "$out" | sed 's/^/      /' | tail -25
    FAILED+=("$label")
  fi
}

banner "checks"

if [[ "$TARGET" == all || "$TARGET" == web ]]; then
  [[ -d "$WEB_DIR/node_modules" ]] || die "Web dependencies missing — run ./scripts/setup.sh"
  step "Web"
  run_check "typecheck" bash -c "cd '$WEB_DIR' && bun run typecheck"
  run_check "production build" bash -c "cd '$WEB_DIR' && bunx vite build --logLevel error"
fi

if [[ "$TARGET" == all || "$TARGET" == go ]]; then
  if has_backend; then
    step "Go"
    run_check "vet" go vet ./...
    run_check "test" go test ./...
    # gofmt exits 0 even when files need formatting, so check for output instead.
    run_check "gofmt" bash -c '[[ -z "$(gofmt -l .)" ]] || { gofmt -l .; exit 1; }'
  elif [[ "$TARGET" == go ]]; then
    die "No go.mod — there is no Go code to check yet."
  fi
fi

printf '\n'
if (( ${#FAILED[@]} )); then
  fail "${#FAILED[@]} check(s) failed: ${FAILED[*]}"
  exit 1
fi
ok "All checks passed"
