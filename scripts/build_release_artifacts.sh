#!/usr/bin/env sh
set -eu

out_dir="${1:-.tmp/release}"
version="${VERSION:-}"
commit="${COMMIT:-}"
date_utc="${DATE_UTC:-}"

case "$out_dir" in
  ""|"/"|".")
    echo "refusing unsafe output directory: $out_dir" >&2
    exit 2
    ;;
esac

if [ -z "$version" ]; then
  if git describe --tags --exact-match >/dev/null 2>&1; then
    version=$(git describe --tags --exact-match)
  else
    version=$(git describe --tags --always --dirty)
  fi
fi
if [ -z "$commit" ]; then
  commit=$(git rev-parse --short HEAD)
fi
if [ -z "$date_utc" ]; then
  date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi

rm -rf "$out_dir"
mkdir -p "$out_dir"
cp install.sh "$out_dir/install.sh"

ldflags="-s -w -X main.buildVersion=$version -X main.buildCommit=$commit -X main.buildDate=$date_utc"
targets="${SQUIRE_RELEASE_TARGETS:-"
linux amd64
linux arm64
darwin amd64
darwin arm64
windows amd64
"}"

checksum_files="install.sh"
set -- $targets
while [ "$#" -gt 0 ]; do
  goos="$1"
  goarch="$2"
  shift 2

  name="squire_${version}_${goos}_${goarch}"
  stage="$out_dir/$name"
  mkdir -p "$stage"
  binary="squire"
  if [ "$goos" = "windows" ]; then
    binary="squire.exe"
  fi

  echo "building $name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "$ldflags" \
    -o "$stage/$binary" \
    ./cmd/squire

  cp README.md RELEASE_CHECKLIST.md SQUIRE_CONTRACT.md "$stage/"
  mkdir -p "$stage/shims"
  cp shims/squire_mmap_shim.c "$stage/shims/"
  cp shims/squire_preload.c "$stage/shims/"
  cp shims/squire_preload_helper.c "$stage/shims/"
  mkdir -p "$stage/vm"
  cp vm/squire_vm_darwin.swift "$stage/vm/"
  cp vm/squire_vm_darwin.entitlements "$stage/vm/"
  if [ -f LICENSE ]; then
    cp LICENSE "$stage/"
  fi

  archive="$out_dir/$name.tar.gz"
  (cd "$out_dir" && tar -czf "$name.tar.gz" "$name")
  rm -rf "$stage"
  checksum_files="$checksum_files $(basename "$archive")"
done

(
  cd "$out_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    # shellcheck disable=SC2086
    sha256sum $checksum_files > SHA256SUMS
  else
    # shellcheck disable=SC2086
    shasum -a 256 $checksum_files > SHA256SUMS
  fi
)

cat > "$out_dir/RELEASE_MANIFEST.txt" <<EOF
Squire release artifacts
version: $version
commit: $commit
date: $date_utc

Artifacts:
$checksum_files

Verify:
  shasum -a 256 -c SHA256SUMS
  # or, on Linux:
  sha256sum -c SHA256SUMS
EOF

echo "release_artifacts: $out_dir"
