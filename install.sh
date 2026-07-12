#!/usr/bin/env sh
set -eu

repo="${SQUIRE_REPO:-reidgoodbar/squire}"
codex_repo="${SQUIRE_CODEX_REPO:-reidgoodbar/squire-codex}"
version="${SQUIRE_VERSION:-}"
codex_version="${SQUIRE_CODEX_VERSION:-}"
install_dir="${SQUIRE_INSTALL_DIR:-${HOME:-}/.local/bin}"
github_base="${GITHUB_BASE_URL:-https://github.com}"
github_api="${GITHUB_API_URL:-https://api.github.com}"
artifact_dir="${SQUIRE_ARTIFACT_DIR:-}"
codex_artifact_dir="${SQUIRE_CODEX_ARTIFACT_DIR:-}"

fail() {
  echo "squire install: $*" >&2
  exit 1
}

have() {
  command -v "$1" >/dev/null 2>&1
}

compile_preload() {
  stage="$1"
  preload_src="$stage/shims/squire_preload.c"
  if [ ! -f "$preload_src" ]; then
    echo "squire install: preload source not present in archive"
    return 0
  fi
  if ! have cc; then
    echo "squire install: cc not found; skipped optional preload library"
    return 0
  fi
  case "$os" in
    darwin)
      preload_out="$install_dir/squire-preload.dylib"
      tmp_preload="$install_dir/.squire-preload.dylib.tmp.$$"
      if cc -O3 -DNDEBUG -dynamiclib -o "$tmp_preload" "$preload_src"; then
        chmod 0755 "$tmp_preload"
        mv "$tmp_preload" "$preload_out"
        echo "squire install: installed $preload_out"
      else
        rm -f "$tmp_preload"
        echo "squire install: optional preload library build failed; squire binary is still installed"
      fi
      ;;
    linux)
      preload_out="$install_dir/squire-preload.so"
      tmp_preload="$install_dir/.squire-preload.so.tmp.$$"
      if cc -O3 -DNDEBUG -shared -fPIC -o "$tmp_preload" "$preload_src" -ldl -lcrypto; then
        chmod 0755 "$tmp_preload"
        mv "$tmp_preload" "$preload_out"
        echo "squire install: installed $preload_out"
      else
        rm -f "$tmp_preload"
        echo "squire install: optional preload library build failed; install libssl headers to enable it"
      fi
      ;;
  esac
}

compile_preload_helper() {
  stage="$1"
  helper_src="$stage/shims/squire_preload_helper.c"
  helper_out="$install_dir/squire-preload-helper"
  if [ ! -f "$helper_src" ]; then
    echo "squire install: preload helper source not present in archive"
    return 0
  fi
  if ! have cc; then
    echo "squire install: cc not found; skipped optional preload helper"
    return 0
  fi
  tmp_helper="$install_dir/.squire-preload-helper.tmp.$$"
  case "$os" in
    darwin)
      if cc -O3 -DNDEBUG -o "$tmp_helper" "$helper_src"; then
        chmod 0755 "$tmp_helper"
        mv "$tmp_helper" "$helper_out"
        echo "squire install: installed $helper_out"
      else
        rm -f "$tmp_helper"
        echo "squire install: optional preload helper build failed; file-action spawns will run native"
      fi
      ;;
    linux)
      if cc -O3 -DNDEBUG -o "$tmp_helper" "$helper_src" -lcrypto; then
        chmod 0755 "$tmp_helper"
        mv "$tmp_helper" "$helper_out"
        echo "squire install: installed $helper_out"
      else
        rm -f "$tmp_helper"
        echo "squire install: optional preload helper build failed; install libssl headers to enable file-action replay"
      fi
      ;;
  esac
}

compile_hot_api() {
  stage="$1"
  hot_src="$stage/shims/squire_hot_api.c"
  if [ ! -f "$hot_src" ]; then
    echo "squire install: runtime source not present in archive"
    return 1
  fi
  if ! have cc; then
    echo "squire install: cc not found; cannot build the Squire runtime"
    return 1
  fi
  case "$os" in
    darwin)
      hot_out="$install_dir/libsquire_runtime.dylib"
      compat_hot_out="$install_dir/libsquire_hot.dylib"
      tmp_hot="$install_dir/.libsquire_runtime.dylib.tmp.$$"
      if cc -O3 -DNDEBUG -dynamiclib -o "$tmp_hot" "$hot_src"; then
        chmod 0755 "$tmp_hot"
        mv "$tmp_hot" "$hot_out"
        cp "$hot_out" "$compat_hot_out"
        printf '1\n' > "$install_dir/.squire-runtime-abi"
        echo "squire install: installed $hot_out"
      else
        rm -f "$tmp_hot"
        echo "squire install: Squire runtime build failed; native fallback remains available"
        return 1
      fi
      ;;
    linux)
      hot_out="$install_dir/libsquire_runtime.so"
      compat_hot_out="$install_dir/libsquire_hot.so"
      tmp_hot="$install_dir/.libsquire_runtime.so.tmp.$$"
      if cc -O3 -DNDEBUG -shared -fPIC -o "$tmp_hot" "$hot_src" -ldl -lcrypto; then
        chmod 0755 "$tmp_hot"
        mv "$tmp_hot" "$hot_out"
        cp "$hot_out" "$compat_hot_out"
        printf '1\n' > "$install_dir/.squire-runtime-abi"
        echo "squire install: installed $hot_out"
      else
        rm -f "$tmp_hot"
        echo "squire install: Squire runtime build failed; install libssl headers to enable it"
        return 1
      fi
      ;;
    *)
      return 1
      ;;
  esac
}

install_prebuilt_runtime() {
  squire_stage="$1"
  squire_codex_stage="$2"
  case "$os" in
    darwin)
      runtime_name="libsquire_runtime.dylib"
      compatibility_name="libsquire_hot.dylib"
      ;;
    linux)
      runtime_name="libsquire_runtime.so"
      compatibility_name="libsquire_hot.so"
      ;;
    *)
      return 1
      ;;
  esac
  for runtime_stage in "$squire_codex_stage" "$squire_stage"; do
    runtime_source="$runtime_stage/$runtime_name"
    runtime_abi_source="$runtime_stage/SQUIRE_RUNTIME_ABI"
    if [ -f "$runtime_source" ] && [ -f "$runtime_abi_source" ] && [ "$(tr -d '\r\n' < "$runtime_abi_source")" = "1" ]; then
      runtime_tmp="$install_dir/.$runtime_name.tmp.$$"
      cp "$runtime_source" "$runtime_tmp"
      chmod 0755 "$runtime_tmp"
      mv "$runtime_tmp" "$install_dir/$runtime_name"
      cp "$install_dir/$runtime_name" "$install_dir/$compatibility_name"
      cp "$runtime_abi_source" "$install_dir/.squire-runtime-abi"
      echo "squire install: installed $install_dir/$runtime_name"
      return 0
    fi
  done
  return 1
}

compile_vm_darwin() {
  stage="$1"
  if [ "$os" != "darwin" ]; then
    return 0
  fi
  helper_src="$stage/vm/squire_vm_darwin.swift"
  helper_out="$install_dir/squire-vm-darwin"
  if [ ! -f "$helper_src" ]; then
    echo "squire install: macOS VM helper source not present in archive"
    return 0
  fi
  if ! have swiftc; then
    echo "squire install: swiftc not found; skipped optional macOS Virtualization.framework helper"
    return 0
  fi
  tmp_helper="$install_dir/.squire-vm-darwin.tmp.$$"
  module_cache="${TMPDIR:-/tmp}/squire-swift-module-cache.$$"
  mkdir -p "$module_cache"
  if swiftc -parse-as-library -module-cache-path "$module_cache" -O -o "$tmp_helper" "$helper_src"; then
    entitlement_src="$stage/vm/squire_vm_darwin.entitlements"
    if have codesign && [ -f "$entitlement_src" ]; then
      if codesign --force --sign - --entitlements "$entitlement_src" "$tmp_helper" >/dev/null 2>&1; then
        echo "squire install: signed macOS VM helper with virtualization entitlement"
      else
        echo "squire install: macOS VM helper signing failed; Virtualization.framework may report unsupported"
      fi
    else
      echo "squire install: codesign or VM entitlement file missing; Virtualization.framework may report unsupported"
    fi
    chmod 0755 "$tmp_helper"
    mv "$tmp_helper" "$helper_out"
    echo "squire install: installed $helper_out"
  else
    rm -f "$tmp_helper"
    echo "squire install: optional macOS VM helper build failed; configure SQUIRE_VM_RUNNER for external Linux guests"
  fi
  rm -rf "$module_cache"
}

print_macos_shell_note() {
  if [ "$os" != "darwin" ]; then
    return 0
  fi
  echo "squire install: macOS uses the host-native Squire runtime; no VM or shell injection is required"
}

print_codex_note() {
  echo "squire install: installed the Squire Codex driver as squire-codex"
  echo "squire install: authenticate and configure Codex normally; squire-codex uses the same Codex home"
}

fetch_stdout() {
  url="$1"
  if have curl; then
    curl -fsSL "$url"
  elif have wget; then
    wget -qO- "$url"
  else
    fail "curl or wget is required"
  fi
}

fetch_file() {
  url="$1"
  dest="$2"
  if have curl; then
    curl -fL "$url" -o "$dest"
  elif have wget; then
    wget -qO "$dest" "$url"
  else
    fail "curl or wget is required"
  fi
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *) fail "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

latest_version_for() {
  release_repo="$1"
  fetch_stdout "$github_api/repos/$release_repo/releases?per_page=1" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

verify_checksum() {
  checksum_work_dir="$1"
  checksum_asset="$2"
  checksum_sums_name="${3:-SHA256SUMS}"
  checksum_sums="$checksum_work_dir/$checksum_sums_name"
  checksum_one="$checksum_work_dir/$checksum_sums_name.one"
  checksum_one_base="$checksum_sums_name.one"

  grep " $checksum_asset\$" "$checksum_sums" > "$checksum_one" || fail "SHA256SUMS does not contain $checksum_asset"
  (
    cd "$checksum_work_dir"
    if have sha256sum; then
      sha256sum -c "$checksum_one_base" >/dev/null
    elif have shasum; then
      shasum -a 256 -c "$checksum_one_base" >/dev/null
    else
      fail "sha256sum or shasum is required to verify downloads"
    fi
  )
}

fetch_codex_archive() {
  if [ -n "$codex_artifact_dir" ]; then
    cp "$codex_artifact_dir/$codex_asset" "$tmp/$codex_asset" &&
      cp "$codex_artifact_dir/SHA256SUMS" "$tmp/SQUIRE_CODEX_SHA256SUMS"
  else
    codex_fetch_base="$github_base/$codex_repo/releases/download/$codex_version"
    fetch_file "$codex_fetch_base/$codex_asset" "$tmp/$codex_asset" &&
      fetch_file "$codex_fetch_base/SHA256SUMS" "$tmp/SQUIRE_CODEX_SHA256SUMS"
  fi
}

if [ -z "$install_dir" ] || [ "$install_dir" = "/.local/bin" ]; then
  fail "HOME is not set; set SQUIRE_INSTALL_DIR"
fi

os="$(detect_os)"
arch="$(detect_arch)"

if [ -z "$version" ]; then
  version="$(latest_version_for "$repo")"
fi
if [ -z "$version" ]; then
  fail "could not resolve latest release; set SQUIRE_VERSION"
fi
if [ -z "$codex_version" ]; then
  codex_version="$version"
fi

asset="squire_${version}_${os}_${arch}.tar.gz"
codex_asset="squire-codex_${codex_version}_${os}_${arch}.tar.gz"
tmp="${TMPDIR:-/tmp}/squire-install.$$"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

if [ -n "$artifact_dir" ]; then
  cp "$artifact_dir/$asset" "$tmp/$asset"
  cp "$artifact_dir/SHA256SUMS" "$tmp/SHA256SUMS"
else
  base="$github_base/$repo/releases/download/$version"
  fetch_file "$base/$asset" "$tmp/$asset"
  fetch_file "$base/SHA256SUMS" "$tmp/SHA256SUMS"
fi

verify_checksum "$tmp" "$asset"

tar -xzf "$tmp/$asset" -C "$tmp"
stage="$tmp/squire_${version}_${os}_${arch}"
codex_stage="$tmp/squire-codex_${codex_version}_${os}_${arch}"
binary="$stage/squire"
if [ ! -f "$binary" ]; then
  fail "archive did not contain squire binary"
fi

codex_binary="$codex_stage/squire-codex"
codex_helper="$codex_stage/codex-code-mode-host"
if fetch_codex_archive; then
  verify_checksum "$tmp" "$codex_asset" SQUIRE_CODEX_SHA256SUMS
  tar -xzf "$tmp/$codex_asset" -C "$tmp"
else
  fail "could not fetch matching squire-codex release artifact"
fi
if [ ! -f "$codex_binary" ]; then
  fail "archive did not contain squire-codex binary"
fi
if [ ! -f "$codex_helper" ]; then
  fail "archive did not contain the required codex-code-mode-host runtime helper"
fi

mkdir -p "$install_dir"
tmp_binary="$install_dir/.squire.tmp.$$"
cp "$binary" "$tmp_binary"
chmod 0755 "$tmp_binary"
mv "$tmp_binary" "$install_dir/squire"
tmp_codex_binary="$install_dir/.squire-codex.tmp.$$"
cp "$codex_binary" "$tmp_codex_binary"
chmod 0755 "$tmp_codex_binary"
mv "$tmp_codex_binary" "$install_dir/squire-codex"
codex_helper_name="codex-code-mode-host"
tmp_codex_helper="$install_dir/.${codex_helper_name}.tmp.$$"
cp "$codex_helper" "$tmp_codex_helper"
chmod 0755 "$tmp_codex_helper"
mv "$tmp_codex_helper" "$install_dir/$codex_helper_name"

echo "squire install: installed $install_dir/squire"
echo "squire install: installed $install_dir/squire-codex"
echo "squire install: installed $install_dir/$codex_helper_name"
"$install_dir/squire" version --short || true
runtime_ready=0
if install_prebuilt_runtime "$stage" "$codex_stage"; then
  runtime_ready=1
elif compile_hot_api "$stage"; then
  runtime_ready=1
fi
if [ "$runtime_ready" -ne 1 ]; then
  echo "squire install: warning: runtime unavailable; squire-codex will preserve native execution without acceleration" >&2
fi
case "${SQUIRE_INSTALL_ADVANCED:-0}" in
  1|true|yes)
    compile_preload "$stage"
    compile_preload_helper "$stage"
    compile_vm_darwin "$stage"
    ;;
esac
print_macos_shell_note
print_codex_note

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo "squire install: add this to your shell profile:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    ;;
esac

echo "squire install: no global command shims installed"
echo "squire install: repo state is created locally when Squire runs in a workspace"
"$install_dir/squire" doctor --short
echo "squire install: start with:"
echo "  squire codex"
