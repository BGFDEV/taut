# Taut: Load test

`make load-test` answers three questions with one command and a written
report: how fast Taut goes, what limits it, and whether it stays correct at
size. It runs against the local Compose stack and a fake Microsoft Graph, so
read [What this cannot tell](#what-this-cannot-tell) before quoting a number
from it.

It keeps two questions apart that are easily confused:

1. **Taut's own ceiling.** With Graph not limiting anything, where is the
   bottleneck: the ledger, staging, the transform, the egress proxy?
2. **Under Microsoft's documented limits**, does Taut reach what those
   limits allow, so that Microsoft is the limit and not Taut?

## Running it

```bash
make load-test                       # ceiling, 2 workers, about 7 minutes
make load-test PROFILE=throttled     # documented limits, 5 minutes of importing
make load-test PROFILE=scaling       # ceiling at 2, 4 and 8 workers, side by side
make load-test PROFILE=smoke         # a small ceiling run, under a minute
make load-test WORKERS=8 SPEC=channels=300,messages=1000000 LATENCY=0s JITTER=0s
```

Variables: `PROFILE`, `WORKERS` (2, 4 or 8), `SPEC` (the export, below),
`DMS` (add this many one-on-one conversations, and a group for every five),
`MINUTES` (the throttled time box), `LATENCY` and `JITTER` (what the fake
adds to every Graph request), `OUT` (where the JSON report goes).
`scripts/load-test.sh --help` lists the flags.

Each run starts a **fresh stack and removes it afterwards, volumes
included**: do not run it beside a local stack you want to keep. It needs
docker with compose and python3. The generated export lives in a temporary
directory and is deleted; the report is printed and also written as JSON
under `testdata/loadtest/` (git-ignored), so that runs can be compared.

The load test is not part of `make test` or CI. What is, so that it cannot
rot: the generator's tests, a small load fixture through the whole pipeline
in-process (`TestLoadFixtureSmoke`), and the script's own arithmetic
(`scripts/loadtest.py --self-test`, run by `TestLoadTestScript`).

### The export

The export is generated to order, deterministically, and written as a
stream, so a million messages do not sit in memory (about 5 seconds and
25 MB of heap for a million):

```bash
taut dev fixtures --load channels=300,messages=1000000 --out /tmp/load
```

writes `load.zip` and `load.facts.json`, the exact counts of what it holds.
Keys, over the defaults: `channels` (150), `messages` (200,000; the load
test's profiles use 120,000), `users` (400), `days` (365), `threads` (0.2
of messages are replies), `files` (0.01 carry a file), `private` (0.1 of
channels), `skew` (1.05), `dms`, `dmmessages`, `seed`.

Its shape is that of a workspace, not a uniform one. Channel sizes fall off
as 1/rank^skew: with the defaults the largest of 150 channels holds a fifth
of all messages and the median channel under 1%. Days are busy on weekdays
and nearly empty at weekends. One user in twenty is deactivated, one in
fifty a bot, a few have no Microsoft 365 account.

### The profiles

| Profile | Fake Graph | Taut's pacing | Ends |
|---|---|---|---|
| `ceiling` | no limits; 5 ms + up to 5 ms latency | out of the way (budgets and per-channel rates far above what the stack can do) | when the import completes; then finalizes |
| `throttled` | 5/s per channel, 1/s per chat, 50/s and 20/s per app and tenant, answered with 429 and Retry-After; same latency | the product's defaults: 40/s Graph budget, a channel from 1/s up to 5/s, a chat at 1/s | after `MINUTES` of importing; the run is cancelled |
| `scaling` | as `ceiling` | as `ceiling` | three runs: 2, 4, 8 workers |
| `smoke` | as `ceiling`, 1 ms latency, 2,500 messages | as `ceiling` | under a minute |

A worker works four lanes at once, and a channel is imported by one lane at
a time, message after message. So "8 workers" is 32 channels at once.

### What a run verifies

Every run ends by comparing the ledger with the fake tenant
(`GET /_fake/summary`, which counts each conversation without carrying its
messages): each conversation holds exactly the posts the ledger records for
it, with one message per millisecond, and for a complete run that is every
message of the export with none failed. A time-boxed run is cancelled in
mid-flight: a message Graph accepted whose record the worker had not yet
committed is in the tenant and not in the ledger, at most one per
conversation and never the other way round. That is the case a later run
adopts instead of importing again. A mismatch makes the command fail.

### What a run measures, and from where

- **Messages a second**: overall (posts over the load phase), while every
  lane was busy (at least nine lanes in ten had a channel), and the best 30
  seconds. From the controller's status, polled every second.
- **Wall time per stage**, from the same polls (one-second resolution).
- **Graph call latency as the worker sees it and as the egress proxy sees
  the upstream** (`http.client.request.duration`), **ledger transaction
  time** (`taut.ledger.transaction.duration`), **429s** and **time the
  egress proxies held imports back** (the difference of the two HTTP
  durations): from the OpenTelemetry metrics the product emits, read off
  the collector's Prometheus page.
- **Controller API latency under load**: the status and dashboard
  endpoints, each requested once a second throughout.
- **CPU and peak memory per container** (`docker stats`), **Postgres size
  and largest tables**, **the statements Postgres spent most time on**
  (`pg_stat_statements`, loaded by the load test's Compose overlay only),
  **staging bytes**.

One metric was added for this, because nothing else showed it:
`taut.ledger.transaction.duration`, the time of one fenced ledger
transaction, by stage.

## Results

One machine, 2026-11-15: a laptop with 14 CPU cores and 24 GB of memory;
Docker was given all 14 cores and 7.7 GB. Other, unrelated containers were
running on it. Everything (Postgres, NATS, the object store, the fake
Graph, the controller and the workers with their sidecars) shared that one
machine and its one disk.

### Ceiling: 120,000 messages, 150 channels (largest 23,896), 5–10 ms Graph latency

| Workers | 2 | 4 | 8 |
|---|---|---|---|
| Import, wall time | 4 min 54 s | 5 min 2 s | 5 min 3 s |
| Messages/s overall | 487 | 469 | 466 |
| Messages/s while every lane was busy | 812 | 1,555 | 2,680 |
| Load stage | 4 min 7 s | 4 min 15 s | 4 min 18 s |
| Extract / transform stage | 23 s / 20 s | 23 s / 21 s | 23 s / 21 s |
| Finalize (150 channels, members added) | 13.4 s | 13.6 s | 13.6 s |
| Graph import, p50 / p95 / p99 (worker) | 8.3 / 21.9 / 24.4 ms | 8.3 / 21.9 / 24.4 ms | 8.4 / 22.2 / 24.5 ms |
| Ledger transaction, p50 / p99 | 0.74 / 3.3 ms | 0.89 / 7.0 ms | 1.3 / 8.4 ms |
| Controller status, p50 / p95 | 32 / 50 ms | 36 / 55 ms | 36 / 76 ms |
| Postgres CPU, cores, mean / peak | 0.52 / 1.5 | 0.62 / 2.1 | 0.73 / 3.7 |
| Workers CPU, all, cores, mean / peak | 0.47 / 3.5 | 0.53 / 4.3 | 0.65 / 4.9 |
| Peak memory: one worker / Postgres / object store | 328 / 279 / 475 MB | 370 / 278 / 422 MB | 353 / 357 / 455 MB |
| Postgres size / staging | 277 MB / 492 MB | 280 MB / 492 MB | 286 MB / 492 MB |
| Correct | yes | yes | yes |

**What limited it.** Not Taut's shared parts: while every lane had a
channel the rate grew almost in step with the workers (812, 1,555, 2,680 a
second; 96% and 82% of linear), with Postgres under one core on average.
The wall time did not move, because a channel is imported by one lane, one
message after another: about 100 messages a second per lane here (8.3 ms
of Graph latency plus about 1.7 ms of Taut's own: 0.7 ms of ledger
transaction, 0.3 ms in the egress proxy, the rest in the worker). The
largest channel alone, 23,896 messages, therefore takes four minutes
whatever the number of workers. More workers finish the long tail sooner
and then wait for the large channels.

Further runs, to find Taut's own ceiling by taking the fake's latency away
(not part of the default profiles):

| Run | Messages/s | Notes |
|---|---|---|
| 8 workers, same export, no added latency | 2,251 overall, 3,093 best 30 s | import in 100 s; ledger transaction p50 2.5 ms |
| 8 workers, 240,000 messages in 160 equal channels (`skew=0`), no added latency | 3,060 overall, **4,318 while all 32 lanes were busy** | Postgres 1.3 cores mean, 3.9 peak; ledger transaction p50 2.9 ms, p99 14 ms |
| 4 workers, **1,000,000 messages** in 300 channels (largest 180,301), no added latency | 1,938 overall, 2,935 while all 16 lanes were busy | complete and correct in 12 min 2 s; Postgres 2.1 cores mean, 5.0 peak, 1.7 GB; staging 1.5 GB; one worker 381 MB at most; fake Graph 1.0 GB |
| 2 workers, 10,000 messages in 20 channels and 18,000 in 120 direct conversations | 1,301 overall | complete and correct; a check that chats go through the same path, not a benchmark |

**Taut's ceiling on this machine is about 3,000 to 4,300 messages a
second**, and what sets it is the ledger: every message is recorded in its
own transaction with its own commit, which is what makes the import
exactly-once. With 8 lanes that transaction takes 0.7 ms; with 16 to 32
lanes and nothing else slowing them it takes 2.5 to 3.3 ms and Postgres is
the busiest container. That is by design and is about a hundred times what
Microsoft allows a tenant.

### Throttled: the same export, documented limits, 2 workers, 5 minutes of importing

| | |
|---|---|
| 429 responses from Graph | **0** |
| Imported in 5 minutes | 8,742 messages into 8 channels |
| Messages/s | 29 overall, 34 while every lane was busy, **40 at best over 30 s** |
| Graph import as the worker sees it, p50 / p95 / p99 | 190 ms / 575 ms / 1.5 s |
| Graph import upstream of the egress proxy, p50 / p95 / p99 | 8.8 / 22.8 / 24.6 ms |
| Time the egress proxies held imports back | 35 min 45 s in all, 245 ms a message |
| Leases lost | 8 (the cancellation at the end of the time box) |
| Correct | yes: ledger 8,742, tenant 8,750 (8 accepted when the run was cancelled, one per lane, not yet recorded) |

The bound, as the report states it:

- conversations imported at once = min(workers × lanes, conversations) = 8
- Microsoft allows min(50/s per tenant, 8 × 5/s per channel) = 40/s =
  12,026 messages in the time box
- Taut allows itself min(40/s budget, 8 × 5/s) = 40/s = the same
- a channel starts at 1/s and gains 0.5/s for every 30 messages (the
  egress proxy's slow start; 111 s and 240 messages to reach 5/s): 1,195
  messages per lane in the time box, 9,557 in all

**Result: Microsoft is the limit, and Taut's own caution at the start of
each channel.** Taut reached 40 messages a second, which is both its budget
and what Microsoft allows eight channels, and stayed there without a single
429. Over the five minutes it imported 91% of what its slow start allows
and 73% of the flat bound; the rest of the shortfall is the first two
minutes, when some lanes were still moving files.

What this means for a whole import under the limits follows from the
export's shape, and the report prints it. For this export, at best:

| Workers | Time | Messages/s | Limited by |
|---|---|---|---|
| 2, 4 or 8 | 80 min 40 s | 24.8 | the largest channel, which one lane imports at 5 a second |

The tenant budget would allow 50 minutes (120,000 at 40/s) and eight lanes
69 minutes; the largest channel needs 81 on its own. **Under real limits
the duration of a migration is the larger of `messages / 40 s` and
`largest channel / 5 s`**, and adding workers beyond the handful that
covers the budget changes neither.

## Findings

### Bugs

- **HTTP duration histograms could not show a percentile** (fixed,
  2413a34). `http.client.request.duration` and
  `http.server.request.duration` are recorded in seconds but had the SDK's
  default bucket boundaries, which are meant for milliseconds (0, 5, 10,
  25 …). Every request fell into the first bucket: the first load test
  reported a p50 of 2.5 s for requests that took 8 ms. They now have
  boundaries in seconds from 1 ms to 60 s.

No run produced a wrong result, a crash, a failed lane, a lost lease
outside the cancellation, or memory that grew with the size of the export
in a worker, an egress proxy or the controller (a worker peaked at 330 to
380 MB from 2,500 messages to a million).

### Performance, recorded and not tuned

- **Watching a run costs more the larger it is.** The status and dashboard
  endpoints recount the run on every request: all of its batches by stage
  and all of its messages by channel and state. At 120,000 messages that is
  about 30 ms a request; at a million (1,500 lanes, 243,000 batches) the
  status endpoint took 150 ms at the median and 0.5 s at p99, and those two
  statements were where Postgres spent most of its time in the whole run
  (3 min 30 s and 75 s, for 1,753 requests, against 2 min 4 s for writing a
  million audit entries). The cost is linear in the size of the run and is
  paid once a second by every dashboard and `taut migrate` that is open.
  At ten million messages a poll would take seconds. A fix means keeping
  counters in the ledger rather than counting rows, which is a design
  change and was not made.
- **Extract and transform do not get faster with more workers** on this
  stack: about 1,000 day files a second with 8 lanes and with 32. The
  ledger transaction for those stages stays under 4 ms, so the shared limit
  is elsewhere, most likely the single object store that every batch is
  written to. Staging calls are not instrumented, so this is an inference.
  The two stages are a small part of a run (45 seconds of five minutes; 3
  minutes of twelve at a million).
- **Finalize adds members one at a time per team**: 12 seconds for one
  team of 360 members and 15 private channels at 8 ms of Graph latency,
  the same with 2 or 8 workers.
- **Staging is deleted slowly after finalize**: 47 seconds for 492 MB,
  almost 3 minutes for 953 MB, and not yet done 3 minutes after finalizing
  the million-message run (1.5 GB), when the test stopped looking.
- **The files stage has a batch for every channel-day**, with or without a
  file in it: 25,138 batches for 1,169 files. They are cheap (under 4 ms)
  but they are a quarter of all batches and of the ledger's batch rows.
- **The slow start costs small channels most.** A channel of fewer than
  240 messages never reaches 5 a second. For an export whose messages are
  mostly in small channels, lanes and not the budget set the pace, and more
  workers do help.

## What this cannot tell

- **The fake is not Graph.** Its latency is a number chosen here, its
  limits are the documented ones enforced exactly, and it never has a bad
  minute. Real Graph is slower per request (so the per-channel pace is
  lower than the ceiling here, though still far above 5 a second), may
  throttle below what it documents, and returns errors the fake does not.
  The throttled profile shows that Taut does not trip the documented
  limits; it cannot show what the real service does at those rates.
- **A laptop is not a cluster.** Everything shared one machine and one
  disk. Postgres commit times, the object store's throughput and the
  network between pods will all differ, in both directions. The absolute
  ceiling (messages a second) is a property of this machine; the shape
  (what scales with workers and what does not, what grows with the size of
  the run) is what carries over.
- **File content is small.** Files are 2 to 64 KB; a workspace with
  gigabytes of attachments is limited by SharePoint's rates and by
  bandwidth, which this does not exercise.
- **Kubernetes is not involved**: no pod restarts, no network policy, one
  egress proxy per worker on a shared Docker network.
- **The numbers above are single runs**, not averages.
