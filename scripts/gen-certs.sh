#!/usr/bin/env bash
#
# Generates a local PKI for development.
#
# Exists so "TLS is fiddly to set up locally" never becomes the reason a
# deployment ends up running without it. Production uses a real CA.
#
#   ./scripts/gen-certs.sh [output-dir]
#
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ARGUS_ROOT"

DIR="${1:-dev/certs}"
banner "development PKI"

if [[ -f "$DIR/ca.crt" ]]; then
  warn "$DIR already contains a CA — delete it first to regenerate"
  exit 0
fi

# The address a container reaches this host on.
#
# An agent or gateway running in a container dials the host across the runtime's
# bridge, and a server certificate that does not name that address is refused --
# correctly. It differs per runtime and per machine, so it is discovered rather
# than listed. Set ARGUS_EXTRA_SANS to add more.
EXTRA=()
runtime=$(command -v container || command -v docker || true)
if [[ -n "$runtime" ]]; then
  gw=$("$runtime" run --rm alpine sh -c "ip route | awk '/default/{print \$3}'" 2>/dev/null | tr -d '\r' | head -1 || true)
  [[ "$gw" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && EXTRA+=("$gw")
fi
# shellcheck disable=SC2206
[[ -n "${ARGUS_EXTRA_SANS:-}" ]] && EXTRA+=(${ARGUS_EXTRA_SANS})
(( ${#EXTRA[@]} )) && info "Also naming: ${EXTRA[*]}"

go run ./scripts/tools/gencerts "$DIR" "${EXTRA[@]+"${EXTRA[@]}"}"
ok "Wrote $DIR"
printf '\n  %-24s %s\n' "ca.crt / ca.key" "the local authority"
printf '  %-24s %s\n' "server.crt / server.key" "control plane and gateway listeners"
printf '  %-24s %s\n' "client.crt / client.key" "gateways and agents calling the control plane"
printf '\n  These are self-signed and for development only.\n\n'
