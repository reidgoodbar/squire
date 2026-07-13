#!/usr/bin/env sh
set -eu

root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
work="${TMPDIR:-/tmp}/squire-install-product-smoke.$$"
version="v0.7.0-beta.10"

cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "install product smoke: unsupported OS" >&2; exit 0 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "install product smoke: unsupported architecture" >&2; exit 0 ;;
esac

mkdir -p \
  "$work/squire-artifacts" \
  "$work/codex-artifacts" \
  "$work/install" \
  "$work/go-cache" \
  "$work/api/repos/reidgoodbar/squire" \
  "$work/api/repos/reidgoodbar/squire-codex"
(
  cd "$root"
  GOCACHE="$work/go-cache" \
    VERSION="$version" \
    SQUIRE_RELEASE_TARGETS="$os $arch" \
    scripts/build_release_artifacts.sh "$work/squire-artifacts"
)

codex_name="squire-codex_${version}_${os}_${arch}"
codex_stage="$work/codex-artifacts/$codex_name"
mkdir -p "$codex_stage"
cp /bin/sh "$codex_stage/squire-codex"
cp /usr/bin/true "$codex_stage/codex-code-mode-host"
chmod 0755 "$codex_stage/squire-codex" "$codex_stage/codex-code-mode-host"
case "$os" in
  darwin)
    cc -O3 -DNDEBUG -dynamiclib \
      -o "$codex_stage/libsquire_runtime.dylib" \
      "$root/shims/squire_hot_api.c"
    ;;
  linux)
    cc -O3 -DNDEBUG -shared -fPIC \
      -o "$codex_stage/libsquire_runtime.so" \
      "$root/shims/squire_hot_api.c" \
      -ldl -lcrypto
    ;;
esac
printf '1\n' > "$codex_stage/SQUIRE_RUNTIME_ABI"
(
  cd "$work/codex-artifacts"
  tar -czf "$codex_name.tar.gz" "$codex_name"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$codex_name.tar.gz" > SHA256SUMS
  else
    shasum -a 256 "$codex_name.tar.gz" > SHA256SUMS
  fi
)

cat > "$work/api/repos/reidgoodbar/squire/releases" <<'EOF'
[
  {"tag_name":"v0.7.0-beta.9"},
  {"tag_name":"v0.7.0-beta.8"},
  {"tag_name":"v0.7.0-beta.10"},
  {"tag_name":"v0.6.5"}
]
EOF
cat > "$work/api/repos/reidgoodbar/squire-codex/releases" <<'EOF'
[
  {"tag_name":"v0.7.0-beta.10"},
  {"tag_name":"v0.7.0-beta.7"}
]
EOF

GITHUB_API_URL="file://$work/api" \
  SQUIRE_ARTIFACT_DIR="$work/squire-artifacts" \
  SQUIRE_CODEX_ARTIFACT_DIR="$work/codex-artifacts" \
  SQUIRE_INSTALL_DIR="$work/install" \
  "$root/install.sh"

doctor="$($work/install/squire doctor --json)"
printf '%s\n' "$doctor" | grep '"ready": true' >/dev/null
test -x "$work/install/squire"
test -x "$work/install/squire-codex"
test -x "$work/install/codex-code-mode-host"
case "$os" in
  darwin) test -f "$work/install/libsquire_runtime.dylib" ;;
  linux) test -f "$work/install/libsquire_runtime.so" ;;
esac

echo "install_product_smoke: pass ($os/$arch)"
