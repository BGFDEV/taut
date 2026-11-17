#!/usr/bin/env bash
#
# Write the local stack's .env with a freshly generated staging key. The key
# is for the local stack only; the file is git-ignored and must never be
# committed.

set -euo pipefail

OUT="${1:?usage: dev-env.sh <path to .env>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ -e "$OUT" ]]; then
  exit 0
fi
umask 077
printf 'TAUT_STAGING_KEY=%s\n' "$("$ROOT/bin/taut" dev keygen)" > "$OUT"
echo "generated $OUT (a local-only staging key; never commit it)"
