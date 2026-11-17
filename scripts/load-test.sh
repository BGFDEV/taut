#!/usr/bin/env bash
#
# Load-test Taut against the local Compose stack: how fast it goes, what
# limits it, and whether it stays correct at size. docs/LOADTEST.md explains
# the profiles and how to read the report.
#
#   make load-test                       ceiling: no Graph limits, 2 workers
#   make load-test PROFILE=throttled     Microsoft's documented limits, time-boxed
#   make load-test PROFILE=scaling       ceiling at 2, 4 and 8 workers, side by side
#   make load-test PROFILE=smoke         a small ceiling run, under a minute
#   make load-test WORKERS=8 SPEC=channels=300,messages=1000000
#
# It starts a fresh stack for every run and removes it afterwards (volumes
# included), so do not run it beside a stack you want to keep. The generated
# export lives in a temporary directory; the JSON report is written under
# testdata/loadtest/ (git-ignored) and its path is printed.
#
# Needs docker with compose, python3 and the built binary and image, which
# `make load-test` provides.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE=(docker compose -f "$ROOT/deploy/compose/compose.yaml" -f "$ROOT/deploy/compose/compose.loadtest.yaml")

command -v python3 >/dev/null || { echo "python3 is needed" >&2; exit 1; }
[[ -x "$ROOT/bin/taut" ]] || { echo "bin/taut is missing: run make build" >&2; exit 1; }

# The stack's staging key, as `make up` generates it.
"$ROOT/scripts/dev-env.sh" "$ROOT/deploy/compose/.env"

case "${1:-}" in
  --self-test | -h | --help) exec python3 "$ROOT/scripts/loadtest.py" "$@" ;;
esac

# Build the image once; every run then only starts containers.
"${COMPOSE[@]}" build >/dev/null

exec python3 "$ROOT/scripts/loadtest.py" "$@"
