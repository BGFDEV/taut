# Design notes

After reading this page you will know why Taut is built the way it is in
the places where an engineer reading the code would stop and ask. Each note
gives the constraint, the choice and what the choice costs.

How the parts fit together is in [how-it-works.md](how-it-works.md).

**Contents**

- [Postgres is the job ledger; NATS is only transport](#postgres-is-the-job-ledger-nats-is-only-transport)
- [Target timestamps as the idempotency key](#target-timestamps-as-the-idempotency-key)
- [Workers hold no credentials](#workers-hold-no-credentials)
- [Client-side envelope encryption for staging](#client-side-envelope-encryption-for-staging)
- [The export reaches staging through the controller](#the-export-reaches-staging-through-the-controller)
- [A hand-written Graph client](#a-hand-written-graph-client)
- [A placeholder account for unmapped authors](#a-placeholder-account-for-unmapped-authors)
- [Reactions as a footer](#reactions-as-a-footer)
- [A Slack app per customer from a manifest](#a-slack-app-per-customer-from-a-manifest)
- [Two load strategies](#two-load-strategies)
- [Finalize is ledger work](#finalize-is-ledger-work)
- [Private channel members join the team](#private-channel-members-join-the-team)
- [Direct messages migrate whole or not at all](#direct-messages-migrate-whole-or-not-at-all)
- [One audit hash chain per lane](#one-audit-hash-chain-per-lane)
- [Telemetry without identifiers or content](#telemetry-without-identifiers-or-content)
- [No terminal background query at startup](#no-terminal-background-query-at-startup)
- [One binary, one image](#one-binary-one-image)
- [RustFS in the local stack](#rustfs-in-the-local-stack)

## Postgres is the job ledger; NATS is only transport

The two hard requirements are strict chronological order within a channel
and resume with no duplicates. Both need progress to be recorded in the
same transaction as the Slack-to-Teams mapping rows. A message broker
cannot give that: its acknowledgement state lives outside the database, so
every crash would need reconciliation between two stores.

So job state lives in Postgres as lanes with leases and fence tokens, and
Taut owns the claim query. NATS carries hints: "there may be work", budget
leases, throttle broadcasts. Workers also poll Postgres every two seconds.
A lost, duplicated or delayed signal delays work and cannot corrupt it, and
the tests run with no NATS at all.

With the ledger authoritative there is nothing for a durable stream to
remember, so Taut uses core NATS and not JetStream.

**Cost:** NATS remains a dependency, justified by fan-out and not by
durability.

## Target timestamps as the idempotency key

Graph's import API has no idempotency key, and an app cannot delete a
message it imported. Graph does refuse a second message with the same
millisecond `createdDateTime` in a channel.

Taut allocates a unique millisecond per message per channel and stores it
before any write. A refused retry is read back and adopted. The full
mechanism is in
[Resuming without duplicates](how-it-works.md#resuming-without-duplicates).

**Cost:** some messages shift a few milliseconds from their Slack time.
Reading a message back by ID relies on an ID format Microsoft's examples
show and do not promise, with a listing as the fallback.

## Workers hold no credentials

Token handling, throttling and redaction must be the same for every worker
and every call, and tokens must never reach logs. Putting them in one
process per pod, the egress proxy, gives one place to enforce and audit.
Worker code has no auth and no rate limiting logic. The token broker is
separate again so that the process holding the long-lived credential makes
no outbound calls except to the sign-in endpoint.

**Cost:** a worker cannot run without its sidecars, including in local
development, so the Compose stack wires them in. The proxy must stream
large bodies without buffering. "No credentials" means no Microsoft or
Slack credentials: a worker still holds the database URL, the staging key
and the bucket keys.

## Client-side envelope encryption for staging

Staged data must be encrypted at rest and removable after finalize.
Server-side encryption differs across S3-compatible stores, and Taut does
not choose the store. Encrypting in the client, with AES-256-GCM in chunks
under a per-run data key wrapped by an operator-supplied key, works the
same on any store. Purging a run is deleting its objects and destroying its
data key.

**Cost:** range reads on ciphertext need chunk-aware handling. Object key
names are not encrypted, so they carry identifiers only.

## The export reaches staging through the controller

Workers need the export ZIP, which sits on the operator's machine. The CLI
could write to the bucket itself, but then every operator laptop would need
the bucket's credentials and the staging key. `taut migrate` streams the
ZIP to the controller, which encrypts it into staging.

**Cost:** the controller is on the upload path, so a very large export is
bounded by its network link. A decrypted copy exists on each worker's
scratch disk while it works, so scratch should be an ephemeral volume.

## A hand-written Graph client

Taut uses about fifteen Graph endpoints. The generated Graph SDK for Go is
very large and slow to compile for that. The client is written over
`net/http`.

Entra sign-in is hand-written too. The client-credentials flow with a
certificate assertion is about 150 lines of standard library, and writing
it lets the authority URL be overridden cleanly for the fake server and for
the national clouds.

The same reasoning applies to SQL. The store's hard parts are a handful of
carefully locked statements: the claim query, the fenced transaction, the
audit append. They are written by hand, with a small embedded migrator.

**Cost:** queries are not type-checked against the schema at compile time.
The integration tests, which run against real Postgres, carry that weight.

## A placeholder account for unmapped authors

Graph requires the author of an imported message to be a user of the
tenant. Deactivated users, external users and bots have no such account. A
display name alone cannot be imported.

Taut attributes those messages to a placeholder account you designate and
shows the original name in the message body. The `service-account` policy
is the same mechanism under a different account, and `block` refuses to
migrate until everyone is mapped.

**Cost:** in Teams those messages all appear to come from one account.
Search by author does not find them.

## Reactions as a footer

Graph accepts reactions on imported messages. Whether the Teams clients
render imported reactions is unverified, so Taut summarises them in a
footer by default: a final line under the message listing each emoji
with its count.
The information is then visible in every client.

**Cost:** it is text, not real reactions. `content.reactions: native` is
reserved for importing them as reactions and currently behaves like
`footer`.

## A Slack app per customer from a manifest

Since 29 May 2025, Slack limits apps that are distributed outside its
Marketplace to one `conversations.history` or `conversations.replies`
request a minute, with 15 objects each. Internal apps, created by a
workspace for itself, keep Tier 3. So `taut auth --slack-manifest` prints a
manifest, and each customer creates their own internal app from it. No
token passes through anyone else's OAuth flow.

The manifest model fits a free, self-hosted tool. Slack's stated position
is that the Marketplace is the only appropriate channel for commercial
distribution, including templated custom apps. Anyone selling a product
built on Taut needs to read Slack's API terms.

**Cost:** one more setup step for the operator.

## Two load strategies

Graph offers two ways into migration mode. `startMigration` puts an
existing channel or chat into migration mode. It covers private channels,
existing teams and chats, and can be re-entered, but exists in the global
cloud only. Creating teams and channels already in migration mode is the
older flow. It works in every cloud, for standard channels and new teams
only.

`modern` is the default because it is one code path for every channel kind
and for re-runs, and because files can be uploaded before migration mode
begins, which sidesteps the undocumented question of whether a channel's
files folder works during it. `legacy` exists for the national clouds.

**Cost:** under `modern` the team is visible to its owner during the
import. Under `legacy` nobody can check the import before finalize, and
Microsoft's import guide no longer documents the flow.

## Finalize is ledger work

Completing migration mode cannot be undone, and it has the same failure
modes as importing: workers die, responses are lost, requests are
throttled. So finalize is done by workers as lanes, under the same leases,
fences and retries. Nothing about it runs in the CLI or in the controller's
request handler.

Two rules came from review. An earlier partial finalize request is part of
every later one, and the controller serializes finalize requests, so two
overlapping partial requests cannot each conclude that a team needs no
members. And the members stage reads who is in the team before it writes,
so existing owners are not demoted and a retry continues.

**Cost:** the controller must stay at one replica.

## Private channel members join the team

Teams only lets members of a team into its private channels. Leaving a
migrated private channel to its owner would lose the access the migration
exists to carry over. So everyone eligible in any migrated channel of a
team becomes a team member, private channels included.

**Cost:** members of a private channel will see the team's standard
channels. `taut preflight` says so. Operators who mind give private
channels a team of their own.

## Direct messages migrate whole or not at all

A direct message is imported into a Teams chat only if every participant
can be put into that chat as themselves. The placeholder account is never
a participant: it would hand whoever holds that account a private
conversation. A guest is left out with the whole conversation, because a
chat would give them more than they had.

A one-on-one chat that two people already use can hold a message on the
millisecond a Slack message needs. Failing the Slack message would lose
it, so the post is moved one millisecond later in the ledger first and
then sent. The timestamp is still the idempotency key; it has changed
value once, durably, before the request that uses it.

Files in direct messages are not moved. Microsoft does not document where
a chat's files should live, and Taut does not guess.

**Cost:** for most workspaces a large share of direct messages do not
migrate, because one participant has left. `taut preflight` counts them by
reason.

## One audit hash chain per lane

A single run-wide hash chain would make every write in the run queue
behind one row lock. Each lane has its own chain. Appends happen inside
the lane's fenced transaction, which already serializes the lane, so the
chain costs nothing extra.

**Cost:** order across lanes is by timestamp, not by the chain.

## Telemetry without identifiers or content

Telemetry leaves the cluster for whatever backend the operator runs.
Metrics therefore have fixed label sets, and nothing carries message text,
names or addresses. The HTTP instrumentation is hand-written because the
stock client instrumentation records full URLs, and here a URL can be a
Slack file link carrying a token or a pre-authorized upload URL. A test
feeds all of those through and asserts none comes out.

**Cost:** only OTLP/gRPC is supported, and spans show a route class where
a full path would sometimes be more convenient.

## No terminal background query at startup

Adaptive light and dark needs the terminal's background color. Asking for
it means writing a query and waiting for a reply, and some terminals never
answer: a scripted pty, `script`, some CI runners and multiplexers. With a
blocking query, every command hung at startup, `--version` included.

Commands never query the terminal synchronously. The starting palette
comes from `TAUT_THEME`, then the `COLORFGBG` hint, then defaults to dark.
Interactive screens ask asynchronously and restyle if the terminal
answers. Status colors are mid-tones that stay legible if the guess is
wrong.

**Cost:** one-shot plain output on a light terminal that does not export
`COLORFGBG` uses the dark palette until you set `TAUT_THEME=light`.

## One binary, one image

The CLI, the controller, the workers and both sidecars are subcommands of
one binary in one distroless image. There is one artifact to build, sign
and scan, and versions cannot drift between components.

**Cost:** the image carries terminal UI code the servers never run, a few
megabytes.

## RustFS in the local stack

The local Compose stack needs an S3-compatible store. MinIO's community
server has been archived, so the stack uses RustFS, an Apache-2.0 store in
a single container. The S3 client is generic, and production uses whatever
S3-compatible store you have.

**Cost:** RustFS is young. It is used locally only, and swapping it is a
change to the Compose file.
