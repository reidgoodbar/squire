#!/usr/bin/env sh
set -eu

squire_bin="${1:-${SQUIRE_BIN:-}}"
if [ -z "$squire_bin" ]; then
  if command -v squire >/dev/null 2>&1; then
    squire_bin=$(command -v squire)
  else
    echo "usage: scripts/release_smoke.sh /path/to/squire" >&2
    echo "or set SQUIRE_BIN=/path/to/squire" >&2
    exit 2
  fi
fi
case "$squire_bin" in
  */*)
    squire_dir=$(dirname "$squire_bin")
    squire_base=$(basename "$squire_bin")
    squire_abs_dir=$(cd "$squire_dir" && pwd -P)
    squire_bin="$squire_abs_dir/$squire_base"
    ;;
  *)
    resolved_squire_bin=$(command -v "$squire_bin" || true)
    if [ -z "$resolved_squire_bin" ]; then
      echo "release smoke failed: squire binary not found on PATH: $squire_bin" >&2
      exit 2
    fi
    squire_bin="$resolved_squire_bin"
    ;;
esac
if [ ! -x "$squire_bin" ]; then
  echo "release smoke failed: squire binary is not executable: $squire_bin" >&2
  exit 2
fi

tmp_parent=${TMPDIR:-/tmp}
tmp_parent=${tmp_parent%/}
tmpdir=$(mktemp -d "$tmp_parent/squire-release-smoke.XXXXXX")
stopped=0

cleanup() {
  if [ "$stopped" -eq 0 ] && [ -d "$tmpdir/.git" ]; then
    (cd "$tmpdir" && "$squire_bin" runtime maintain --stop --short >/dev/null 2>&1) || true
  fi
}
trap cleanup EXIT INT TERM

require_contains() {
  haystack=$1
  needle=$2
  label=$3
  case "$haystack" in
    *"$needle"*) ;;
    *)
      echo "release smoke failed: $label missing '$needle'" >&2
      echo "$haystack" >&2
      exit 1
      ;;
  esac
}

echo "release_smoke_repo: $tmpdir"
cd "$tmpdir"

git init >/dev/null
git config user.email squire@example.invalid
git config user.name "Squire Release"
printf "hello\n" > README.md
git add README.md
git commit -m init >/dev/null

setup_out=$("$squire_bin" setup)
require_contains "$setup_out" "privacy_mode: standard" "setup"
require_contains "$setup_out" "global_shims: not installed" "setup"

start_out=$("$squire_bin" runtime maintain --background --short)
require_contains "$start_out" "status: started" "maintainer start"
require_contains "$start_out" "running: true" "maintainer start"
require_contains "$start_out" "native_fallback: true" "maintainer start"
require_contains "$start_out" "agent_visible_suggestions: false" "maintainer start"

mkdir -p .squire
cat > .squire/checks.toml <<'EOF'
version = 1
quiescence = "500ms"
poll_interval = "5s"

[[check]]
name = "git-diff-check"
command = ["sh", "-c", "sleep 0.3; git diff --check"]
inputs = ["README.md"]
timeout = "30s"
EOF

if "$squire_bin" verify --short >"$tmpdir/verify-untrusted.out" 2>&1; then
  echo "release smoke failed: untrusted Green config verified successfully" >&2
  exit 1
fi
verify_untrusted=$(cat "$tmpdir/verify-untrusted.out")
require_contains "$verify_untrusted" "SQUIRE UNTRUSTED" "Green untrusted gate"

trust_out=$("$squire_bin" green trust --short)
require_contains "$trust_out" "config trusted" "Green trust"
require_contains "$trust_out" "git-diff-check" "Green trust check list"

green_ready=0
green_attempt=0
while [ "$green_attempt" -lt 100 ]; do
  if "$squire_bin" verify --short >"$tmpdir/verify-green.out" 2>&1; then
    green_ready=1
    break
  fi
  green_attempt=$((green_attempt + 1))
  sleep 0.1
done
if [ "$green_ready" -ne 1 ]; then
  echo "release smoke failed: trusted Green check did not become current" >&2
  cat "$tmpdir/verify-green.out" >&2 || true
  exit 1
fi
verify_green=$(cat "$tmpdir/verify-green.out")
require_contains "$verify_green" "SQUIRE GREEN" "Green current state"
require_contains "$verify_green" "git-diff-check: PASS current" "Green current check"

printf "changed\n" >> README.md
if "$squire_bin" verify --short >"$tmpdir/verify-stale.out" 2>&1; then
  echo "release smoke failed: changed Green input remained current" >&2
  exit 1
fi
verify_stale=$(cat "$tmpdir/verify-stale.out")
case "$verify_stale" in
  *STALE*|*RUNNING*) ;;
  *)
    echo "release smoke failed: changed Green input was non-current for an unexpected reason" >&2
    echo "$verify_stale" >&2
    exit 1
    ;;
esac

green_ready=0
green_attempt=0
while [ "$green_attempt" -lt 100 ]; do
  if "$squire_bin" verify --short >"$tmpdir/verify-green-after-edit.out" 2>&1; then
    green_ready=1
    break
  fi
  green_attempt=$((green_attempt + 1))
  sleep 0.1
done
if [ "$green_ready" -ne 1 ]; then
  echo "release smoke failed: changed Green input was not revalidated" >&2
  cat "$tmpdir/verify-green-after-edit.out" >&2 || true
  exit 1
fi

printf "\n" >> .squire/checks.toml
if "$squire_bin" verify --short >"$tmpdir/verify-revoked.out" 2>&1; then
  echo "release smoke failed: changed Green config retained trust" >&2
  exit 1
fi
verify_revoked=$(cat "$tmpdir/verify-revoked.out")
require_contains "$verify_revoked" "SQUIRE UNTRUSTED" "Green config invalidation"

warm_out=$("$squire_bin" runtime warm --short)
require_contains "$warm_out" "privacy_mode: standard" "warm"
require_contains "$warm_out" "replay_set_unchanged: true" "warm"

status_out=$("$squire_bin" runtime status --short)
require_contains "$status_out" "repo_oracle: available" "runtime status"
require_contains "$status_out" "native_fallback: available" "runtime status"
require_contains "$status_out" "runtime_decisions: replay_or_native" "runtime status"

native_head=$(git rev-parse HEAD)
expected_head_b64=$(printf "%s\n" "$native_head" | base64 | tr -d '\n')
adapter_out=$(printf '%s\n' '{"id":"head","argv":["git","rev-parse","HEAD"],"session_id":"release-smoke"}' | "$squire_bin" runtime adapter --stdio)
require_contains "$adapter_out" '"id":"head"' "adapter"
require_contains "$adapter_out" '"ok":true' "adapter"
require_contains "$adapter_out" '"exit_code":0' "adapter"
require_contains "$adapter_out" '"mode":"replay"' "adapter"
require_contains "$adapter_out" "\"stdout_b64\":\"$expected_head_b64\"" "adapter"

squire_head=$("$squire_bin" runtime run -- git rev-parse HEAD)
if [ "$squire_head" != "$native_head" ]; then
  echo "release smoke failed: git rev-parse HEAD mismatch" >&2
  echo "native: $native_head" >&2
  echo "squire: $squire_head" >&2
  exit 1
fi

native_status=$(git status --short)
squire_status=$("$squire_bin" runtime run -- git status --short)
if [ "$squire_status" != "$native_status" ]; then
  echo "release smoke failed: git status --short mismatch" >&2
  echo "native: $native_status" >&2
  echo "squire: $squire_status" >&2
  exit 1
fi

boost_out=$("$squire_bin" boost status --short)
require_contains "$boost_out" "native_fallback_available: true" "boost status"
require_contains "$boost_out" "runtime_decisions: replay_or_native" "boost status"
require_contains "$boost_out" "replays: 3" "boost status"
require_contains "$boost_out" "hot_client_replays: 3" "boost status"

stop_out=$("$squire_bin" runtime maintain --stop --short)
stopped=1
require_contains "$stop_out" "status: stopped" "maintainer stop"
require_contains "$stop_out" "running: false" "maintainer stop"

echo "release_smoke: pass"
