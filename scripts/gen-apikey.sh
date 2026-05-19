#!/usr/bin/env bash
# gen-apikey.sh — emit one or more DocPipe API keys.
#
# Usage:
#   ./scripts/gen-apikey.sh                       # one anonymous key
#   ./scripts/gen-apikey.sh portal                # one named key
#   ./scripts/gen-apikey.sh portal internal admin # multiple named keys
#
# Keys are formatted as ak_live_<32 hex>. Names produce a name:secret pair
# suitable for direct paste into DOCPIPE_API_KEYS.

set -euo pipefail

gen_secret() {
  # 32 bytes of randomness rendered as 64 hex chars.
  openssl rand -hex 32
}

if [[ $# -eq 0 ]]; then
  echo "ak_live_$(gen_secret)"
  exit 0
fi

pairs=()
for name in "$@"; do
  if ! [[ "$name" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "invalid key name (use A-Za-z0-9_-): $name" >&2
    exit 1
  fi
  pairs+=("${name}:ak_live_$(gen_secret)")
done

# Print one per line for readability, then a single comma-joined line to paste.
printf '%s\n' "${pairs[@]}"
echo
echo "# Paste as DOCPIPE_API_KEYS:"
( IFS=,; echo "DOCPIPE_API_KEYS=${pairs[*]}" )
