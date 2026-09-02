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

go run ./scripts/tools/gencerts "$DIR"
ok "Wrote $DIR"
printf '\n  %-24s %s\n' "ca.crt / ca.key" "the local authority"
printf '  %-24s %s\n' "server.crt / server.key" "control plane and gateway listeners"
printf '  %-24s %s\n' "client.crt / client.key" "gateways and agents calling the control plane"
printf '\n  These are self-signed and for development only.\n\n'
