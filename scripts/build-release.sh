#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
version="$(tr -d '[:space:]' <"$repo_root/VERSION")"
dist_dir="$repo_root/dist"

mkdir -p "$dist_dir"
rm -f "$dist_dir"/squire_*.tar.gz "$dist_dir"/squire_checksums.txt

build_one() {
	local goos="$1"
	local goarch="$2"
	local asset="squire_${goos}_${goarch}.tar.gz"
	local stage
	stage="$(mktemp -d)"
	trap 'rm -rf "$stage"' RETURN

	(
		cd "$repo_root"
		GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -o "$stage/squire" ./cmd/squire
	)
	tar -C "$stage" -czf "$dist_dir/$asset" squire
	rm -rf "$stage"
	trap - RETURN
}

build_one darwin amd64
build_one darwin arm64
build_one linux amd64
build_one linux arm64

(
	cd "$dist_dir"
	shasum -a 256 squire_*.tar.gz >squire_checksums.txt
)

echo "Built CLI release artifacts for version $version in $dist_dir"
