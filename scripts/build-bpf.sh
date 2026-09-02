#!/usr/bin/env bash
#
# Compiles the kernel-side execution probe.
#
#   ./scripts/build-bpf.sh            build for the host architecture
#   ./scripts/build-bpf.sh --check    compile only, do not write the object
#
# Runs in a container because the BPF target needs an LLVM that Apple's clang
# does not ship, and because vmlinux.h has to come from a kernel with BTF.
#
# The resulting object is committed. Requiring clang and kernel headers to
# `go build` would make the agent unbuildable on most machines that need to
# build it, and the object is reproducible from this script.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

source scripts/lib.sh

CHECK_ONLY=0
[[ "${1:-}" == "--check" ]] && CHECK_ONLY=1

BPF_DIR="internal/execlog/bpf"
IMAGE="docker.io/library/debian:13"

banner "Building the eBPF execution probe"

# vmlinux.h is generated from the build container's own BTF rather than
# committed, so it always matches the headers the program is compiled against.
# CO-RE is what makes the resulting object portable to other kernels; the header
# only has to be self-consistent.
script='
set -eux
apt-get update -qq
apt-get install -y -qq clang llvm libbpf-dev linux-headers-generic bpftool >/dev/null 2>&1 ||
  apt-get install -y -qq clang llvm libbpf-dev >/dev/null

cd /src/internal/execlog/bpf

if [ -r /sys/kernel/btf/vmlinux ] && command -v bpftool >/dev/null; then
  bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
elif [ ! -s vmlinux.h ]; then
  echo "no BTF in the build container and no committed vmlinux.h" >&2
  exit 1
fi

ARCH=$(uname -m | sed "s/x86_64/x86/; s/aarch64/arm64/")
clang -O2 -g -Wall -Werror -target bpf \
  -D__TARGET_ARCH_${ARCH} \
  -I/usr/include/${ARCH}-linux-gnu \
  -c exec.bpf.c -o exec.bpf.o
llvm-strip -g exec.bpf.o 2>/dev/null || true
echo "built $(ls -l exec.bpf.o | awk "{print \$5}") bytes"
'

if (( CHECK_ONLY )); then
  script="${script//-c exec.bpf.c -o exec.bpf.o/-fsyntax-only exec.bpf.c}"
fi

container run --rm -v "$PWD:/src" "$IMAGE" bash -c "$script"
ok "probe built"
