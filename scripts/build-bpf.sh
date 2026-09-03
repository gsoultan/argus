#!/usr/bin/env bash
#
# Compiles the kernel-side execution probe.
#
#   ./scripts/build-bpf.sh                     build the object
#   ./scripts/build-bpf.sh --check             compile only, write nothing
#   ./scripts/build-bpf.sh --runtime docker    choose the container runtime
#
# Runs in a container because the BPF target needs an LLVM that Apple's clang
# does not ship.
#
# The resulting object is committed. Requiring an LLVM with a BPF target to
# `go build` would make the agent unbuildable on most machines that need to
# build it. That makes drift invisible in return, so CI rebuilds and compares:
# the build is byte-reproducible for a given toolchain, and deliberately does
# not depend on anything about the machine running it.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

source scripts/lib.sh

CHECK_ONLY=0
RUNTIME=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check)   CHECK_ONLY=1; shift ;;
    --runtime) RUNTIME="$2"; shift 2 ;;
    -h|--help) sed -n '3,9p' "${BASH_SOURCE[0]}" | sed -e 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument $1" ;;
  esac
done

# Apple's `container` locally, Docker or Podman elsewhere. Named explicitly with
# --runtime when more than one is present and the wrong one gets picked.
if [[ -z "$RUNTIME" ]]; then
  for candidate in container docker podman; do
    if command -v "$candidate" >/dev/null 2>&1; then RUNTIME="$candidate"; break; fi
  done
fi
[[ -n "$RUNTIME" ]] || die "no container runtime found (tried container, docker, podman)"
command -v "$RUNTIME" >/dev/null 2>&1 || die "$RUNTIME is not installed"

IMAGE="docker.io/library/debian:13"
BPF_DIR="internal/execlog/bpf"

banner "Building the eBPF execution probe"
step "Using $RUNTIME"

# vmlinux.h is committed rather than generated.
#
# Generating it from the build host's BTF would make the object depend on
# whichever kernel happened to compile it, so the same source would produce
# different bytes on a developer's machine and in CI — and the committed object
# could never be checked for staleness. The committed header declares only the
# few types the probe reads; CO-RE resolves their real offsets against the
# target kernel at load time, which is what makes one object portable.
[[ -s "$BPF_DIR/vmlinux.h" ]] || die "$BPF_DIR/vmlinux.h is missing"

OUTPUT="-c exec.bpf.c -o exec.bpf.o"
(( CHECK_ONLY )) && OUTPUT="-fsyntax-only exec.bpf.c"

"$RUNTIME" run --rm -v "$PWD:/src" "$IMAGE" bash -euc '
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends clang llvm libbpf-dev >/dev/null

cd /src/'"$BPF_DIR"'

# The define follows the build architecture by convention. It changes nothing
# today because the probe attaches only to tracepoints and touches no
# architecture-specific registers, which is what lets one committed object serve
# both amd64 and arm64. A future kprobe would change that, and the staleness
# check in CI would start failing across architectures — which is the right way
# to find out.
ARCH=$(uname -m | sed "s/x86_64/x86/; s/aarch64/arm64/")

clang -O2 -g -Wall -Werror -target bpf -D__TARGET_ARCH_${ARCH} '"$OUTPUT"'

if [ -f exec.bpf.o ]; then
  # Strips DWARF while keeping .BTF and .BTF.ext, which the loader needs.
  llvm-strip -g exec.bpf.o
  echo "built $(stat -c %s exec.bpf.o) bytes"
fi
'

if (( CHECK_ONLY )); then
  ok "probe compiles"
else
  [[ -s "$BPF_DIR/exec.bpf.o" ]] || die "no object produced"
  ok "probe built ($(shasum -a 256 "$BPF_DIR/exec.bpf.o" 2>/dev/null || sha256sum "$BPF_DIR/exec.bpf.o" | cut -c1-64))"
fi
