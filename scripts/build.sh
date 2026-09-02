#!/usr/bin/env bash
#
# Production build.
#
# Typechecks before building — a build that succeeds on code that does not
# typecheck is worse than no build, because it ships.
#
#   ./scripts/build.sh
#   ./scripts/build.sh web      only the web bundle
#   ./scripts/build.sh go       only the Go binaries
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

TARGET="${1:-all}"

banner "production build"

if [[ "$TARGET" == all || "$TARGET" == web ]]; then
  [[ -d "$WEB_DIR/node_modules" ]] || die "Web dependencies missing — run ./scripts/setup.sh"

  step "Web"
  info "Typechecking"
  (cd "$WEB_DIR" && bun run typecheck) || die "Typecheck failed — not building."

  info "Bundling"
  (cd "$WEB_DIR" && bunx vite build) 2>&1 | sed 's/^/  /'

  local_size=$(du -sh "$WEB_DIR/dist" 2>/dev/null | cut -f1)
  ok "web/dist ${DIM}($local_size)${RESET}"
fi

if [[ "$TARGET" == all || "$TARGET" == go ]]; then
  if has_backend; then
    step "Go"
    require_go
    mkdir -p bin

    # Strip debug info and stamp the version so a deployed binary can say what
    # it is. Static build so the container image needs no libc.
    version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
    commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

    info "Building cmd/... ${DIM}($version)${RESET}"
    CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
      -o bin/ ./cmd/... 2>&1 | sed 's/^/  /'

    for b in bin/*; do
      [[ -f "$b" ]] && ok "$b ${DIM}($(du -h "$b" | cut -f1))${RESET}"
    done
  elif [[ "$TARGET" == go ]]; then
    die "No go.mod — there is nothing to build yet."
  fi
fi

printf '\n'
ok "Build complete"
