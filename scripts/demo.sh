#!/usr/bin/env bash
#
# End-to-end demo of init, auth and preflight against the "medium"
# fixture export and the fake Entra / Graph / Slack servers. No real tenant,
# no real credentials. Used by `make demo` and by CI.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TAUT="$ROOT/bin/taut"
WORK="$(mktemp -d)"
PORT="${TAUT_DEMO_PORT:-18080}"
BASE="http://127.0.0.1:$PORT"

[[ -x "$TAUT" ]] || { echo "bin/taut not found; run 'make build' first" >&2; exit 1; }
[[ -f "$ROOT/testdata/fixtures/medium.zip" ]] || "$TAUT" dev fixtures --out "$ROOT/testdata/fixtures" >/dev/null

"$TAUT" dev fakegraph --addr "127.0.0.1:$PORT" --fixture medium 2>"$WORK/fake.log" &
FAKE_PID=$!
trap 'kill "$FAKE_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT

for _ in $(seq 1 50); do
  "$TAUT" dev healthcheck "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

# The fake tenant's well-known values. These are not real credentials.
seed() { "$TAUT" dev seed | sed -n "s/.*\"$1\": \"\(.*\)\".*/\1/p"; }
export TAUT_SLACK_TOKEN="$(seed slackToken)"
export TAUT_ENTRA_CLIENT_SECRET="$(seed clientSecret)"
CONFIG="$WORK/taut.yaml"

step() { printf '\n\033[1m$ taut %s\033[0m\n' "$*"; }

step init --yes ...
"$TAUT" init --yes --config "$CONFIG" \
  --name fixture-medium \
  --export "$ROOT/testdata/fixtures/medium.zip" \
  --tenant-id "$(seed tenantId)" --client-id "$(seed clientId)" \
  --client-secret-env TAUT_ENTRA_CLIENT_SECRET \
  --team "Fixture Corp" --owner "$(seed owner)" --unmapped-account "$(seed unmappedAccount)" \
  --authority "$BASE" --graph-url "$BASE" --slack-api-url "$BASE/slack/api"

step auth
"$TAUT" auth --config "$CONFIG"

step preflight
"$TAUT" preflight --config "$CONFIG"

step preflight --json
"$TAUT" preflight --config "$CONFIG" --json > "$WORK/preflight.json"
echo "wrote a $(wc -c < "$WORK/preflight.json" | tr -d ' ')-byte JSON report"
