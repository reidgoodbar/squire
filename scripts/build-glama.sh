#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
bin_dir="${repo_root}/bin"

go_version="${GLAMA_GO_VERSION:-1.26.0}"

arch="$(uname -m)"
case "$arch" in
	x86_64|amd64)
		go_arch="amd64"
		;;
	aarch64|arm64)
		go_arch="arm64"
		;;
	*)
		echo "unsupported architecture for Glama build: $arch" >&2
		exit 2
		;;
esac

go_bin="go"
if ! command -v "$go_bin" >/dev/null 2>&1 || ! "$go_bin" version | grep -q "go1.26"; then
	tmpdir="$(mktemp -d)"
	trap 'rm -rf "$tmpdir"' EXIT
	archive="$tmpdir/go.tgz"
	curl -fsSL "https://go.dev/dl/go${go_version}.linux-${go_arch}.tar.gz" -o "$archive"
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$archive"
	go_bin="/usr/local/go/bin/go"
fi

mkdir -p "$bin_dir"

(
	cd "$repo_root"
	CGO_ENABLED=0 "$go_bin" build -ldflags "-X squire/internal/buildinfo.Version=v$(tr -d '[:space:]' < VERSION)" -o "$bin_dir/squire" ./cmd/squire
)

"$bin_dir/squire" --help --json >/dev/null
