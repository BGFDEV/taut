# Taut: Operating on Kubernetes

After reading this page you will be able to install Taut from the Helm
chart in `deploy/helm/taut`, size it, run a migration through it, and take
it down again.

> [!NOTE]
> A workspace small enough to migrate from one machine does not need any
> of this. [Local mode](migration-guide.md#local-mode) runs the whole
> migration in one `taut` process with nothing to install, at the price of
> the process isolation described in section 1.

It assumes you have followed the first steps of the
[migration guide](migration-guide.md): the Entra app registration exists,
the Slack app is installed, and you have a Slack export and a config file
(`taut.yaml`) that passes `taut preflight`.

> [!WARNING]
> Taut has not been run against a real tenant, and the chart has not been
> applied to a cluster. It is verified by rendering and schema validation.
> Treat the first install as a test and read [Known gaps](#11-known-gaps)
> before you rely on it.

**Contents**

1. [What gets installed](#1-what-gets-installed)
2. [Requirements](#2-requirements)
3. [Secrets](#3-secrets)
4. [Install](#4-install)
5. [Sizing](#5-sizing)
6. [Running a migration](#6-running-a-migration)
7. [During a run](#7-during-a-run)
8. [Finishing](#8-finishing)
9. [NATS authorization](#9-nats-authorization)
10. [Troubleshooting](#10-troubleshooting)
11. [Known gaps](#11-known-gaps)

## 1. What gets installed

| Workload | Pods | Containers | Talks to |
|---|---|---|---|
| controller | 1 | `controller` | Postgres, NATS, the bucket. Operators' CLIs talk to it. |
| worker | N | `worker`, `egress-proxy`, `token-broker` | Postgres, NATS, the bucket; Microsoft and Slack through the egress proxy |

Postgres, NATS and the S3 bucket are yours. The chart installs none of them.

Who can read what, as the chart wires it:

| Container | Postgres URL | Staging key | S3 keys | Controller token | NATS credentials | Entra credential | Slack token |
|---|---|---|---|---|---|---|---|
| controller | file | file | file | file | its own file | no | no |
| worker | file | file | file | no | its own file | no | no |
| egress-proxy | no | no | no | no | its own file | no | no |
| token-broker | no | no | no | no | no | file | file |

"file" means a read-only mounted Secret. By default no secret is passed
through an environment variable (`tokenBroker.credentialEnv` is the one
opt-in exception, section 3). The NATS URL is a plain value and the chart refuses
one with a password in it; each container that uses NATS mounts only its own
credentials file (section 9).

Workers do not call the controller. They find work in Postgres and are woken
over NATS, so the controller can be restarted at any time during a run.

### Why the controller stays at one replica

Finalize requests are serialized inside the controller process, which is what
stops two overlapping partial finalize requests from each concluding that a
team needs no members. The controller also hands out the tenant's
request budget from one in-memory allocator; two controllers would each hand
out the whole budget. The chart's schema rejects `controller.replicas` above
1 and the Deployment uses the `Recreate` strategy so an upgrade never runs
two at once. A controller outage of a few seconds costs nothing: workers
carry on, and `taut migrate` reconnects.

## 2. Requirements

- Kubernetes 1.29 or later. The worker pod uses native sidecars
  (`initContainers` with `restartPolicy: Always`), so the token broker and
  the egress proxy start before the worker and stop after it. On 1.25 to 1.28
  set `legacySidecars: true`; the chart refuses to render otherwise.
- A CNI that enforces NetworkPolicy.
- Postgres with a database and a user that owns it. Taut creates and
  migrates its own schema at startup.
- NATS. Core NATS is enough; JetStream is not used.
- An S3-compatible bucket and an access key pair for it. Taut creates the
  bucket if it does not exist and the key is allowed to.
- The image in a registry your cluster can pull from. Set
  `image.repository` and either `image.tag` or `image.digest`.

## 3. Secrets

The chart never takes a secret as a value by default. Create these Secrets
first and give the chart their names. Replace every `<placeholder>`; do not
paste real values into a file you keep or into shell history you keep.

```bash
kubectl create namespace taut
```

The Entra certificate: one PEM file holding the certificate and its private
key, as `scripts/entra-app.sh` writes it.

```bash
kubectl -n taut create secret generic taut-entra \
  --from-file=certificate.pem=<path-to-taut-app.pem>
```

If you must use a client secret instead (`entra.credential.type: clientSecret`):

```bash
kubectl -n taut create secret generic taut-entra \
  --from-file=client-secret=<path-to-file-holding-the-client-secret>
```

The Slack bot token, if the config file enables `source.slack.api`:

```bash
kubectl -n taut create secret generic taut-slack \
  --from-file=token=<path-to-file-holding-the-xoxb-token>
```

The staging key. Generate it once; it protects everything in the bucket.

```bash
taut dev keygen > staging.key
kubectl -n taut create secret generic taut-staging-key --from-file=key=staging.key
rm staging.key
```

The controller's API token: any long random string.

```bash
openssl rand -hex 32 > controller.token
kubectl -n taut create secret generic taut-controller-token --from-file=token=controller.token
rm controller.token
```

The Postgres URL and the S3 keys:

```bash
kubectl -n taut create secret generic taut-postgres \
  --from-file=url=<path-to-file-holding-postgres://user:password@host:5432/taut?sslmode=require>
kubectl -n taut create secret generic taut-s3 \
  --from-file=access-key=<path-to-file> --from-file=secret-key=<path-to-file>
```

`--from-file` keeps the values out of your shell history. A trailing newline
in a file is harmless; Taut trims it.

Keep a copy of the staging key somewhere safe for as long as any run is
unfinalized. Without it the staged export cannot be read and the run cannot
be resumed.

### What the config file must say

The token broker reads the credentials from fixed paths, and it is the
config file that names them. A config file used with the cluster must
reference exactly these:

```yaml
target:
  entra:
    credential:
      certificate: { file: /run/secrets/taut/entra-certificate.pem }
      # or: clientSecret: { file: /run/secrets/taut/entra-client-secret }
source:
  slack:
    api:
      token: { file: /run/secrets/taut/slack-token }
```

Those paths do not exist on your workstation, so `taut auth` and
`taut preflight`, which run locally, cannot use the same references. Two ways
round it: keep the credentials at the same paths on the workstation, or set
`tokenBroker.credentialEnv: true` and use `{ env: TAUT_ENTRA_CERTIFICATE }`
(or `TAUT_ENTRA_CLIENT_SECRET`) and `{ env: TAUT_SLACK_TOKEN }`, exporting
the same variables locally. Files are the better choice in the cluster
because they follow rotation.

The token broker refuses any tenant, application or cloud other than
`entra.tenantId`, `entra.clientId` and `entra.cloud`, whatever the config
file says, and reads nothing outside `/run/secrets/taut`.

### Rotation

| Secret | How to rotate | Restart needed |
|---|---|---|
| Entra certificate or client secret | Add the new credential to the app registration, update the Secret, remove the old credential once sign-ins succeed | No. The token broker notices the changed file (the kubelet updates it within about a minute) and reloads. With `credentialEnv: true`, restart the workers. |
| Slack token | Update the Secret | No, as above |
| Controller token | Update the Secret, then `kubectl rollout restart deploy/<release>-controller`, and export the new value where `taut` runs | Yes, the controller reads it at startup |
| Postgres password, S3 keys | Update the Secret, then restart the controller and the workers | Yes |
| NATS credentials files, TLS files | Update the Secret, restart the controller and the workers | Yes, they are read when the connection is made |
| Staging key | Cannot be rotated while a run has staging. Finalize or cancel every run and let it purge first, then replace the Secret and restart everything | Yes |

Restarting workers mid-run is safe (section 7).

## 4. Install

A values file for a real tenant:

```yaml
image:
  repository: <registry>/taut
  tag: <version>

entra:
  tenantId: <tenant-id>
  clientId: <client-id>
  cloud: global
  credential:
    type: certificate
    existingSecret: taut-entra
slack:
  token:
    existingSecret: taut-slack

postgres:
  existingSecret: taut-postgres
nats:
  url: nats://nats.nats.svc:4222      # never with a password; credentials in section 9
  credentials:
    existingSecret: taut-nats-creds
s3:
  endpoint: https://s3.example.com     # leave empty for AWS S3
  region: us-east-1
  bucket: <bucket>
  existingSecret: taut-s3
stagingKey:
  existingSecret: taut-staging-key
controllerToken:
  existingSecret: taut-controller-token

worker:
  replicas: 4
  scratch:
    sizeLimit: 50Gi

networkPolicy:
  postgres:
    to: [{ ipBlock: { cidr: <postgres-address>/32 } }]
  nats:
    to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: nats } } }]
```

```bash
helm install taut deploy/helm/taut --namespace taut -f values-prod.yaml
kubectl -n taut rollout status deploy/taut-controller
kubectl -n taut rollout status deploy/taut-worker
```

If a required value is missing, the install stops and names it. The release
notes repeat the paths the config file must use and how to reach the
controller.

The budgets (`budgets.graphRps` 40, `budgets.sharepointRps` 20,
`budgets.slackRps` 1) are the tenant-wide request rates. The chart gives the
same numbers to the controller, which hands them out, and to every egress
proxy, which treats them as a ceiling. Change them in one place, the values
file, and never per pod.

Direct messages (`target.directMessages.enabled`) are imported into Teams
chats, which Microsoft limits apart from channels: one message a second per
chat and 20 a second per app per tenant, against 50 for channels. There is
no separate budget for them. Each egress proxy holds a chat to one message
a second (`TAUT_GRAPH_CHAT_RPS`, through `egressProxy.extraEnv`; it is
lowered when Graph throttles and never raised, unlike a channel's rate)
and lets chat imports use at most
`egressProxy.chatShare` of its share of the Graph budget: 0.4 by default,
which is 16 a second of the default 40 across all proxies, under the limit
with headroom. If you raise `budgets.graphRps`, lower `chatShare` so the
product stays below 20.
`chatShare` must be more than 0 and at most 1; the egress proxy does not
start with anything else.
Direct messages also need three more application permissions, `Chat.Create`,
`Chat.ReadBasic.All` and `Chat.Read.All` (`scripts/entra-app.sh --direct-messages`),
and exist in the global cloud only.

### Network policy

With `networkPolicy.enabled` (the default) every pod of the release is
denied all traffic, then:

- the controller accepts connections only from the peers in
  `networkPolicy.controllerIngressFrom` (none by default) and may connect to
  DNS, Postgres, NATS, the bucket and the telemetry collector;
- the worker pods accept nothing and may connect to the same services plus
  public addresses on port 443.

The kubelet's probes (the controller's and the worker's `/healthz`) need no
rule. They come from the node the pod runs on, and Kubernetes does not let a
NetworkPolicy block traffic between a pod and its own node. The token
broker's and the egress proxy's probes run inside their containers and use
no pod network at all. If your CNI is configured to filter host traffic
(Cilium's host firewall, for example), allow the node to reach port 8081 of
the worker pods and 7070 of the controller.

Say where each dependency is: `networkPolicy.postgres.to`, `.nats.to`,
`.s3.to` and `.otlp.to` take NetworkPolicy peers. A dependency whose `to` is
empty is taken to be a public service: its rule allows public IPv4 addresses
on its port, with the same private, link-local and carrier-grade NAT ranges
left out as for the worker's own internet egress
(`networkPolicy.workerExternalEgress.except`). Two things follow.

- A Postgres, NATS, bucket or collector inside the cluster or on a private
  network is not reachable until its `to` names it. The pods then fail their
  readiness checks with that dependency `unreachable`.
- Until `networkPolicy.s3.to` is set, the controller and the worker pods may
  open connections to any public address on the bucket's port (443 by
  default). For the worker pods that is no more than they already may; for
  the controller it is the only way to the internet it has, so pin it. With
  `networkPolicy.cilium.enabled` an unpinned dependency on 443 also undoes
  the restriction to named hosts, because the rules of both policies add up.

Only DNS is different: with an empty `networkPolicy.dns.to` any resolver may
be asked.

A NetworkPolicy selects pods, not containers. The worker container shares
its pod's network identity with the egress proxy, so no policy can stop the
worker container from opening a connection the egress proxy is allowed to
open. What holds inside the pod is this: the worker container cannot read a
credential or reach the token broker's socket, so anything it sends directly
is unauthenticated; and the egress proxy forwards only to Microsoft Graph,
SharePoint and Slack, with a token only for the host it belongs to.

Plain NetworkPolicy cannot express host names, and neither Microsoft nor
Slack offers a short fixed address list, so the default allows any public
IPv4 address on 443 (private, link-local and carrier-grade NAT ranges are
excluded, which keeps the cloud metadata service out of reach). To restrict
by name you need a CNI that supports it. With Cilium:

```yaml
networkPolicy:
  cilium:
    enabled: true
```

replaces the address rule with a CiliumNetworkPolicy listing the sign-in,
Graph and SharePoint hosts of `entra.cloud` and Slack's hosts. Otherwise put
an egress gateway or a filtering proxy's address in
`networkPolicy.workerExternalEgress.cidrs`.

### Telemetry

```yaml
telemetry:
  endpoint: http://otel-collector.observability.svc:4317
```

The controller, the workers and the egress proxies push traces and metrics
over OTLP/gRPC to that collector. There is nothing to scrape and no
ServiceMonitor. Metrics carry no run, channel or user identifiers and nothing
carries message content ([Telemetry](how-it-works.md#telemetry)). The controller reports
`taut.runs.active`, `taut.run.posts` (by state) and `taut.run.lanes` (by
stage and status); workers report `taut.posts`, `taut.files`,
`taut.batch.duration`, `taut.ledger.transaction.duration`, `taut.batch.retries`
and `taut.lease.lost`.

## 5. Sizing

None of these numbers has been measured on a real tenant. Those marked
"measured" come from the load test against a fake Graph on one machine
([LOADTEST.md](LOADTEST.md)); the rest follow from how the code works.
Adjust after your first rehearsal run.

**How long it takes.** Under Microsoft's limits the duration is the larger
of two times: all messages at the tenant budget (40 a second by default),
and the largest channel at 5 a second, since a channel is imported by one
worker at a time. A workspace of 120,000 messages whose largest channel
holds 24,000 takes 80 minutes at best, not 50, and no number of workers
changes that (measured: 40 a second reached and held with two workers, no
429). Without limits Taut imported 3,000 to 4,300 messages a second on a
laptop, so it is not what you will be waiting for.

**Workers.** A channel is loaded by one worker at a time and Graph limits a
channel to between 1 and 5 messages a second, so throughput comes from
loading many channels at once, up to the tenant budget (40 requests a second
by default). One worker runs `worker.concurrency` lanes (default 4). Enough
workers to cover the budget is roughly `budget / (concurrency x per-channel
rate)`: with the defaults, 4 to 10 pods. More than that adds nothing, since
the egress proxies share one budget. Fewer is fine and only slower. Chats
of direct messages are loaded the same way, one worker per chat, within
their share of that budget.

**Worker memory and CPU.** Transform is CPU work; load and files are mostly
waiting. Start with the defaults (250m, 256Mi request, 1Gi limit) and raise
the memory limit if pods are OOM-killed during transform of very large
channel-days. Measured: a worker peaked at 330 to 380 MB whether the export
held 2,500 messages or a million.

**Scratch.** Each worker pod downloads and decrypts one copy of the export
ZIP for every run it works on, into an `emptyDir` at `/var/lib/taut/scratch`.
Set `worker.scratch.sizeLimit` to the size of your largest export ZIP plus a
fifth, times the number of runs in progress at once (normally one). File
attachments are not kept there; they stream from Slack through staging to
SharePoint. The space comes from the node's ephemeral storage, so nodes need
that much free per worker pod. A pod that exceeds the limit is evicted and
its work resumes on another pod, which will hit the same limit: raise it.
The copy is plaintext; it is deleted when the pod goes away, and within 30
seconds of the run being finalized or cancelled.

**Bucket.** Staging holds the encrypted export, the extracted and transformed
batches and every file on its way to SharePoint. The batches are not
compressed and the export ZIP is, so staging is many times the ZIP.
Measured: 1.5 GB for a million messages (a 120 MB ZIP), and 490 MB for
120,000 messages spread over more day files (a 21 MB ZIP). Allow 2 to 4 KB
per message plus the total size of the files Slack holds. After finalize it
is deleted at a few megabytes a second.

**Postgres.** The ledger keeps one row per message (`map_messages`), one per
file, and one audit entry per write to Teams or SharePoint, so plan for two
to three rows per migrated message. Measured: 1.7 GB per million messages
(about 0.8 KB a message in `map_messages` and 0.7 KB in the audit log), a
little more per message when channels have many quiet days, since every
channel-day is four batch rows. Each message is one transaction with its
own commit, so the disk's commit latency sets Taut's own ceiling; at the
rates Microsoft allows one core is ample. The status and dashboard
endpoints count the run's rows on every request: about 30 ms at 120,000
messages and 150 ms at a million, once a second for every dashboard that
is open. Connections: each worker holds a
pool and the controller another; a small instance with 100 connections is
enough for ten workers. Use a managed instance with point-in-time recovery
(section 8).

**Controller.** It streams the export upload straight into staging and
otherwise serves small requests. The defaults are enough.

## 6. Running a migration

`taut init`, `auth`, `preflight` and `map` run on your workstation and need
nothing from the cluster. `migrate`, `status`, `dashboard`, `finalize` and
`audit` talk to the controller.

Reach the controller with a port-forward:

```bash
kubectl -n taut port-forward svc/taut-controller 7070:7070
```

In the config file:

```yaml
controller:
  url: http://127.0.0.1:7070
```

and in the shell that runs `taut`:

```bash
export TAUT_CONTROLLER_TOKEN="$(kubectl -n taut get secret taut-controller-token -o jsonpath='{.data.token}' | base64 -d)"
```

Then:

```bash
taut migrate
```

`taut migrate` uploads the export through the controller into staging, then
watches. The upload of a large export through a port-forward is slow and a
dropped port-forward interrupts it; run `taut migrate` again and it resumes
or re-sends. Once the upload is done you can stop watching at any time
(`taut migrate --detach` returns immediately); the run continues in the
cluster. `taut status` and `taut dashboard` show where it stands.

For anything but a small export, use an ingress instead of a port-forward:

```yaml
controller:
  ingress:
    enabled: true
    className: nginx
    annotations:
      nginx.ingress.kubernetes.io/proxy-body-size: "0"
      nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
      nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
      nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    hosts:
      - host: taut.example.com
    tls:
      - secretName: taut-tls
        hosts: [taut.example.com]
networkPolicy:
  controllerIngressFrom:
    - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: ingress-nginx } }
```

The ingress must accept a request body as large as the export without
buffering it, and must not cut long-lived event streams. Serve it over TLS
and keep it off the public internet: the API token is the only thing between
a caller and the migration, and one token grants everything.

## 7. During a run

### Scaling workers

```bash
kubectl -n taut scale deploy/taut-worker --replicas=8
```

or change `worker.replicas` and `helm upgrade`. New workers start claiming
lanes within seconds. Scaling down is as safe as scaling up: a worker told
to stop hands its lanes back before it exits.

`worker.autoscaling.enabled` adds a CPU-based HorizontalPodAutoscaler. It is
off by default because load and file transfer are bound by Microsoft's rate
limits, not CPU, so CPU says little about whether more workers would help.

To give different stages their own pool, install the chart a second time
under another release name with `worker.stages` set (for example
`transform`) and `controller.replicas: 0`, pointing at the same Postgres,
NATS, bucket and staging key.

### What happens when pods are killed

Nothing is imported twice and nothing is lost. This is the property the
test suite's kill tests check against the fake Graph.

- **A worker pod is deleted or drained.** It receives SIGTERM, hands its
  lanes back and exits; another worker takes them at once. The grace period
  (`worker.terminationGracePeriodSeconds`, 60) is more than it needs.
- **A worker pod is killed outright** (node loss, OOM, eviction). Its leases
  expire after 30 seconds and another worker resumes each lane from the last
  recorded batch. A message Graph had accepted but the worker had not yet
  recorded is refused by Graph as a duplicate on retry and adopted.
- **The egress proxy or token broker container dies.** The kubelet restarts
  it in place. Requests in flight fail and are retried by the worker.
- **A container hangs without exiting.** Each of the three has a liveness
  probe: the worker's `/healthz` on port 8081, the egress proxy's health
  page on loopback, the token broker's over its socket. After three failed
  checks the kubelet restarts that container. The worker's endpoint says the
  process is serving, not that every lane is moving; a stuck lane shows in
  `taut dashboard`, and its lease expires on its own.
- **The controller pod dies.** Workers continue. `taut migrate` and
  `taut dashboard` lose their connection until it is back; budget leases
  fall back to a small safe rate in the meantime, so the run slows.
- **NATS is down.** Workers find work by polling Postgres every two seconds
  and egress proxies fall back to the safe rate. Slower, still correct.
- **Postgres is down.** Everything waits. Nothing is written to Teams
  without the ledger.

### Upgrading

```bash
helm upgrade taut deploy/helm/taut --namespace taut -f values-prod.yaml
```

The controller is replaced (stop, then start) and the workers roll a quarter
at a time. An upgrade during a run is safe for the same reasons a pod kill
is. Both the controller and the workers apply pending database migrations at
startup. Do not run two versions against one database for longer than the
rollout takes, and take a database backup before upgrading across a release
that changes the schema.

## 8. Finishing

### Finalize and purge

`taut migrate` leaves every channel in migration mode. Check the result in
Teams, then:

```bash
taut finalize
```

It verifies the audit log, shows what it will open and asks. Completing
migration mode opens the channels to their members, which cannot be taken
back. The work itself runs in the cluster as ordinary lanes; closing the
terminal changes nothing.

- `taut finalize --channel <name>` opens some channels and leaves the rest
  importable. A team gets its members once all its channels are open.
- When a run is finalized the controller deletes its staging objects and
  then destroys its data key, and each worker removes its scratch copy
  within 30 seconds. `taut finalize --retain` keeps staging, still encrypted;
  that choice has to be made when finalizing starts and cannot be released
  later except by deleting from the bucket yourself.
- A cancelled run (`taut migrate --new` abandons the current one) has its
  staging deleted too, once no worker still holds a lease on it and none
  of its chats is still in migration mode.
- **Chats of a run that will not finish.** The chat of a direct message is
  in front of its participants, in migration mode, from the moment the
  import touches it. Cancelling a run takes its chats out of migration
  mode (a `release` stage in `taut status`, `chat.release_migration` in
  the audit log), and so does `taut migrate --new` for the run it
  replaces. For a run that failed and that you do not mean to resume, do
  it yourself: `taut finalize --only direct-messages`. It ends migration
  mode on that run's chats, finalizes nothing else, and cannot be undone.
  Workers must be running for it to happen.
- With direct messages on, staging holds the export's direct messages,
  those of conversations that were not migrated too, until it is purged;
  `--retain` keeps them. With direct messages off, `taut migrate` does not
  upload them at all. When staging is purged, the names of the people in
  each migrated conversation are erased from the database as well; the
  audit log keeps the chats' IDs.
- A run that completed but was never finalized keeps its staging until you
  finalize it, follow it with an incremental pass, or delete the objects
  from the bucket.

### Incremental passes

`taut migrate --incremental` imports a newer export of the same workspace
on top of what is there; the operator's steps are in the
[migration guide](migration-guide.md#migrating-in-passes). For the cluster
it is one more run:

- **The schema.** Passes need migration `0005_passes`, which the controller
  and the workers apply at start, as with every release. Upgrade all of
  them before starting a pass. A pass has a ledger layout of its own, so a
  worker from an older release refuses its lanes and logs why instead of
  working them wrongly.
- **Staging.** The new export is uploaded and staged like the first, so
  budget the bucket for it. Once the pass has planned, the earlier pass's
  staging is deleted and its data key destroyed, like a finalized run's,
  unless that run was finalized with `--retain`.
- **Load.** A pass extracts and transforms the whole export again, which is
  CPU and Postgres work, and sends Graph only what is new. Planning copies
  the earlier pass's posted rows into the new run in one transaction: about
  one row per migrated message, so expect the `map_messages` table to grow
  by its own size with every pass. The rows of earlier passes are not
  deleted; they are that pass's record.
- **Reopened channels.** A pass after a finalize puts channels with new
  messages back into migration mode until `taut finalize` is run again.
  Schedule it like a maintenance window.
- **The audit log** is per pass. Export each one:
  `taut audit --pass 1 --export pass1.ndjson`, and so on.
- **Backups.** A pass reads the earlier pass's ledger from Postgres, not
  from Teams. Restoring a backup taken before a pass finished, and then
  running the next pass, would start that pass from an incomplete ledger.
  Resume the restored pass to the end first; it adopts what it had already
  sent.

**Versioned buckets.** On a bucket with versioning enabled, deleting an
object leaves its earlier versions in place, so a purge does not remove the
data. The objects are encrypted and their key is destroyed, but
the wrapped key may still exist in a database backup. Either use an
unversioned bucket for staging or add a lifecycle rule that expires
noncurrent versions and delete markers after a day.

Export the audit log before you take anything down:

```bash
taut audit --export audit.ndjson
```

### Backup and restore of Postgres

After a purge, Postgres is the only record of the migration: the ledger,
the Slack-to-Teams mappings and the hash-chained audit log. Back it up like
any system of record.

- During a run, use point-in-time recovery or frequent snapshots. Taut needs
  nothing special: a consistent backup of the database is enough.
- After finalizing, take a final dump and keep it with the exported audit log
  for as long as your retention rules require:

  ```bash
  pg_dump --format=custom --file=taut-final.dump "<postgres-url>"
  ```

- Restore into an empty database of the same name with `pg_restore`, point
  the `taut-postgres` Secret at it and restart the controller and workers.

Restoring an older backup **during** a run winds the ledger back while
Teams keeps what was already posted. That is safe for messages: on resume,
Graph refuses each already-posted message as a duplicate and Taut adopts
it. It is slower, and the audit entries for the re-adopted writes are
written again from the restore point. Do not restore a backup taken before
a finalize into a tenant where the finalize has since happened; the ledger
would then describe channels as importable that no longer are.

A database backup taken before a purge still contains that run's wrapped
data key. With the staging key and leftover object versions it could decrypt
them. Expire old backups, or accept that and say so in your records.

### Uninstall

```bash
helm uninstall taut --namespace taut
```

This removes the Deployments, the Service, the policies and the service
accounts. It leaves:

- the Secrets you created (and their contents);
- the Postgres database, with the ledger, mappings and audit log;
- the bucket, with the staging of any run that was not finalized or cancelled;
- NATS, unchanged (Taut stores nothing in it);
- everything in Microsoft 365, including the Entra app registration.

When the migration is over: delete the Secrets, remove or archive the
database, empty the bucket, and remove the app registration or at least its
credential. `Teamwork.Migrate.All` lets its holder post as anyone with any
timestamp.

## 9. NATS authorization

Taut uses three subjects:

| Subject | Publishes | Subscribes |
|---|---|---|
| `taut.wake` | controller, workers | controller, workers, egress proxies |
| `taut.throttle` | egress proxies | egress proxies, controller |
| `taut.budget.lease` | egress proxies (as requests) | controller (queue group `taut`), which replies |

Messages on the bus are not authenticated by Taut. What a forged message can
do is bounded (a lease is clamped to the proxy's own ceiling, a throttle
pauses for at most 30 seconds), but the real control is NATS subject
permissions. **The chart does not configure the NATS server: it
is yours.** What the chart does is give each kind of process its own NATS
identity, as a credentials file that only that container mounts. Without
permissions on the server, anything that can connect to NATS can slow the
migration or answer lease requests.

The permissions each identity needs:

| Identity | May publish | May subscribe | Replies |
|---|---|---|---|
| `taut-controller` | `taut.wake` | `taut.wake`, `taut.throttle`, `taut.budget.lease` | may answer requests it received |
| `taut-worker` | `taut.wake` | `taut.wake` | no |
| `taut-egress-proxy` | `taut.throttle`, `taut.budget.lease` | `taut.wake`, `taut.throttle`, `_INBOX.>` | no |

Only `taut-controller` may subscribe to `taut.budget.lease`, so only the
controller can answer a lease. Workers cannot publish throttles or request
leases. The egress proxy needs `_INBOX.>` to receive the lease replies.

### With credentials files (operator and account JWTs)

A credentials file holds a user's JWT and its key, and the permissions are
part of the JWT. With `nsc`, in the account your NATS server serves:

```bash
nsc add user --name taut-controller \
  --allow-pub "taut.wake" \
  --allow-sub "taut.wake,taut.throttle,taut.budget.lease" \
  --allow-pub-response

nsc add user --name taut-worker \
  --allow-pub "taut.wake" \
  --allow-sub "taut.wake"

nsc add user --name taut-egress-proxy \
  --allow-pub "taut.throttle,taut.budget.lease" \
  --allow-sub "taut.wake,taut.throttle,_INBOX.>"

nsc generate creds --name taut-controller   > controller.creds
nsc generate creds --name taut-worker       > worker.creds
nsc generate creds --name taut-egress-proxy > egress-proxy.creds
```

Put the three files in one Secret, under the key names the chart expects,
and delete the local copies:

```bash
kubectl -n taut create secret generic taut-nats-creds \
  --from-file=controller.creds --from-file=worker.creds --from-file=egress-proxy.creds
rm controller.creds worker.creds egress-proxy.creds
```

```yaml
nats:
  url: nats://nats.nats.svc:4222
  credentials:
    existingSecret: taut-nats-creds
    # controllerKey: controller.creds
    # workerKey: worker.creds
    # egressProxyKey: egress-proxy.creds
```

The chart mounts `controller.creds` into the controller only, `worker.creds`
into the worker container only and `egress-proxy.creds` into the egress
proxy only, each as `/run/secrets/taut-nats/nats.creds`, and sets
`TAUT_NATS_CREDS_FILE`. The worker container and the egress proxy share a
pod but not a volume, so a worker cannot use the egress proxy's identity.
The token broker does not use NATS and mounts none of this.

### The same permissions in a server configuration file

If your server uses a static `authorization` block instead of JWTs, the
equivalent is below. Taut does not accept a password in `nats.url`, so the
users are identified by client certificate: issue one certificate per
identity and have the server map the certificate's subject to the user
(`tls { verify_and_map: true }`).

```text
authorization {
  users = [
    {
      user: "CN=taut-controller"
      permissions: {
        publish:   { allow: ["taut.wake"] }
        subscribe: { allow: ["taut.wake", "taut.throttle", "taut.budget.lease"] }
        allow_responses: true
      }
    }
    {
      user: "CN=taut-worker"
      permissions: {
        publish:   { allow: ["taut.wake"] }
        subscribe: { allow: ["taut.wake"] }
      }
    }
    {
      user: "CN=taut-egress-proxy"
      permissions: {
        publish:   { allow: ["taut.throttle", "taut.budget.lease"] }
        subscribe: { allow: ["taut.wake", "taut.throttle", "_INBOX.>"] }
      }
    }
  ]
}
```

The chart takes one client certificate (`nats.tls.certKey`, `nats.tls.keyKey`)
and mounts it into every container that uses NATS, so through the chart all
three would be the same user. Separate identities by certificate therefore
need credentials files as above, or one release per identity. Use the
credentials files.

### TLS

```bash
kubectl -n taut create secret generic taut-nats-tls --from-file=ca.crt=<path-to-ca-bundle.pem>
```

```yaml
nats:
  url: tls://nats.nats.svc:4222
  tls:
    existingSecret: taut-nats-tls
    caKey: ca.crt
    # certKey: tls.crt   # only if the server verifies client certificates
    # keyKey: tls.key
```

With no `nats.tls.existingSecret`, a `tls://` URL is verified against the
system trust store.

Both the permission table and the commands were checked against the
subjects in the code (`internal/bus`, `internal/sidecar/wire`,
`internal/sidecar/budget`), not against a running NATS server. Test them in
your rehearsal: a missing permission shows as "budgets not leased" on the
dashboard or as workers that only notice new work every two seconds.

## 10. Troubleshooting

Logs are structured JSON on stdout. Start with:

```bash
kubectl -n taut logs deploy/taut-controller
kubectl -n taut logs deploy/taut-worker -c worker
kubectl -n taut logs deploy/taut-worker -c egress-proxy
kubectl -n taut logs deploy/taut-worker -c token-broker
```

| Symptom | Likely cause and what to do |
|---|---|
| `helm install` fails with "... is required" | A required value is missing; the message names it. |
| `helm install` fails with "need 1.29 or later" | Old cluster. Upgrade it or set `legacySidecars: true`. |
| Controller pod never becomes Ready | `kubectl -n taut port-forward deploy/taut-controller 7070` then `curl 127.0.0.1:7070/readyz` lists each dependency. `unreachable` for Postgres, NATS or staging usually means the NetworkPolicy port or `to` list does not match the real service (`networkPolicy.postgres.ports`, `networkPolicy.nats.ports`, `networkPolicy.s3.ports`), or that a service inside the cluster or on a private address has no `to` at all: an empty `to` allows public addresses only. |
| Controller exits with `service.config_missing` or `controller.no_token` | A Secret is missing a key the chart expects. Compare the key names in section 3 with `kubectl -n taut describe secret <name>`. |
| Pods stuck in `ContainerCreating` with `secret "..." not found` or `references non-existent secret key` | The Secret or key named in the values does not exist in the release's namespace. |
| Worker pod stuck in `Init` | The pod starts the token broker, then the egress proxy, then the worker, each once the one before answers its health check. Read the log of the first container that is not ready. A token broker that never becomes ready usually cannot create its socket directory. |
| `bus.unreachable`, or NATS logs authorization violations | The credentials file does not match the server, or a permission from section 9 is missing. Check that the Secret's keys are `controller.creds`, `worker.creds` and `egress-proxy.creds`. |
| Token broker logs `token.socket_dir_insecure` | `podSecurityContext` was changed so the containers no longer share one user. The token broker and the egress proxy must run as the same user. |
| Lanes fail with `token.identity_not_allowed` | The config file's tenant, client or cloud differs from `entra.tenantId`, `entra.clientId`, `entra.cloud`. Correct whichever is wrong. |
| Lanes fail with `token.ref_not_allowed` or `token.secret_missing` | The config file references a credential somewhere other than the paths in section 3, or the Slack Secret was not configured (`slack.token.existingSecret`). |
| Lanes fail with `token.broker_unreachable` | The token broker container is not running; read its log. |
| Requests refused by the egress proxy as a destination that is not allowed | The tenant's SharePoint host is not among the built-in ones. Add it to `egressProxy.extraAllow` (for example `sharepoint=*.sharepoint-mil.us`), and to `networkPolicy.cilium.extraFQDNs` if you use that. |
| Everything is slow; the dashboard shows throttling | Microsoft or Slack is rate limiting. That is expected in bursts. If it is constant, lower `budgets.graphRps`. More workers will not help. |
| Budgets show as not leased | The egress proxies cannot reach the controller over NATS: the controller is down, or NATS permissions block `taut.budget.lease` or `_INBOX.>`. |
| Worker pods evicted for ephemeral storage | `worker.scratch.sizeLimit` is smaller than the export. Raise it. |
| `taut migrate` cannot connect | The port-forward dropped, or `networkPolicy.controllerIngressFrom` does not include where you are connecting from. |
| `taut migrate` gets 401 | `TAUT_CONTROLLER_TOKEN` in your shell differs from the Secret, or the controller was not restarted after the Secret changed. |
| Upload of the export fails through the ingress | The ingress limits or buffers request bodies. See the annotations in section 6. |

## 11. Known gaps

- **Not run on a real cluster or tenant.** The chart is linted, rendered
  under every supported value set, validated against the Kubernetes 1.31
  schemas and checked by `deploy/helm/taut/ci/assert.py`. Nothing has been
  applied to a cluster.
- **NATS permissions are documented, not configured** (section 9). The
  chart supplies a separate identity per container; the server must enforce
  what each may do. The client certificate, if used, is shared by all three.
- **Probes that run the binary.** The token broker's and the egress proxy's
  probes start the `taut` binary inside the container every 30 seconds,
  because neither listens where the kubelet can reach it.
- **S3 access is by static key only.** There is no support for workload
  identity (IRSA and the like).
- **The egress proxy's built-in destination list cannot be narrowed** to one
  Microsoft cloud; `egressProxy.extraAllow` only adds. The token broker is
  pinned to one cloud, so a token for another cloud cannot be obtained.
- **One controller token grants everything.** Limit who can reach the API.
- **A worker still holds the staging key, the database URL and the S3
  keys.** "No credentials" means no Microsoft or Slack credentials.
