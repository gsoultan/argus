#!/usr/bin/env bash
#
# Builds a Linux kernel that can run the Argus execution probe, for use as the
# Apple `container` default kernel.
#
#   ./scripts/build-test-kernel.sh          build and install
#   ./scripts/build-test-kernel.sh --build  build only, do not install
#
# The kernel `container` ships is deliberately minimal: FTRACE and KPROBES are
# both off, so CONFIG_BPF_EVENTS is unavailable and BPF cannot attach to a
# tracepoint at all. It also carries no BTF, without which CO-RE relocations
# cannot be resolved. Neither is a bug — they are the right defaults for running
# application containers — but they make the eBPF tier untestable.
#
# Rather than guess at a working configuration, this starts from the config of
# the kernel `container` is already running (read out of /proc/config.gz) and
# turns on only what the probe needs. Every virtio driver the VM depends on is
# already built in there, which is the part that is easy to get wrong and
# expensive to debug: a kernel that cannot see its root filesystem fails with no
# useful output.
#
# This is a development and verification tool. It is not part of the product,
# and no Argus deployment needs it.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

source scripts/lib.sh

INSTALL=1
[[ "${1:-}" == "--build" ]] && INSTALL=0

WORK="${ARGUS_KERNEL_WORK:-/tmp/argus-kernel}"
KVER="${ARGUS_KERNEL_VERSION:-6.18.15}"
OUT="$WORK/Image"
JOBS="$(sysctl -n hw.ncpu 2>/dev/null || echo 4)"

mkdir -p "$WORK"

banner "Building Linux $KVER with BTF and BPF tracepoint support"

# The running kernel's own config, which is known to boot here.
if [[ ! -s "$WORK/base.config" ]]; then
  step "Reading the current kernel's configuration"
  container run --rm -v "$WORK:/out" docker.io/library/debian:13 \
    sh -c 'zcat /proc/config.gz > /out/base.config' >/dev/null
  ok "$(grep -c . "$WORK/base.config") settings"
fi

step "Building (this takes a while; $JOBS jobs)"

# The build script is written out and mounted rather than passed inline.
# Quoting a multi-line script into `bash -c` puts every apostrophe in every
# comment one keystroke away from ending the string early, and the failure mode
# is silent: the remainder runs on the host instead.
cat > "$WORK/build-inner.sh" <<'INNER'
#!/usr/bin/env bash
set -euo pipefail

KVER="$1"
JOBS="$2"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# dwarves supplies pahole, which is what actually emits the BTF blob. Without
# it the build silently produces a kernel with no BTF, which is the exact
# problem this script exists to fix.
apt-get install -y -qq --no-install-recommends \
  build-essential flex bison bc libssl-dev libelf-dev dwarves \
  zlib1g-dev pkg-config libcap-dev python3 \
  cpio kmod xz-utils curl ca-certificates rsync >/dev/null

MAJOR="${KVER%%.*}"

# The tarball is cached on the shared mount, but the source tree is extracted
# and built on the guest's own filesystem. The kernel tree contains symlinks
# that virtiofs refuses to create from inside the guest, and building across the
# share would be far slower besides.
cd /work
if [ ! -f "linux-${KVER}.tar.xz" ] || ! xz -t "linux-${KVER}.tar.xz" 2>/dev/null; then
  rm -f "linux-${KVER}.tar.xz"
  echo "downloading linux-${KVER}"
  curl -fSL --retry 3 -o "linux-${KVER}.tar.xz" \
    "https://cdn.kernel.org/pub/linux/kernel/v${MAJOR}.x/linux-${KVER}.tar.xz"
fi

mkdir -p /build
echo "extracting to container-local storage"
tar xf "/work/linux-${KVER}.tar.xz" -C /build
cd "/build/linux-${KVER}"
[ -x scripts/config ] || { echo "kernel source tree is incomplete" >&2; exit 1; }

cp /work/base.config .config

# What the probe needs, and nothing else.
#
#   FTRACE/KPROBES  bring in the tracepoint and event infrastructure;
#                   BPF_EVENTS depends on KPROBE_EVENTS || UPROBE_EVENTS
#   BPF_EVENTS      is what lets a BPF program attach to a tracepoint
#   DEBUG_INFO_BTF  emits the BTF blob that CO-RE relocates against
#   BPF_JIT         not required, but an interpreted probe on every execve is
#                   a cost worth avoiding even in a test kernel
scripts/config --file .config \
  --enable FTRACE \
  --enable KPROBES \
  --enable KPROBE_EVENTS \
  --enable UPROBE_EVENTS \
  --enable FTRACE_SYSCALLS \
  --enable BPF_EVENTS \
  --enable BPF_JIT \
  --enable BPF_JIT_ALWAYS_ON \
  --enable DEBUG_INFO \
  --enable DEBUG_INFO_DWARF5 \
  --enable DEBUG_INFO_BTF \
  --disable DEBUG_INFO_NONE \
  --disable DEBUG_INFO_REDUCED \
  --disable DEBUG_INFO_SPLIT

# Resolves everything the above selects or requires. Answering prompts by hand
# would be how a dependency gets missed.
make olddefconfig >/dev/null

for opt in DEBUG_INFO_BTF BPF_EVENTS BPF_SYSCALL VIRTIO_BLK VIRTIO_NET VIRTIO_FS; do
  grep -q "^CONFIG_${opt}=y" .config || { echo "CONFIG_${opt} did not survive olddefconfig" >&2; exit 1; }
done
echo "configuration verified"

make -j"${JOBS}" Image 2>&1 | tail -60
cp arch/arm64/boot/Image /work/Image

# A kernel without a .BTF section would boot and then fail to load the probe,
# which is a slow way to discover a build problem.
if ! readelf -S vmlinux 2>/dev/null | grep -q "\.BTF"; then
  echo "the built kernel has no .BTF section" >&2
  exit 1
fi
echo "BTF present"
ls -l /work/Image
INNER

container run --rm --cpus "$JOBS" --memory 8g \
  -v "$WORK:/work" docker.io/library/debian:13 \
  bash /work/build-inner.sh "$KVER" "$JOBS"

[[ -s "$OUT" ]] || die "no kernel produced"
ok "built $(du -h "$OUT" | cut -f1)"

if (( ! INSTALL )); then
  echo "  Kernel at $OUT — install with:"
  echo "    container system kernel set --binary $OUT --arch arm64 --force"
  exit 0
fi

step "Installing as the default container kernel"
container system kernel set --binary "$OUT" --arch arm64 --force
ok "installed; existing containers must be recreated to pick it up"
