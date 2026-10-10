#!/usr/bin/env bash
#
# Rebuild the RDP pixel decoder the console runs in the browser.
#
#   ./scripts/build-wasm.sh
#
# The output, web/src/workers/rdp-pixels.wasm, is committed, so nobody needs
# this to run or build the console -- only to change internal/rdp/pixels.
# Run it whenever that package changes, and commit the result with it.
#
# TinyGo makes a 130 KB module where the standard toolchain makes 2.5 MB; the
# decoder is fetched before the first frame of every session, so the size is
# felt. TinyGo lags Go releases, so it is pointed at the newest Go it supports
# (downloaded once by the go command) rather than the one on PATH.
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

OUT="$WEB_DIR/src/workers/rdp-pixels.wasm"
PKG=./cmd/argus-pixels-wasm
TINYGO_GO="${TINYGO_GO:-go1.25.5}"

banner "rdp pixel decoder"

if command -v tinygo >/dev/null; then
  step "TinyGo"
  require_go
  info "Fetching $TINYGO_GO for TinyGo"
  goroot="$(GOTOOLCHAIN="$TINYGO_GO" go env GOROOT)"

  # The module asks for a newer Go than TinyGo accepts. The decoder imports
  # nothing outside the standard library, so a go.mod claiming the older
  # version builds it identically.
  modfile="$(mktemp -t argus-wasm-XXXXXX.mod)"
  trap 'rm -f "$modfile" "${modfile%.mod}.sum"' EXIT
  printf 'module github.com/gsoultan/argus\n\ngo 1.25\n' >"$modfile"

  GOTOOLCHAIN=local GOROOT="$goroot" PATH="$goroot/bin:$PATH" GOFLAGS="-modfile=$modfile" \
    tinygo build -o "$OUT" -target=wasip1 -buildmode=c-shared \
      -no-debug -opt=2 -scheduler=none -gc=conservative "$PKG"
else
  warn "tinygo not found -- building with the standard toolchain, which is ~20x larger"
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
fi

ok "$OUT ${DIM}($(du -h "$OUT" | cut -f1))${RESET}"
