#!/usr/bin/env bash
#
# Import the "medium" fixture export into the fake Microsoft tenant through
# the local stack: the real CLI, the controller and worker containers, Postgres,
# NATS and encrypted S3 staging. Requires `make up`.
#
# With TAUT_KILL=1 it also kills worker containers while the import runs
# and brings replacements up, then checks that the tenant holds every
# message exactly once.
#
# With TAUT_DMS=1 direct messages and group DMs are migrated too, into chats
# of the fake tenant, and the chats are checked: one per conversation, with
# exactly its messages. It needs a fresh stack (`make down && make up`): a
# one-on-one chat is unique to its two people, so a second run would import
# into the chats of the first and find its own messages already there.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TAUT="$ROOT/bin/taut"
COMPOSE=(docker compose -f "$ROOT/deploy/compose/compose.yaml")
FAKE="http://127.0.0.1:8080"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/taut-migrate-demo.XXXXXX")"
cleanup() { [[ -d "$WORK" && "$WORK" == *taut-migrate-demo.* ]] && rm -r -- "$WORK"; }
trap cleanup EXIT

"$TAUT" dev healthcheck http://127.0.0.1:7070/readyz >/dev/null 2>&1 || {
  echo "The local stack is not running. Start it with: make up" >&2; exit 1; }

seed() { "$TAUT" dev seed | sed -n "s/.*\"$1\": \"\(.*\)\".*/\1/p"; }
export TAUT_SLACK_TOKEN="$(seed slackToken)"
export TAUT_ENTRA_CLIENT_SECRET="$(seed clientSecret)"
NAME="demo-$(date +%H%M%S)"
CONFIG="$WORK/taut.yaml"
step() { printf '\n\033[1m$ taut %s\033[0m\n' "$*"; }
state() { curl -fsS "$FAKE/_fake/state"; }
count() { python3 -c "import json,sys; d=json.load(sys.stdin); print(d['stats']['$1'])"; }

before="$(state | count importsAccepted)"

INIT_EXTRA=()
if [[ "${TAUT_DMS:-}" == "1" ]]; then
  existing="$(state | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('chats') or []))")"
  if [[ "$existing" != "0" ]]; then
    echo "The fake tenant already has $existing chats from an earlier run. TAUT_DMS=1 needs a fresh stack: make down && make up" >&2
    exit 1
  fi
  INIT_EXTRA=(--include-direct-messages)
fi

step init
"$TAUT" init --yes --config "$CONFIG" \
  --name "$NAME" --export "$ROOT/testdata/fixtures/medium.zip" \
  --tenant-id "$(seed tenantId)" --client-id "$(seed clientId)" \
  --client-secret-env TAUT_ENTRA_CLIENT_SECRET \
  --team "Fixture Corp $NAME" --owner "$(seed owner)" --unmapped-account "$(seed unmappedAccount)" \
  --authority "$FAKE" --graph-url "$FAKE" --slack-api-url "$FAKE/slack/api" ${INIT_EXTRA[@]+"${INIT_EXTRA[@]}"} >/dev/null

if [[ "${TAUT_KILL:-}" != "1" ]]; then
  step migrate --yes
  "$TAUT" migrate --yes --config "$CONFIG"
else
  step migrate --yes --detach
  "$TAUT" migrate --yes --detach --config "$CONFIG"
  kills=0
  for round in 1 2 3 4 5 6; do
    sleep 2
    # Work through workers and their egress proxies in turn, so each kind of
    # container dies mid-work at least once.
    services=(worker-a egress-proxy-b worker-b egress-proxy-a worker-a worker-b)
    service="${services[$(( round - 1 ))]}"
    victim="$("${COMPOSE[@]}" ps -q "$service" | head -n 1)"
    [[ -n "$victim" ]] || continue
    docker kill "$victim" >/dev/null && kills=$((kills + 1))
    printf '  killed %s\n' "$service"
    "${COMPOSE[@]}" up -d --no-recreate "$service" >/dev/null 2>&1
  done
  printf '  %d containers killed mid-run\n' "$kills"
  step status --watch
  "$TAUT" status --watch --config "$CONFIG"
fi

step status --json
"$TAUT" status --json --config "$CONFIG" > "$WORK/status.json"
posted="$(python3 -c "import json; print(json.load(open('$WORK/status.json'))['progress']['posted'])")"
status="$(python3 -c "import json; print(json.load(open('$WORK/status.json'))['status'])")"
after="$(state | count importsAccepted)"
accepted=$((after - before))

echo
echo "  run status                 $status"
echo "  posts recorded by Taut     $posted"
echo "  messages Graph accepted    $accepted"
if [[ "$status" == "completed" && "$posted" == "$accepted" && "$posted" -gt 0 ]]; then
  echo "  result                     every message is in Teams exactly once"
else
  echo "  result                     MISMATCH" >&2
  exit 1
fi

# With TAUT_DMS=1, the chats: one per direct-message conversation of the run,
# holding exactly the posts Taut recorded for it, each once.
check_chats() { # $1: the migration mode every chat must be in
  state > "$WORK/state.json"
  python3 - "$WORK/status.json" "$WORK/state.json" "$1" <<'PY'
import json, sys
status, state, mode = json.load(open(sys.argv[1])), json.load(open(sys.argv[2])), sys.argv[3]
convs = [c for c in status["progress"]["channels"] if c.get("kind") in ("dm", "mpim")]
chats = state.get("chats") or []
posted = sum(c["posted"] for c in convs)
imported = sum(1 for chat in chats for m in chat["messages"] if m.get("importedBy"))
ids = [m["id"] for chat in chats for m in chat["messages"]]
wrong = [chat["id"] for chat in chats if chat.get("migrationMode") != mode]
print()
print("  direct-message conversations  %d (%d one-on-one, %d group)" % (
    len(convs), sum(c["kind"] == "dm" for c in convs), sum(c["kind"] == "mpim" for c in convs)))
print("  chats in the tenant           %d" % len(chats))
print("  posts recorded for them       %d" % posted)
print("  messages in the chats         %d" % imported)
ok = (len(convs) > 0 and len(chats) == len(convs) and posted == imported and posted > 0
      and not wrong and all(c["failed"] == 0 for c in convs))
if not ok:
    if wrong:
        print("  chats not %s: %s" % (mode, ", ".join(wrong)), file=sys.stderr)
    print("  result                        MISMATCH", file=sys.stderr)
    sys.exit(1)
print("  result                        every conversation is in one chat, every message once; migration mode %s" % mode)
PY
}
if [[ "${TAUT_DMS:-}" == "1" ]]; then
  check_chats inProgress
fi

# With TAUT_FINALIZE=1, finalize as well: open the channels, add members, purge
# staging, and check the audit trail.
if [[ "${TAUT_FINALIZE:-}" == "1" ]]; then
  step finalize --yes
  "$TAUT" finalize --yes --config "$CONFIG"
  step audit --verify
  "$TAUT" audit --verify --config "$CONFIG"
  "$TAUT" status --json --config "$CONFIG" > "$WORK/status.json"
  status="$(python3 -c "import json; print(json.load(open('$WORK/status.json'))['status'])")"
  purged="$(python3 -c "import json; print(json.load(open('$WORK/status.json')).get('purgedAt') or '')")"
  echo
  echo "  run status                 $status"
  echo "  staging purged             ${purged:-no}"
  if [[ "$status" != "finalized" || -z "$purged" ]]; then
    echo "  result                     NOT FINALIZED" >&2
    exit 1
  fi
  echo "  result                     finalized; staging is gone"
  if [[ "${TAUT_DMS:-}" == "1" ]]; then
    check_chats completed
  fi
fi
