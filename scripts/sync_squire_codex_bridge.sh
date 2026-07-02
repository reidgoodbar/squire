#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
squire_root="$(cd "$script_dir/.." && pwd)"
codex_root="${1:-$(cd "$squire_root/.." && pwd)/squire-codex}"

dest="$codex_root/vendor/squire"
mkdir -p \
  "$dest/crates/squire-codex-adapter/src" \
  "$dest/crates/squire-codex-bridge/src"

cp "$squire_root/crates/squire-codex-adapter/README.md" \
  "$dest/crates/squire-codex-adapter/README.md"
cp "$squire_root/crates/squire-codex-adapter/src/exec_bridge.rs" \
  "$dest/crates/squire-codex-adapter/src/exec_bridge.rs"
cp "$squire_root/crates/squire-codex-adapter/src/unified_exec_bridge.rs" \
  "$dest/crates/squire-codex-adapter/src/unified_exec_bridge.rs"
cp "$squire_root/crates/squire-codex-bridge/Cargo.toml" \
  "$dest/crates/squire-codex-bridge/Cargo.toml"
cp "$squire_root/crates/squire-codex-bridge/src/lib.rs" \
  "$dest/crates/squire-codex-bridge/src/lib.rs"

echo "Synced Squire Codex bridge into $dest"
