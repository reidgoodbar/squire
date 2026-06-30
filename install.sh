#!/usr/bin/env sh
set -eu

repo="${SQUIRE_REPO:-${SQUIRE_KERNEL_REPO:-reidgoodbar/squire}}"
version="${SQUIRE_VERSION:-${SQUIRE_KERNEL_VERSION:-}}"
install_dir="${SQUIRE_INSTALL_DIR:-${HOME:-}/.local/bin}"
github_base="${GITHUB_BASE_URL:-https://github.com}"
github_api="${GITHUB_API_URL:-https://api.github.com}"
artifact_dir="${SQUIRE_ARTIFACT_DIR:-${SQUIRE_KERNEL_ARTIFACT_DIR:-}}"

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

compile_mmap_shim() {
  stage="$1"
  shim_src="$stage/shims/squire_mmap_shim.c"
  shim_out="$install_dir/squire-mmap-shim"
  if [ ! -f "$shim_src" ]; then
    echo "squire install: mmap shim source not present in archive"
    return 0
  fi
  if ! have cc; then
    echo "squire install: cc not found; skipped optional scoped mmap shim"
    return 0
  fi
  tmp_shim="$install_dir/.squire-mmap-shim.tmp.$$"
  case "$os" in
    darwin)
      if cc -O3 -DNDEBUG -o "$tmp_shim" "$shim_src"; then
        chmod 0755 "$tmp_shim"
        mv "$tmp_shim" "$shim_out"
        echo "squire install: installed $shim_out"
      else
        rm -f "$tmp_shim"
        echo "squire install: optional scoped mmap shim build failed; hardened launchers run native"
      fi
      ;;
    linux)
      if cc -O3 -DNDEBUG -o "$tmp_shim" "$shim_src" -lcrypto; then
        chmod 0755 "$tmp_shim"
        mv "$tmp_shim" "$shim_out"
        echo "squire install: installed $shim_out"
      else
        rm -f "$tmp_shim"
        echo "squire install: optional scoped mmap shim build failed; install libssl headers to enable it"
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

  if [ -x /opt/homebrew/bin/zsh ]; then
    echo "squire install: macOS note: Homebrew zsh detected at /opt/homebrew/bin/zsh"
    echo "squire install: shell preload is release-gated; Codex uses scoped mmap shim fallback"
    return 0
  fi
  if [ -x /usr/local/bin/zsh ]; then
    echo "squire install: macOS note: Homebrew zsh detected at /usr/local/bin/zsh"
    echo "squire install: shell preload is release-gated; Codex uses scoped mmap shim fallback"
    return 0
  fi

  echo "squire install: macOS note: Apple /bin/zsh ignores DYLD_INSERT_LIBRARIES"
  echo "squire install: Homebrew zsh is optional for future preload-safe shell integrations:"
  echo "  brew install zsh"
  echo "squire install: Squire works without it; Codex uses scoped mmap shim fallback"
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

latest_version() {
  fetch_stdout "$github_api/repos/$repo/releases?per_page=1" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

verify_checksum() {
  work_dir="$1"
  asset="$2"
  sums="$work_dir/SHA256SUMS"
  one="$work_dir/SHA256SUMS.one"

  grep " $asset\$" "$sums" > "$one" || fail "SHA256SUMS does not contain $asset"
  (
    cd "$work_dir"
    if have sha256sum; then
      sha256sum -c SHA256SUMS.one >/dev/null
    elif have shasum; then
      shasum -a 256 -c SHA256SUMS.one >/dev/null
    else
      fail "sha256sum or shasum is required to verify downloads"
    fi
  )
}

if [ -z "$install_dir" ] || [ "$install_dir" = "/.local/bin" ]; then
  fail "HOME is not set; set SQUIRE_INSTALL_DIR"
fi

os="$(detect_os)"
arch="$(detect_arch)"

if [ -z "$version" ]; then
  version="$(latest_version)"
fi
if [ -z "$version" ]; then
  fail "could not resolve latest release; set SQUIRE_VERSION"
fi

asset="squire_${version}_${os}_${arch}.tar.gz"
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
binary="$stage/squire"
if [ ! -f "$binary" ]; then
  fail "archive did not contain squire binary"
fi

mkdir -p "$install_dir"
tmp_binary="$install_dir/.squire.tmp.$$"
cp "$binary" "$tmp_binary"
chmod 0755 "$tmp_binary"
mv "$tmp_binary" "$install_dir/squire"

echo "squire install: installed $install_dir/squire"
"$install_dir/squire" version --short || true
compile_preload "$stage"
compile_mmap_shim "$stage"
compile_preload_helper "$stage"
compile_vm_darwin "$stage"
print_macos_shell_note

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo "squire install: add this to your shell profile:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    ;;
esac

echo "squire install: no global command shims installed"
echo "squire install: repo state is created locally when Squire runs in a workspace"
