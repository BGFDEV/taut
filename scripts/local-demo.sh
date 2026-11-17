#!/usr/bin/env bash
#
# A whole migration in local mode: init, preflight, migrate, an incremental
# pass from a later export, finalize and audit verification, with one taut
# process at a time and no Docker. It runs against the "small" fixture pair
# and the fake Entra / Graph / Slack servers. No real tenant, no real
# credentials. Used by `make local-demo`.
#
# Local mode keeps its ledger in a PostgreSQL server it starts itself. If
# none is installed, set TAUT_DEMO_DOWNLOAD_POSTGRES=1 to let taut download
# one from Maven Central (15 to 30 MB, kept in the user cache directory).
#
# TAUT_DEMO_KILL=1 also kills `taut migrate` part-way and resumes it.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TAUT="$ROOT/bin/taut"
WORK="$(mktemp -d)"
PORT="${TAUT_DEMO_PORT:-18081}"
BASE="http://127.0.0.1:$PORT"
FIXTURES="$ROOT/testdata/fixtures"

[[ -x "$TAUT" ]] || { echo "bin/taut not found; run 'make build' first" >&2; exit 1; }
[[ -f "$FIXTURES/small-later.zip" ]] || "$TAUT" dev fixtures --out "$FIXTURES" >/dev/null

# The fake tenant knows the people of the later export, which include the
# earlier one's.
"$TAUT" dev fakegraph --addr "127.0.0.1:$PORT" --fixture small-later 2>"$WORK/fake.log" &
FAKE_PID=$!
trap 'kill "$FAKE_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT

for _ in $(seq 1 50); do
  "$TAUT" dev healthcheck "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

seed() { "$TAUT" dev seed | sed -n "s/.*\"$1\": \"\(.*\)\".*/\1/p"; }
export TAUT_SLACK_TOKEN="$(seed slackToken)"
export TAUT_ENTRA_CLIENT_SECRET="$(seed clientSecret)"
# The fakes: let the local stack accept a config file that names them, and
# fetch Slack files from the fake file host. Never set these with a real
# tenant.
export TAUT_ALLOW_TEST_ENDPOINTS=1
export TAUT_SLACK_FILES_URL="$BASE"
# The fake has no rate limits worth waiting for in a demo.
export TAUT_GRAPH_CHANNEL_RPS="${TAUT_GRAPH_CHANNEL_RPS:-20}"
export TAUT_GRAPH_CHANNEL_MAX="${TAUT_GRAPH_CHANNEL_MAX:-20}"

CONFIG="$WORK/taut.yaml"
cp "$FIXTURES/small.zip" "$WORK/export.zip"

step() { printf '\n\033[1m$ taut %s\033[0m\n' "$*"; }

DOWNLOAD=()
[[ "${TAUT_DEMO_DOWNLOAD_POSTGRES:-}" == "1" ]] && DOWNLOAD=(--download-postgres)

step init --yes --local ...
"$TAUT" init --yes --local "${DOWNLOAD[@]}" --config "$CONFIG" \
  --name local-demo \
  --export "$WORK/export.zip" \
  --tenant-id "$(seed tenantId)" --client-id "$(seed clientId)" \
  --client-secret-env TAUT_ENTRA_CLIENT_SECRET \
  --team "Fixture Corp" --owner "$(seed owner)" --unmapped-account "$(seed unmappedAccount)" \
  --authority "$BASE" --graph-url "$BASE" --slack-api-url "$BASE/slack/api"

step doctor
"$TAUT" doctor --config "$CONFIG"

step preflight
"$TAUT" preflight --config "$CONFIG" | tail -n 6

if [[ "${TAUT_DEMO_KILL:-}" == "1" ]]; then
  step migrate --yes "   (killed part-way with SIGKILL)"
  # At one message a second per channel the import is still going when the
  # kill arrives.
  TAUT_GRAPH_CHANNEL_RPS=1 TAUT_GRAPH_CHANNEL_MAX=1 \
    "$TAUT" migrate --yes --config "$CONFIG" >"$WORK/killed.log" 2>&1 &
  PID=$!
  sleep 8
  kill -9 "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  tail -n 3 "$WORK/killed.log" || true
  echo "  killed; the next migrate resumes from the ledger"
fi

step migrate --yes
"$TAUT" migrate --yes --config "$CONFIG"

step status
"$TAUT" status --config "$CONFIG"

# People kept using Slack. A later export of the same workspace arrives.
cp "$FIXTURES/small-later.zip" "$WORK/export.zip"

step preflight --incremental
"$TAUT" preflight --incremental --config "$CONFIG" | sed -n '/Incremental/,/Plan/p' | sed '$d'

step migrate --incremental --yes
"$TAUT" migrate --incremental --yes --config "$CONFIG"

step finalize --yes
"$TAUT" finalize --yes --config "$CONFIG"

step audit --verify
"$TAUT" audit --verify --yes --config "$CONFIG"
step audit --pass 1 --verify
"$TAUT" audit --pass 1 --verify --yes --config "$CONFIG"

printf '\nThe state directory held %s in %s; it is removed with the demo.\n' "$(du -sh "$WORK/.taut" | cut -f1)" "$WORK/.taut"
