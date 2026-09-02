#!/usr/bin/env bash
#
# First-time setup. Idempotent — safe to re-run whenever something looks wrong.
#
# Verifies the toolchain, installs dependencies, and reports what is and is not
# ready. It deliberately does NOT start anything; that is ./scripts/dev.sh.
#
#   ./scripts/setup.sh
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

banner "first-time setup"

step "Toolchain"
require_bun
check_node
if has_backend; then
  require_go
else
  printf '  %-8s %s(no go.mod yet — skipping)%s\n' "go" "$DIM" "$RESET"
fi

step "Container runtime"
if detect_runtime; then
  if runtime_ready; then
    ok "$ARGUS_RUNTIME is running"
  else
    warn "$ARGUS_RUNTIME is installed but not running — start it with: $(runtime_start_hint)"
  fi
else
  # Not fatal: the UI runs against an in-memory mock API and needs no database.
  warn "No container runtime found."
  warn "Install one when you need Postgres: brew install --cask container"
fi

step "Web dependencies"
if [[ -d "$WEB_DIR/node_modules" ]]; then
  ok "already installed ${DIM}(delete web/node_modules to force a reinstall)${RESET}"
else
  info "Installing with bun"
  (cd "$WEB_DIR" && bun install) 2>&1 | sed 's/^/  /'
  ok "installed"
fi

if has_backend; then
  step "Go modules"
  go mod download
  ok "downloaded"
fi

step "Verifying"
if (cd "$WEB_DIR" && bun run typecheck >/dev/null 2>&1); then
  ok "web typechecks"
else
  warn "web typecheck failed — run ./scripts/check.sh to see why"
fi

cat <<EOF

${BOLD}Ready.${RESET}

  ./scripts/dev.sh        run Argus locally
  ./scripts/deps.sh up    start Postgres + MinIO (needed once a backend exists)
  ./scripts/check.sh      typecheck, vet and test
  ./scripts/build.sh      production build

EOF
