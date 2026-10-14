#!/usr/bin/env bash
# Runs inside the container started by build-images.sh.
set -euo pipefail

br_version=2026.02.3
br_sha256=5a59e7501b0b4ec52c41f4bfa79412320e0b37eae5f719605a258e8d0c6fc7fb

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
  build-essential bc bison flex cpio rsync unzip wget file python3 perl \
  ca-certificates libncurses-dev git >/dev/null

cd /work
br=buildroot-$br_version
if [[ ! -d $br ]]; then
  wget -q "https://buildroot.org/downloads/$br.tar.xz"
  echo "$br_sha256  $br.tar.xz" | sha256sum -c -
  tar xf "$br.tar.xz"
  rm "$br.tar.xz"
fi

export FORCE_UNSAFE_CONFIGURE=1 # we build as root in a throwaway container
export BR2_DL_DIR=/work/dl
make -s -C "$br" O=/work/out BR2_EXTERNAL=/src rvsim_defconfig

# Kconfig drops unknown or unmet symbols silently; every BR2_ line of the
# defconfig must survive into the resolved configuration.
while IFS= read -r line; do
  if ! grep -qxF "$line" /work/out/.config; then
    echo "container-build: defconfig line not in the resolved config: $line" >&2
    exit 1
  fi
done < <(grep -E '^(# )?BR2_' /src/configs/rvsim_defconfig)

# Buildroot cannot change the target architecture or toolchain of an
# existing build; start over when they change.
arch=$(grep -E '^(# )?BR2_(RISCV|GCC_TARGET|TOOLCHAIN|KERNEL_HEADERS|PACKAGE_HOST_LINUX_HEADERS)' /work/out/.config | sha256sum)
if [[ -d /work/out/host && $(cat /work/out/.arch-hash 2>/dev/null) != "$arch" ]]; then
  echo "container-build: architecture or toolchain changed; rebuilding from scratch"
  make -s -C /work/out clean
fi
echo "$arch" > /work/out/.arch-hash

# Buildroot does not notice edits to the kernel config or the firmware
# options; rebuild those packages (not the toolchain) when /src changes.
hash=$(cd /src && find . -type f | LC_ALL=C sort | xargs sha256sum | sha256sum | cut -d' ' -f1)
if [[ -f /work/out/.src-hash && $(cat /work/out/.src-hash) != "$hash" ]]; then
  make -s -C /work/out linux-dirclean opensbi-dirclean
fi
# One compile job per ~1.5 GiB: GCC's generated sources need ~1 GiB each,
# and small container VMs (e.g. Podman on macOS) are OOM-killed otherwise.
mem_kib=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
jobs=${JOBS:-$(( mem_kib / 1572864 ))}
jobs=$(( jobs < 1 ? 1 : jobs > $(nproc) ? $(nproc) : jobs ))
echo "container-build: $jobs jobs"
# Fetch every source first, retrying transient network failures.
for attempt in 1 2 3; do
  make -s -C /work/out source && break
  [[ $attempt == 3 ]] && exit 1
  echo "container-build: download failed; retrying" >&2
  sleep 15
done
make -C /work/out BR2_JLEVEL="$jobs" toolchain

# The toolchain's build trees (~6 GB) are not needed once it is installed in
# out/host. Empty them, keeping Buildroot's stamp files so nothing rebuilds;
# small container VMs run out of disk otherwise.
for d in /work/out/build/{host-gcc-*,gcc-final-*,host-binutils-*,linux-headers-*}; do
  [[ -d $d ]] && find "$d" -mindepth 1 -maxdepth 1 ! -name '.stamp_*' -exec rm -rf {} +
done

make -C /work/out BR2_JLEVEL="$jobs"
echo "$hash" > /work/out/.src-hash

# Every option linux.config sets must survive into the kernel's resolved
# configuration (Kconfig drops unmet ones silently); among them, no FPU.
kconfig=/work/out/build/linux-6.12.112/.config
while IFS= read -r line; do
  if ! grep -qxF "$line" "$kconfig"; then
    echo "container-build: linux.config line not in the resolved config: $line" >&2
    exit 1
  fi
done < <(grep -E '^(# )?CONFIG_' /src/linux.config)

cp /work/out/images/fw_jump.bin /work/out/images/Image /work/out/images/rootfs.cpio \
  /work/out/images/rootfs.ext4 /out/
cp "$kconfig" /out/linux.config.resolved
