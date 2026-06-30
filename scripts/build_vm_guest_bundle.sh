#!/usr/bin/env sh
set -eu

out_dir="${1:-.tmp/vm-guest}"
mirror="${SQUIRE_ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
version="${SQUIRE_ALPINE_VERSION:-latest-stable}"
codex_version="${SQUIRE_CODEX_VERSION:-}"
host_arch="$(uname -m)"

case "${SQUIRE_VM_ARCH:-$host_arch}" in
  arm64|aarch64) alpine_arch="aarch64"; goarch="arm64" ;;
  x86_64|amd64) alpine_arch="x86_64"; goarch="amd64" ;;
  *) echo "unsupported VM guest arch: ${SQUIRE_VM_ARCH:-$host_arch}" >&2; exit 2 ;;
esac

case "$out_dir" in
  ""|"/"|".") echo "refusing unsafe output directory: $out_dir" >&2; exit 2 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to fetch Alpine netboot assets" >&2
  exit 2
fi
if ! command -v cpio >/dev/null 2>&1 || ! command -v gzip >/dev/null 2>&1; then
  echo "cpio and gzip are required to assemble the initramfs" >&2
  exit 2
fi

mkdir -p "$out_dir"
work="$out_dir/work"
rm -rf "$work"
mkdir -p "$work/initrd" "$work/cache/go-build" "$work/cache/gomod"

kernel_url="$mirror/$version/releases/$alpine_arch/netboot/vmlinuz-virt"
initramfs_url="$mirror/$version/releases/$alpine_arch/netboot/initramfs-virt"

echo "fetching $kernel_url"
curl -fsSL "$kernel_url" -o "$out_dir/kernel"
echo "fetching $initramfs_url"
curl -fsSL "$initramfs_url" -o "$work/initramfs-virt"

gzip_offset="$(LC_ALL=C sh -c 'grep -aob "$(printf "\\037\\213\\010")" "$1" | head -n 1 | cut -d: -f1' sh "$out_dir/kernel" || true)"
if [ -n "$gzip_offset" ]; then
  echo "extracting raw Linux kernel image from EFI-stub wrapper"
  if ! tail -c +"$((gzip_offset + 1))" "$out_dir/kernel" | gzip -dc > "$out_dir/kernel.Image" 2>/dev/null; then
    if [ ! -s "$out_dir/kernel.Image" ]; then
      echo "failed to extract raw Linux kernel image" >&2
      exit 1
    fi
  fi
else
  cp "$out_dir/kernel" "$out_dir/kernel.Image"
fi

echo "building linux/$goarch guest binaries"
GOCACHE="$work/cache/go-build" GOMODCACHE="$work/cache/gomod" \
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
  go build -trimpath -buildvcs=false -o "$work/squire-vm-agent" ./cmd/squire-vm-agent
GOCACHE="$work/cache/go-build" GOMODCACHE="$work/cache/gomod" \
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
  go build -trimpath -buildvcs=false -o "$work/squire" ./cmd/squire

if [ -n "$codex_version" ] || [ "${SQUIRE_VM_INCLUDE_CODEX:-0}" = "1" ]; then
  if [ -z "$codex_version" ]; then
    if command -v codex >/dev/null 2>&1; then
      codex_version="$(codex --version | awk '{print $2}')"
    else
      echo "SQUIRE_VM_INCLUDE_CODEX=1 requires SQUIRE_CODEX_VERSION when codex is not on PATH" >&2
      exit 2
    fi
  fi
  case "$goarch" in
    arm64) codex_target="aarch64-unknown-linux-musl" ;;
    amd64) codex_target="x86_64-unknown-linux-musl" ;;
    *) echo "unsupported Codex guest arch: $goarch" >&2; exit 2 ;;
  esac
  codex_asset="codex-${codex_target}.tar.gz"
  codex_url="https://github.com/openai/codex/releases/download/rust-v${codex_version}/${codex_asset}"
  echo "fetching $codex_url"
  curl -fsSL "$codex_url" -o "$work/$codex_asset"
  mkdir -p "$work/codex"
  tar -xzf "$work/$codex_asset" -C "$work/codex"
  codex_bin="$(find "$work/codex" -type f -name "codex*" -perm +111 | head -n 1 || true)"
  if [ -z "$codex_bin" ]; then
    codex_bin="$(find "$work/codex" -type f -name "codex*" | head -n 1 || true)"
  fi
  if [ -z "$codex_bin" ]; then
    echo "downloaded Codex asset did not contain a codex binary" >&2
    exit 1
  fi
  cp "$codex_bin" "$work/codex-linux"
  chmod 0755 "$work/codex-linux"
fi

(
  cd "$work/initrd"
  gzip -dc ../initramfs-virt | cpio -idmu >/dev/null 2>&1
)

mkdir -p "$work/initrd/mnt/squire-workspace" "$work/initrd/mnt/squire-store" "$work/initrd/usr/local/bin" "$work/initrd/usr/local/share/squire/shims" "$work/initrd/proc" "$work/initrd/sys" "$work/initrd/dev"
cp "$work/squire-vm-agent" "$work/initrd/squire-vm-agent"
cp "$work/squire" "$work/initrd/usr/local/bin/squire"
cp shims/squire_mmap_shim.c "$work/initrd/usr/local/share/squire/shims/"
cp shims/squire_preload.c "$work/initrd/usr/local/share/squire/shims/"
cp shims/squire_preload_helper.c "$work/initrd/usr/local/share/squire/shims/"
chmod 0755 "$work/initrd/squire-vm-agent" "$work/initrd/usr/local/bin/squire"
if [ -f "$work/codex-linux" ]; then
  mkdir -p "$work/initrd/etc"
  cp "$work/codex-linux" "$work/initrd/usr/local/bin/codex"
  printf '%s\n' bubblewrap build-base git openssl-dev ripgrep > "$work/initrd/etc/squire-vm-apk-packages"
  chmod 0755 "$work/initrd/usr/local/bin/codex"
fi

if [ -f "$work/initrd/init" ]; then
  mv "$work/initrd/init" "$work/initrd/init.alpine"
fi

cat > "$work/initrd/init" <<'EOF'
#!/usr/bin/sh
set -eu

export PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin
BB=/usr/bin/busybox

if [ "${SQUIRE_VM_STAGE2:-0}" != "1" ]; then
  $BB mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
  $BB mkdir -p /newroot
  $BB mount -t tmpfs -o mode=755,size=1536m tmpfs /newroot
  for path in bin etc lib sbin usr var squire-vm-agent init.alpine; do
    if [ -e "/$path" ]; then
      $BB cp -a "/$path" "/newroot/$path"
    fi
  done
  $BB cp -a /init /newroot/init
  $BB mkdir -p /newroot/dev /newroot/proc /newroot/sys /newroot/tmp /newroot/mnt/squire-workspace /newroot/mnt/squire-store /newroot/mnt/squire-codex-home
  $BB chmod 755 /newroot
  $BB chmod 1777 /newroot/tmp
  $BB mount -t devtmpfs devtmpfs /newroot/dev 2>/dev/null || true
  export SQUIRE_VM_STAGE2=1
  exec $BB switch_root /newroot /init
fi

$BB mount -t proc proc /proc 2>/dev/null || true
$BB mount -t sysfs sysfs /sys 2>/dev/null || true
$BB mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
$BB chmod 755 / /etc /mnt /usr /usr/bin /usr/local /usr/local/bin /var 2>/dev/null || true
$BB modprobe virtio_net 2>/dev/null || true
$BB mkdir -p /tmp
$BB chmod 1777 /tmp

$BB cat > /tmp/squire-udhcpc.script <<'EOS'
#!/usr/bin/sh
BB=/usr/bin/busybox

case "$1" in
  deconfig)
    $BB ifconfig "$interface" 0.0.0.0 2>/dev/null || true
    ;;
  bound|renew)
    if [ -n "${broadcast:-}" ]; then
      $BB ifconfig "$interface" "$ip" netmask "$subnet" broadcast "$broadcast" up
    else
      $BB ifconfig "$interface" "$ip" netmask "$subnet" up
    fi
    $BB route del default dev "$interface" 2>/dev/null || true
    for gateway in ${router:-}; do
      $BB route add default gw "$gateway" dev "$interface" 2>/dev/null && break
    done
    if [ -n "${dns:-}" ]; then
      : > /etc/resolv.conf
      for nameserver in $dns; do
        echo "nameserver $nameserver" >> /etc/resolv.conf
      done
    fi
    ;;
esac

exit 0
EOS
$BB chmod 0755 /tmp/squire-udhcpc.script

for ifc in eth0 enp0s1 ens1; do
  if [ -e "/sys/class/net/$ifc" ]; then
    $BB ifconfig "$ifc" up 2>/dev/null || true
    $BB udhcpc -i "$ifc" -q -t 5 -s /tmp/squire-udhcpc.script 2>/dev/null || true
    break
  fi
done

if [ -f /etc/squire-vm-apk-packages ] && [ -x /usr/sbin/apk ]; then
  $BB mkdir -p /lib/apk/db /etc/apk/cache /var/cache/apk
  if [ ! -s /etc/apk/repositories ]; then
    echo "https://dl-cdn.alpinelinux.org/alpine/latest-stable/main" > /etc/apk/repositories
    echo "https://dl-cdn.alpinelinux.org/alpine/latest-stable/community" >> /etc/apk/repositories
  fi
  /usr/sbin/apk add --initdb --no-cache $($BB cat /etc/squire-vm-apk-packages) >/tmp/squire-apk.log 2>&1 || {
    $BB cat /tmp/squire-apk.log >&2 || true
  }
fi

if [ -x /usr/bin/cc ] && [ -f /usr/local/share/squire/shims/squire_preload.c ]; then
  (
    cd /usr/local/share/squire/shims
    cc -O3 -DNDEBUG -shared -fPIC -o /usr/local/bin/squire-preload.so squire_preload.c -ldl -lcrypto
    cc -O3 -DNDEBUG -o /usr/local/bin/squire-preload-helper squire_preload_helper.c -lcrypto
    $BB chmod 0755 /usr/local/bin/squire-preload.so /usr/local/bin/squire-preload-helper
  ) >/tmp/squire-preload-build.log 2>&1 || {
    $BB cat /tmp/squire-preload-build.log >&2 || true
  }
fi

$BB mkdir -p /mnt/squire-workspace /mnt/squire-store /mnt/squire-codex-home
$BB mount -t virtiofs squire-workspace /mnt/squire-workspace
$BB mount -t virtiofs squire-store /mnt/squire-store
$BB mount -t virtiofs squire-codex-home /mnt/squire-codex-home 2>/dev/null || true

cd /mnt/squire-workspace
export SQUIRE_VM_AGENT_TRANSPORT=serial
export SQUIRE_VM_AGENT_SERIAL=/dev/hvc0
export SQUIRE_VM_AGENT_INTERACTIVE_SERIAL=/dev/hvc1
if [ -f /mnt/squire-codex-home/auth.json ] || [ -f /mnt/squire-codex-home/config.toml ]; then
  export CODEX_HOME=/mnt/squire-codex-home
fi
exec /squire-vm-agent
EOF
chmod 0755 "$work/initrd/init"

(
  cd "$work/initrd"
  find . | cpio -o -H newc 2>/dev/null | gzip -9 > "$work/initrd.img"
)
mv "$work/initrd.img" "$out_dir/initrd"

cat > "$out_dir/README.txt" <<EOF
Squire VM guest bundle

kernel: $out_dir/kernel
raw kernel: $out_dir/kernel.Image
initrd: $out_dir/initrd
arch: $alpine_arch

Use from macOS:
  export SQUIRE_VM_KERNEL=$out_dir/kernel.Image
  export SQUIRE_VM_INITRD=$out_dir/initrd
  export SQUIRE_VM_CODEX_HOME=$HOME/.codex
  squire vm status --short
  squire vm session -- /bin/sh -lc 'echo hello from guest'
  squire vm session -- codex --version
EOF

echo "vm_guest_bundle: $out_dir"
echo "kernel: $out_dir/kernel.Image"
echo "initrd: $out_dir/initrd"
