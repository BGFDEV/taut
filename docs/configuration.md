# Configuration

After reading this page you will be able to write or review a `taut.yaml`
by hand: every field, its default, and the rules `taut` checks before it
accepts the file.

The config file is the declarative plan for one migration. It says which
Slack export to read, which tenant and teams to write to, and how to map
people and content. It contains no secrets and is safe to commit.

**Contents**

- [How the file is read](#how-the-file-is-read)
- [Secret references](#secret-references)
- [Full example](#full-example)
- [Fields](#fields)
- [Rules across fields](#rules-across-fields)
- [What is not in the config file](#what-is-not-in-the-config-file)

## How the file is read

- The default path is `taut.yaml` in the working directory. Pass another
  with `--config`.
- Parsing is strict. An unknown field is an error with its line number.
- Omitted optional fields take the defaults below.
- Relative paths (the export, the override CSV, secret files) resolve
  against the directory of the config file, not the working directory.
- Validation reports every problem at once, each with its field path and
  line. Errors must be fixed. Warnings are advisory.
- Validation reads neither the environment nor the filesystem. Whether a
  referenced secret or file exists is checked by `taut doctor` and
  `taut auth`.

The quickest way to get a valid file is `taut init`. To check one you
edited:

```bash
taut preflight --offline
```

## Secret references

A credential is always named by reference. A reference has exactly one of
two forms:

```yaml
token: { env: TAUT_SLACK_TOKEN }        # an environment variable
certificate: { file: ./taut-app.pem }   # a file; relative to the config file
```

| Rule | Detail |
|---|---|
| Exactly one of `env` or `file` | Both, or neither, is an error |
| `env` is a variable name | Letters, digits and underscores, not starting with a digit |
| The reference is a name, not the secret | A value that looks like a token, key or client secret is rejected |

Taut scans the whole file, not only the reference fields. Any value that
looks like a literal credential makes the file invalid. A trailing newline
in a secret file is trimmed.

> [!IMPORTANT]
> On Kubernetes the token broker reads credentials only from fixed paths
> under `/run/secrets/taut`, and the config file must reference exactly
> those. See [OPERATIONS.md](OPERATIONS.md#what-the-config-file-must-say).

## Full example

<details open>
<summary><code>taut.yaml</code>, annotated</summary>

```yaml
apiVersion: taut.dev/v1alpha1
kind: Config
metadata:
  name: acme-migration            # 1-63 lowercase letters, digits, hyphens

# Where the data comes from.
source:
  slack:
    export: ./export.zip          # the export ZIP or its extracted folder
    api:
      enabled: true               # scope check in `taut auth`, file downloads
      token: { env: TAUT_SLACK_TOKEN }

# Where it goes.
target:
  entra:
    tenantId: 3f2504e0-4f89-11d3-9a0c-0305e82c3301
    clientId: 9b2f6d1c-7a44-4c0e-8f13-5d2a1e6b7c90
    cloud: global                 # global | usgov | usgovhigh | dod | china
    credential:
      certificate: { file: ./taut-app.pem }
      # or, less safe:  clientSecret: { env: TAUT_ENTRA_CLIENT_SECRET }
  teams:
    - name: Acme
      description: Migrated from Slack
      owner: admin@acme.com       # UPN of the initial owner
      visibility: private         # private | public
      channels:
        include: ['*']            # globs on Slack channel names
        exclude: ['tmp-*', 'sales-*']
        types: [public, private]  # private needs the modern strategy
        rename:
          dev: Engineering        # Slack name: Teams name
    - name: Acme Sales
      owner: sales-lead@acme.com
      visibility: private
      channels:
        include: ['sales-*']
        types: [public]
  directMessages:
    enabled: false                # off unless asked for

# How Slack users map to Entra users.
identity:
  overrides: ./overrides.csv      # optional: slack_user_id,slack_email,entra
  unmapped:
    policy: former-member         # former-member | service-account | block
    account: former@acme.com      # required unless the policy is block

# How messages are converted, and an optional date range.
content:
  reactions: footer               # footer | native | drop
  customEmoji: text               # text | image
  since: 2020-01-01               # optional, inclusive, YYYY-MM-DD
  until: 2026-06-30               # optional, inclusive

# How messages are imported.
load:
  strategy: modern                # modern | legacy

# Object store for staged data. Encrypted by Taut before upload.
staging:
  endpoint: http://localhost:9000
  region: us-east-1
  bucket: taut-staging
  accessKey: { env: TAUT_S3_ACCESS_KEY }
  secretKey: { env: TAUT_S3_SECRET_KEY }
  encryptionKey: { env: TAUT_STAGING_KEY }

# The controller that runs the migration.
controller:
  url: http://localhost:7070
```

</details>

## Fields

### Header

| Field | Required | Default | Rule |
|---|---|---|---|
| `apiVersion` | Yes | | Must be `taut.dev/v1alpha1` |
| `kind` | Yes | | Must be `Config` |
| `metadata.name` | Yes | | 1 to 63 lowercase letters, digits and hyphens, starting and ending with a letter or digit |

### `source.slack`

| Field | Required | Default | Rule |
|---|---|---|---|
| `export` | Yes | | Path of the export ZIP or its extracted directory |
| `api.enabled` | No | `false` (`taut init` writes `true`) | |
| `api.token` | When `api.enabled` | | A [secret reference](#secret-references) |
| `api.baseURL` | No | | Tests only. An absolute `http` or `https` URL. |

### `target.entra`

| Field | Required | Default | Rule |
|---|---|---|---|
| `tenantId` | Yes | | A GUID. The all-zero GUID is a warning. |
| `clientId` | Yes | | A GUID |
| `cloud` | Yes | `global` | One of `global`, `usgov`, `usgovhigh`, `dod`, `china` |
| `credential.certificate` | One of the two | | A secret reference to a PEM file with the certificate and its private key |
| `credential.clientSecret` | One of the two | | A secret reference. Works, with a warning: a certificate is preferred. |
| `authority` | No | | Tests only. Overrides the token authority. |
| `graphBaseURL` | No | | Tests only. Overrides the Graph endpoint. |

Exactly one of `certificate` and `clientSecret` must be set.

> [!NOTE]
> The controller rejects a config file that sets `authority`,
> `graphBaseURL` or `source.slack.api.baseURL`, unless it was started with
> `TAUT_ALLOW_TEST_ENDPOINTS=1`. These fields exist for the fake services.

### `target.teams[]`

At least one team is required.

| Field | Required | Default | Rule |
|---|---|---|---|
| `name` | Yes | | The Teams display name. At most 256 characters. Unique among teams, ignoring case. |
| `description` | No | | |
| `owner` | Yes | | A user principal name, `user@domain.com` |
| `visibility` | Yes | `private` | `private` or `public` |
| `channels.include` | Yes | `['*']` | Glob patterns on Slack channel names. Must not be empty. |
| `channels.exclude` | No | | Glob patterns. Exclude wins over include. |
| `channels.types` | Yes | `[public]` | `public`, `private` or both |
| `channels.rename` | No | | A map of Slack channel name to Teams channel name. No two entries may rename to the same name. |

Globs follow Go's `path.Match`: `*`, `?` and `[...]` classes. A Slack
channel can go to one team only. Two teams listing the same include
pattern for the same channel kind is a warning here; `taut preflight`
reports the channels that are claimed twice. `taut map` edits this section
for you.

### `target.directMessages`

| Field | Required | Default | Rule |
|---|---|---|---|
| `enabled` | No | `false` | Needs `cloud: global` and `load.strategy: modern` |

See [Direct messages](migration-guide.md#direct-messages) before turning
it on.

### `identity`

| Field | Required | Default | Rule |
|---|---|---|---|
| `overrides` | No | | Path of the override CSV: `slack_user_id,slack_email,entra` |
| `unmapped.policy` | Yes | `former-member` | `former-member`, `service-account` or `block` |
| `unmapped.account` | Unless the policy is `block` | | A user principal name. Ignored, with a warning, when the policy is `block`. |

### `content`

| Field | Required | Default | Rule |
|---|---|---|---|
| `reactions` | Yes | `footer` | `footer`, `native` or `drop`[^native] |
| `customEmoji` | Yes | `text` | `text` or `image`[^emoji] |
| `since` | No | | `YYYY-MM-DD`, inclusive |
| `until` | No | | `YYYY-MM-DD`, inclusive. Must not be before `since`. |

[^native]: `native` is accepted and currently behaves like `footer`.
    Whether imported reactions render in the Teams clients is unverified.

[^emoji]: `image` is accepted and not acted on yet. Custom emoji are
    written as `:name:` text.

### `load`

| Field | Required | Default | Rule |
|---|---|---|---|
| `strategy` | Yes | `modern` | `modern` or `legacy` |

See [Load strategies](how-it-works.md#load-strategies).

### `staging`

| Field | Required | Default | Rule |
|---|---|---|---|
| `endpoint` | Yes | `http://localhost:9000` | An absolute `http` or `https` URL. No credentials, query string or fragment in it. |
| `region` | Yes | `us-east-1` | |
| `bucket` | Yes | `taut-staging` | 3 to 63 lowercase letters, digits, hyphens and dots |
| `accessKey` | Yes | `{ env: TAUT_S3_ACCESS_KEY }` | A secret reference |
| `secretKey` | Yes | `{ env: TAUT_S3_SECRET_KEY }` | A secret reference |
| `encryptionKey` | Yes | `{ env: TAUT_STAGING_KEY }` | A secret reference |

### `controller`

| Field | Required | Default | Rule |
|---|---|---|---|
| `url` | Yes | `http://localhost:7070` | An absolute `http` or `https` URL. No credentials, query string or fragment in it. |

The controller's API token is not in the config file. The CLI reads it
from `TAUT_CONTROLLER_TOKEN`.

## Rules across fields

| Combination | Result |
|---|---|
| `load.strategy: modern` with a national cloud (`usgov`, `usgovhigh`, `dod`, `china`) | Error. Set `load.strategy: legacy`. |
| `load.strategy: legacy` with `private` in a team's `channels.types` | Error. `legacy` supports standard channels only. |
| `directMessages.enabled: true` with a national cloud | Error |
| `directMessages.enabled: true` with `load.strategy: legacy` | Error |
| `api.enabled: true` with no `api.token` | Error |
| Both `certificate` and `clientSecret` | Error |

## What is not in the config file

- **Secrets.** Only references to them.
- **Rate budgets and worker settings.** These belong to the deployment:
  environment variables on the controller, workers and sidecars, set by the
  Helm chart or the Compose file. See
  [cli.md](cli.md#service-commands) and [OPERATIONS.md](OPERATIONS.md).
- **The controller's API token.** `TAUT_CONTROLLER_TOKEN` in the shell
  that runs `taut`.
