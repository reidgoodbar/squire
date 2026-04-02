#!/bin/sh
set -eu

if [ -z "${SQUIRE_TOKEN:-}" ]; then
	echo "SQUIRE_TOKEN is required. Run 'squire mcp login' locally to generate MCP env vars." >&2
	exit 64
fi

home_dir="${HOME:-/home/squire}"
config_dir="$home_dir/.squire"
config_path="$config_dir/config.json"
api_base_url="${SQUIRE_API_BASE_URL:-https://api.squire.run}"

mkdir -p "$config_dir"

cat >"$config_path" <<EOF
{
  "api_base_url": "${api_base_url}",
  "session_token": "${SQUIRE_TOKEN}",
  "created_at": "1970-01-01T00:00:00Z"
}
EOF

exec /usr/local/bin/squire mcp serve
