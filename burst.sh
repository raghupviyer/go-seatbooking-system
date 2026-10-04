#!/usr/bin/env bash
# Usage: ./burst.sh <BASE_URL> [flags]   e.g. ./burst.sh https://my-app.example.com -users 500
set -euo pipefail
cd "$(dirname "$0")"
base="${1:-${BASE_URL:-}}"
[ -n "$base" ] || { echo "usage: $0 <BASE_URL> [flags]" >&2; exit 2; }
shift || true
exec go run ./cmd/burst "$@" "$base"
