#!/usr/bin/env bash
#
# Create the Entra ID app registration Taut uses, with a certificate
# credential and the least-privilege Microsoft Graph application permissions.
#
# Requires: Azure CLI (az) signed in as a user who can create app
# registrations and grant admin consent (Privileged Role Administrator or
# Global Administrator), plus openssl and python3.
#
#   ./scripts/entra-app.sh                         # core permissions
#   ./scripts/entra-app.sh --private-channels      # + private/shared channels
#   ./scripts/entra-app.sh --direct-messages       # + DMs and group DMs
#   ./scripts/entra-app.sh --dry-run               # show what would happen
#
# The private key never leaves this machine. It is written to
# <out>/taut-app.pem with mode 0600; reference that path from the config file:
#
#   target.entra.credential.certificate: { file: ./taut-app.pem }

set -euo pipefail

NAME="Taut Migration"
OUT_DIR="."
VALID_DAYS=180
DRY_RUN=false
SKIP_CONSENT=false
WITH_PRIVATE=false
WITH_DMS=false

GRAPH_APP_ID="00000003-0000-0000-c000-000000000000"

# Application permissions (see docs/migration-guide.md, "Permissions", for what each is for).
CORE_PERMS=(
  Teamwork.Migrate.All
  User.Read.All
  Team.Create
  Channel.Create
  TeamMember.ReadWrite.All
  ChannelMessage.Read.All
  Files.ReadWrite.All
  Sites.ReadWrite.All
)
PRIVATE_PERMS=(ChannelMember.ReadWrite.All)
DM_PERMS=(Chat.Create Chat.ReadBasic.All Chat.Read.All)

usage() { sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'; }

die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) NAME="${2:?--name needs a value}"; shift 2 ;;
    --out) OUT_DIR="${2:?--out needs a value}"; shift 2 ;;
    --days) VALID_DAYS="${2:?--days needs a value}"; shift 2 ;;
    --private-channels) WITH_PRIVATE=true; shift ;;
    --direct-messages) WITH_DMS=true; shift ;;
    --skip-consent) SKIP_CONSENT=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

PERMS=("${CORE_PERMS[@]}")
$WITH_PRIVATE && PERMS+=("${PRIVATE_PERMS[@]}")
$WITH_DMS && PERMS+=("${DM_PERMS[@]}")

for tool in az openssl python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done

KEY_FILE="$OUT_DIR/taut-app.pem"
CERT_FILE="$OUT_DIR/taut-app.crt"

if $DRY_RUN; then
  say "Dry run. Nothing will be created."
  echo "    App registration : $NAME"
  echo "    Certificate      : $CERT_FILE (valid $VALID_DAYS days)"
  echo "    Private key      : $KEY_FILE (mode 0600, stays on this machine)"
  echo "    Graph application permissions:"
  printf '      - %s\n' "${PERMS[@]}"
  $SKIP_CONSENT && echo "    Admin consent    : skipped (--skip-consent)" || echo "    Admin consent    : granted for the tenant"
  exit 0
fi

az account show >/dev/null 2>&1 || die "not signed in. Run: az login --allow-no-subscriptions"
TENANT_ID="$(az account show --query tenantId -o tsv)"
say "Tenant $TENANT_ID"

[[ -e "$KEY_FILE" ]] && die "$KEY_FILE already exists; move it or pass --out <dir>"
mkdir -p "$OUT_DIR"

say "Generating a certificate (RSA 3072, $VALID_DAYS days)"
umask 077
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$VALID_DAYS" \
  -subj "/CN=$NAME" -keyout "$TMP/key.pem" -out "$TMP/cert.pem" >/dev/null 2>&1
cp "$TMP/cert.pem" "$CERT_FILE"
# The config file expects one PEM file holding both the certificate and its key.
cat "$TMP/cert.pem" "$TMP/key.pem" > "$KEY_FILE"
chmod 600 "$KEY_FILE"

say "Resolving Microsoft Graph permission IDs"
az ad sp show --id "$GRAPH_APP_ID" --query "appRoles[].{id:id,value:value}" -o json > "$TMP/roles.json"
RESOURCE_ACCESS="$(python3 - "$TMP/roles.json" "${PERMS[@]}" <<'PY'
import json, sys
roles = {r["value"]: r["id"] for r in json.load(open(sys.argv[1]))}
missing = [p for p in sys.argv[2:] if p not in roles]
if missing:
    sys.exit("unknown Graph application permissions: " + ", ".join(missing))
print(json.dumps([{
    "resourceAppId": "00000003-0000-0000-c000-000000000000",
    "resourceAccess": [{"id": roles[p], "type": "Role"} for p in sys.argv[2:]],
}]))
PY
)"
echo "$RESOURCE_ACCESS" > "$TMP/access.json"

say "Creating app registration \"$NAME\""
APP_ID="$(az ad app create \
  --display-name "$NAME" \
  --sign-in-audience AzureADMyOrg \
  --required-resource-accesses "@$TMP/access.json" \
  --query appId -o tsv)"

say "Uploading the certificate (public half only)"
az ad app credential reset --id "$APP_ID" --cert "@$CERT_FILE" --append >/dev/null

say "Creating the service principal"
az ad sp create --id "$APP_ID" >/dev/null 2>&1 || true

if $SKIP_CONSENT; then
  say "Skipping admin consent. An administrator must grant it before 'taut auth' will pass:"
  echo "    https://login.microsoftonline.com/$TENANT_ID/adminconsent?client_id=$APP_ID"
else
  say "Granting admin consent"
  # The service principal can take a few seconds to replicate.
  for attempt in 1 2 3 4 5 6; do
    if az ad app permission admin-consent --id "$APP_ID" >/dev/null 2>&1; then
      break
    fi
    [[ $attempt -eq 6 ]] && die "admin consent failed. Grant it in the portal, then run: taut auth"
    sleep 5
  done
fi

cat <<EOF

Done. Put these in your config file (or answer them in 'taut init'):

  target:
    entra:
      tenantId: $TENANT_ID
      clientId: $APP_ID
      credential:
        certificate: { file: $KEY_FILE }

The private key is in $KEY_FILE (mode 0600). Keep it out of version control.
Next: taut auth
EOF
