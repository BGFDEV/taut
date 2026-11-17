#!/usr/bin/env python3
"""Load test for Taut against the local Compose stack.

Run it through scripts/load-test.sh (or `make load-test`), which builds the
binary and the image first. See docs/LOADTEST.md for what the profiles mean
and how to read the report.

It answers two questions, and keeps them apart:

  ceiling    How fast does Taut go when Microsoft Graph does not limit it,
             and what is in the way: the ledger, staging, the transform,
             the egress proxy? The fake Graph answers with a modest latency
             and enforces no limits.
  throttled  Under the limits Microsoft documents, does Taut reach what
             those limits allow? The fake enforces them with 429 and
             Retry-After; the run is time-boxed and compared with a bound
             computed from the fixture's shape.
  scaling    The ceiling profile at 2, 4 and 8 workers, side by side.
  smoke      A ceiling run small enough to finish in under a minute.

Every run ends by checking that the import is correct: each conversation
holds in the fake tenant exactly the posts the ledger records for it, one
per millisecond, and for a complete run that is every message of the export.

Standard library only. Needs docker (with compose) and bin/taut.
"""

import argparse
import json
import math
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TAUT = os.path.join(ROOT, "bin", "taut")
COMPOSE_DIR = os.path.join(ROOT, "deploy", "compose")
FAKE = "http://127.0.0.1:8080"
CONTROLLER = "http://127.0.0.1:7070"
METRICS = "http://127.0.0.1:9464/metrics"

# What the egress proxy does with a conversation's rate (internal/sidecar/
# egressproxy): it starts at 1 a second and gains 0.5 for every 30 messages
# that go through, up to 5 for a channel. A chat stays at 1.
CHANNEL_START_RPS, CHANNEL_MAX_RPS, CHANNEL_STEP_RPS, CHANNEL_STEP_EVERY = 1.0, 5.0, 0.5, 30
CHAT_RPS = 1.0
# Taut's default Graph budget for a tenant, below the 50 a second Microsoft
# documents for imports into channels, and the share of it chats may use.
TAUT_GRAPH_BUDGET, TAUT_CHAT_SHARE = 40.0, 0.5
DOCUMENTED = {"channel": 5.0, "chat": 1.0, "tenantChannels": 50.0, "tenantChats": 20.0}

PROFILES = {
    # spec: the load fixture. latency/jitter: what the fake adds to every
    # Graph request. limits: "none" or "documented".
    "ceiling": {"spec": "channels=150,messages=120000,users=400", "latency": "5ms", "jitter": "5ms", "limits": "none"},
    "throttled": {"spec": "channels=150,messages=120000,users=400", "latency": "5ms", "jitter": "5ms", "limits": "documented"},
    "smoke": {"spec": "channels=10,messages=2500,users=40,days=30", "latency": "1ms", "jitter": "1ms", "limits": "none"},
}


# --- small helpers -----------------------------------------------------------


def comma(n):
    return f"{int(round(n)):,}"


def human_bytes(n):
    n = float(n)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024


def human_seconds(s):
    if s is None:
        return "-"
    if s == 0:
        return "0"
    if s < 0.001:
        return f"{s * 1e6:.0f} µs"
    if s < 1:
        return f"{s * 1000:.1f} ms"
    if s < 120:
        return f"{s:.1f} s"
    return f"{int(s // 60)} min {int(s % 60)} s"


def quantile(values, q):
    """The q-quantile of a list of numbers (nearest rank)."""
    if not values:
        return None
    v = sorted(values)
    return v[min(len(v) - 1, max(0, math.ceil(q * len(v)) - 1))]


def parse_duration(text):
    """Seconds in a Go-style duration such as 5ms, 1.5s or 0s."""
    m = re.fullmatch(r"\s*([0-9.]+)\s*(us|µs|ms|s|m)?\s*", text)
    if not m:
        raise ValueError(f"not a duration: {text!r}")
    return float(m.group(1)) * {"us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1.0, "m": 60.0, None: 1.0}[m.group(2)]


# --- Prometheus text ---------------------------------------------------------

_SAMPLE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)\s*$")
_LABEL = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')


def parse_prometheus(text):
    """[(name, labels, value)] for every sample of a Prometheus text page."""
    out = []
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        m = _SAMPLE.match(line)
        if not m:
            continue
        try:
            value = float(m.group(3))
        except ValueError:
            continue
        out.append((m.group(1), dict(_LABEL.findall(m.group(2) or "")), value))
    return out


class Histogram:
    """One histogram, summed over every series that matched."""

    def __init__(self):
        self.buckets = {}  # upper bound → cumulative count
        self.count = 0.0
        self.sum = 0.0

    def quantile(self, q):
        """As Prometheus's histogram_quantile: linear within the bucket."""
        if self.count <= 0 or not self.buckets:
            return None
        rank = q * self.count
        lower, below = 0.0, 0.0
        for upper in sorted(self.buckets):
            cum = self.buckets[upper]
            if cum >= rank:
                if math.isinf(upper):
                    return lower  # beyond the last boundary: all that is known
                inside = cum - below
                return upper if inside <= 0 else lower + (upper - lower) * (rank - below) / inside
            lower, below = upper, cum
        return lower

    def mean(self):
        return self.sum / self.count if self.count else None

    def summary(self):
        return {"count": int(self.count), "mean": self.mean(), "p50": self.quantile(0.5), "p95": self.quantile(0.95),
                "p99": self.quantile(0.99), "sum": self.sum}


def histogram(samples, name, **want):
    """Sum the histogram `name` over the series whose labels equal `want`."""
    h = Histogram()
    for sample, labels, value in samples:
        if not sample.startswith(name + "_"):
            continue
        if any(labels.get(k) != v for k, v in want.items()):
            continue
        kind = sample[len(name) + 1:]
        if kind == "bucket":
            le = labels.get("le", "")
            upper = math.inf if le == "+Inf" else float(le)
            h.buckets[upper] = h.buckets.get(upper, 0.0) + value
        elif kind == "count":
            h.count += value
        elif kind == "sum":
            h.sum += value
    return h


def counter(samples, name, **want):
    return sum(v for s, labels, v in samples if s == name and all(labels.get(k) == x for k, x in want.items()))


# --- the theoretical bound ---------------------------------------------------


def ramp(seconds, start=CHANNEL_START_RPS, top=CHANNEL_MAX_RPS, step=CHANNEL_STEP_RPS, every=CHANNEL_STEP_EVERY):
    """Messages one conversation can take in `seconds` at the egress proxy's
    pace: `start` a second, `step` more after every `every` messages, up to
    `top`."""
    sent, rate, left = 0.0, start, float(seconds)
    while left > 0 and rate < top:
        need = every / rate  # seconds for the next `every` messages
        if need >= left:
            return sent + rate * left
        sent, left, rate = sent + every, left - need, min(top, rate + step)
    return sent + rate * max(left, 0.0)


def ramp_seconds(messages, start=CHANNEL_START_RPS, top=CHANNEL_MAX_RPS, step=CHANNEL_STEP_RPS, every=CHANNEL_STEP_EVERY):
    """Seconds one channel of `messages` takes at the egress proxy's pace:
    the inverse of ramp."""
    left, rate, seconds = float(messages), start, 0.0
    while left > 0 and rate < top:
        n = min(left, every)
        seconds, left, rate = seconds + n / rate, left - n, min(top, rate + step)
    return seconds + left / rate


def full_run(sizes, lanes, budget=TAUT_GRAPH_BUDGET):
    """How long a whole import of channels of these sizes takes under the
    limits, at best: every lane always has a channel, and nothing but
    importing messages uses the budget. It is the largest of three times:
    the tenant budget's, the lanes' (each channel starts slowly, so small
    channels never reach 5 a second) and the largest channel's own."""
    total = sum(sizes)
    per_channel = [ramp_seconds(n) for n in sizes]
    by_budget, by_lanes, by_largest = total / budget, sum(per_channel) / max(lanes, 1), max(per_channel, default=0.0)
    seconds = max(by_budget, by_lanes, by_largest)
    return {"seconds": seconds, "byBudget": by_budget, "byLanes": by_lanes, "byLargestChannel": by_largest,
            "rate": total / seconds if seconds else None,
            "limitedBy": {by_budget: "the tenant budget", by_lanes: "the number of lanes and each channel's slow start",
                          by_largest: "the largest channel, which one lane imports at 5 a second"}[seconds]}


def bound(seconds, lanes, channels, chats=0, budget=TAUT_GRAPH_BUDGET):
    """What the limits allow in `seconds` of importing, and how it is
    arrived at. `lanes` is how many conversations are imported at once
    (workers × lanes per worker); `channels` and `chats` how many of each
    there are to import."""
    parallel = min(lanes, channels + chats)
    microsoft_rate = min(DOCUMENTED["tenantChannels"], parallel * DOCUMENTED["channel"])
    taut_rate = min(budget, parallel * CHANNEL_MAX_RPS)
    per_lane = ramp(seconds)
    with_ramp = min(budget * seconds, parallel * per_lane)
    return {
        "seconds": seconds,
        "parallelConversations": parallel,
        "microsoftRate": microsoft_rate,
        "microsoftMessages": microsoft_rate * seconds,
        "tautRate": taut_rate,
        "tautMessages": taut_rate * seconds,
        "rampSeconds": sum(CHANNEL_STEP_EVERY / (CHANNEL_START_RPS + CHANNEL_STEP_RPS * i)
                           for i in range(int((CHANNEL_MAX_RPS - CHANNEL_START_RPS) / CHANNEL_STEP_RPS))),
        "perLaneWithRamp": per_lane,
        "tautMessagesWithRamp": with_ramp,
        "arithmetic": [
            f"conversations imported at once = min(workers x lanes, conversations) = {parallel}",
            f"Microsoft allows min(50/s per tenant, {parallel} x 5/s per channel) = {microsoft_rate:g}/s"
            f" = {comma(microsoft_rate * seconds)} messages in {human_seconds(seconds)}",
            f"Taut allows itself min({budget:g}/s budget, {parallel} x 5/s) = {taut_rate:g}/s"
            f" = {comma(taut_rate * seconds)} messages",
            f"a channel starts at 1/s and gains 0.5/s per 30 messages: {comma(per_lane)} messages per lane in"
            f" {human_seconds(seconds)} if each lane stays on one channel, {comma(with_ramp)} in all",
        ],
    }


# --- rates from the progress timeline ---------------------------------------


def rates(timeline, lanes):
    """Overall, steady-state and peak message rates from [(t, posted, active)].

    Steady state is the time during which nearly every lane had a channel to
    import; once the long tail is done, a few large channels are left and
    the rate is what one lane per channel can do."""
    pts = [p for p in timeline if p[1] is not None]
    moving = [p for p in pts if p[1] > 0]
    if len(moving) < 2:
        return {"overall": None, "steady": None, "peak": None, "loadSeconds": None, "steadySeconds": 0.0}
    # The load phase: from the last poll before the first post to the first
    # poll that shows the final count.
    first = pts[max(0, pts.index(moving[0]) - 1)]
    final = moving[-1][1]
    last = next(p for p in moving if p[1] == final)
    span = max(last[0] - first[0], 1e-9)
    busy_time, busy_posts = 0.0, 0
    best = 0.0
    for a, b in zip(pts, pts[1:]):
        dt = b[0] - a[0]
        if dt <= 0 or b[0] > last[0] or a[0] < first[0]:
            continue
        if min(a[2], b[2]) >= max(1, math.ceil(0.9 * lanes)):
            busy_time += dt
            busy_posts += b[1] - a[1]
    window = 30.0
    for i, a in enumerate(pts):
        for b in pts[i + 1:]:
            if b[0] - a[0] >= window:
                best = max(best, (b[1] - a[1]) / (b[0] - a[0]))
                break
    return {
        "overall": (final - first[1]) / span,
        "steady": busy_posts / busy_time if busy_time >= 5 else None,
        "peak": best or None,
        "loadSeconds": span,
        "steadySeconds": busy_time,
    }


def stage_times(polls):
    """Wall time per stage from [(t, stages)]: from the first poll that
    shows work on it to the first that shows all of its lanes done."""
    out = {}
    for t, stages in polls:
        for s in stages or []:
            name = s["stage"]
            st = out.setdefault(name, {"start": None, "end": None, "lanes": 0, "batches": 0})
            st["lanes"], st["batches"] = s.get("lanes", 0), s.get("batches", 0)
            working = s.get("lanesActive", 0) > 0 or s.get("lanesDone", 0) > 0 or s.get("batchesDone", 0) > 0
            if st["start"] is None and working:
                st["start"] = t
            done = s.get("lanes", 0) > 0 and s.get("lanesDone", 0) == s.get("lanes", 0)
            if done and st["end"] is None and st["start"] is not None:
                st["end"] = t
            if not done:
                st["end"] = None
    for st in out.values():
        st["seconds"] = st["end"] - st["start"] if st["start"] is not None and st["end"] is not None else None
    return out


# --- running things ----------------------------------------------------------


class Failure(Exception):
    pass


def sh(cmd, env=None, check=True, capture=True, cwd=None, timeout=None):
    p = subprocess.run(cmd, env=env, cwd=cwd, timeout=timeout, text=True,
                       stdout=subprocess.PIPE if capture else None, stderr=subprocess.STDOUT if capture else None)
    if check and p.returncode != 0:
        raise Failure(f"{' '.join(cmd[:6])} … exited {p.returncode}\n{(p.stdout or '')[-2000:]}")
    return p.stdout or ""


def http_json(url, method="GET", timeout=30):
    req = urllib.request.Request(url, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = resp.read()
    return json.loads(body) if body else None


def say(text=""):
    print(text, flush=True)


def step(text):
    say(f"  · {text}")


class Stack:
    """The Compose stack with the load-test overlay."""

    def __init__(self, workers, env):
        if workers not in (2, 4, 8):
            raise Failure("the stack runs 2, 4 or 8 workers")
        self.workers = workers
        self.env = dict(os.environ, **env)
        self.base = ["docker", "compose", "-f", os.path.join(COMPOSE_DIR, "compose.yaml"),
                     "-f", os.path.join(COMPOSE_DIR, "compose.loadtest.yaml")]
        if workers > 2:
            self.base += ["--profile", f"w{workers}"]

    def compose(self, *args, check=True, timeout=None):
        return sh(self.base + list(args), env=self.env, check=check, timeout=timeout)

    def down(self):
        # Every profile, so that workers of an earlier, larger run go too.
        sh(self.base[:6] + ["--profile", "w4", "--profile", "w8", "down", "-v", "--remove-orphans"], env=self.env, check=False)

    def up(self):
        self.compose("up", "-d", "--wait", "--remove-orphans", timeout=600)

    def psql(self, sql):
        return self.compose("exec", "-T", "postgres", "psql", "-U", "taut", "-d", "taut", "-At", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-c", sql)

    def rows(self, sql):
        return [line.split("\t") for line in self.psql(sql).splitlines() if line]


class Sampler(threading.Thread):
    """Samples `docker stats` and the controller's API while a run goes on."""

    def __init__(self, run_id):
        super().__init__(daemon=True)
        self.run_id = run_id
        self.stop = threading.Event()
        self.containers = {}  # name → {"cpu": [...], "mem": [...]}
        self.api = {"status": [], "dashboard": []}
        self.api_errors = 0

    @staticmethod
    def _bytes(text):
        m = re.match(r"\s*([0-9.]+)\s*([KMGT]?i?B)", text)
        if not m:
            return 0.0
        mult = {"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}
        return float(m.group(1)) * mult.get(m.group(2), 1)

    def _stats(self):
        try:
            out = sh(["docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"], check=False, timeout=30)
        except subprocess.TimeoutExpired:
            return
        for line in out.splitlines():
            parts = line.split("\t")
            if len(parts) != 3 or not parts[0].startswith("taut-"):
                continue
            c = self.containers.setdefault(parts[0], {"cpu": [], "mem": []})
            try:
                c["cpu"].append(float(parts[1].rstrip("%")))
            except ValueError:
                continue
            c["mem"].append(self._bytes(parts[2].split("/")[0]))

    def _api(self):
        for name, path in (("status", f"/v1/runs/{self.run_id}"), ("dashboard", f"/v1/runs/{self.run_id}/dashboard")):
            start = time.monotonic()
            try:
                with urllib.request.urlopen(CONTROLLER + path, timeout=30) as resp:
                    resp.read()
                self.api[name].append(time.monotonic() - start)
            except (urllib.error.URLError, OSError):
                self.api_errors += 1

    def run(self):
        api = threading.Thread(target=self._api_loop, daemon=True)
        api.start()
        while not self.stop.is_set():
            self._stats()
            self.stop.wait(2.0)
        api.join(timeout=35)

    def _api_loop(self):
        while not self.stop.is_set():
            self._api()
            self.stop.wait(1.0)

    def summary(self):
        roles = {}
        for name, c in self.containers.items():
            # taut-worker-a-1 → worker; taut-egress-proxy-b-1 → egress-proxy
            role = re.sub(r"^taut-(.*?)(?:-[a-h])?-\d+$", r"\1", name)
            r = roles.setdefault(role, {"containers": 0, "peakRssBytes": 0.0, "cpuPercentMean": 0.0, "cpuPercentPeak": 0.0})
            r["containers"] += 1
            r["peakRssBytes"] = max(r["peakRssBytes"], max(c["mem"], default=0.0))
            # CPU is added over the role's containers: 100 is one core.
            r["cpuPercentMean"] += statistics.fmean(c["cpu"]) if c["cpu"] else 0.0
            r["cpuPercentPeak"] += max(c["cpu"], default=0.0)
        api = {}
        for name, values in self.api.items():
            api[name] = {"requests": len(values), "p50": quantile(values, 0.5), "p95": quantile(values, 0.95),
                         "p99": quantile(values, 0.99), "max": max(values, default=None)}
        return roles, api


def verify(stack, run_id, facts, complete):
    """Compare the ledger with what the fake tenant holds."""
    summary = http_json(FAKE + "/_fake/summary", timeout=120)
    tenant = {}
    one_per_ms = True
    for c in summary["channels"] + summary["chats"]:
        tenant[c.get("channelId") or c.get("chatId")] = c
        one_per_ms = one_per_ms and c["distinctMillis"] == c["messages"]
    ledger = stack.rows(
        "SELECT c.slack_id, c.teams_id, c.kind, c.state, count(m.*), count(m.*) FILTER (WHERE m.state = 'posted'), "
        "count(m.*) FILTER (WHERE m.state IN ('failed', 'held')) "
        f"FROM map_channels c LEFT JOIN map_messages m ON m.run_id = c.run_id AND m.channel_id = c.slack_id "
        f"WHERE c.run_id = '{run_id}' GROUP BY 1, 2, 3, 4")
    posted = mismatched = short = failed = over = 0
    under = False
    examples = []
    for slack_id, teams_id, _kind, _state, posts, done, bad in ledger:
        posts, done, bad = int(posts), int(done), int(bad)
        posted += done
        failed += bad
        have = tenant.get(teams_id, {}).get("imported", 0)
        if have != done:
            mismatched += 1
            over = max(over, have - done)
            under = under or have < done
            if len(examples) < 5:
                examples.append(f"{slack_id}: ledger {done}, tenant {have}")
        if complete and (done + bad != posts or posts < facts["PerChannel"].get(slack_id, {}).get("Messages", 0)):
            short += 1
    accepted = summary["stats"]["importsAccepted"]
    result = {
        "conversations": len(ledger),
        "ledgerPosted": posted,
        "ledgerFailedOrHeld": failed,
        "tenantAccepted": accepted,
        "tenantConflicts": summary["stats"]["conflicts"],
        "tenantThrottles": summary["stats"]["throttles"],
        "oneMessagePerMillisecond": one_per_ms,
        "conversationsThatDiffer": mismatched,
        "tenantMinusLedger": accepted - posted,
        "conversationsShortOfTheExport": short,
        "examples": examples,
        "exportMessages": facts["Messages"],
    }
    # A complete run: everything, exactly. A time-boxed run was cancelled in
    # mid-flight: a message Graph accepted whose record a worker had not yet
    # committed is in the tenant and not in the ledger. That is the case a
    # resumed run adopts instead of importing again; there is at most one
    # per conversation, and never the other way round.
    result["ok"] = bool(one_per_ms and posted > 0 and (
        (mismatched == 0 and accepted == posted and short == 0 and failed == 0) if complete
        else (not under and over <= 1 and accepted >= posted)))
    return result


def database(stack):
    size = int(stack.psql("SELECT pg_database_size('taut')").strip())
    tables = [{"table": t, "bytes": int(b), "rows": int(float(r))} for t, b, r in stack.rows(
        "SELECT s.relname, pg_total_relation_size(s.relid), s.n_live_tup FROM pg_stat_user_tables s ORDER BY 2 DESC LIMIT 6")]
    slow = []
    try:
        for calls, total, mean, query in stack.rows(
                "SELECT calls, round(total_exec_time::numeric, 1), round(mean_exec_time::numeric, 3), "
                "left(regexp_replace(query, '\\s+', ' ', 'g'), 110) FROM pg_stat_statements "
                "WHERE query NOT LIKE '%pg_stat_statements%' AND query NOT ILIKE 'copy %' ORDER BY total_exec_time DESC LIMIT 8"):
            slow.append({"calls": int(calls), "totalMs": float(total), "meanMs": float(mean), "query": query})
    except Failure:
        pass  # the extension is a convenience of the overlay, not a requirement
    return {"bytes": size, "tables": tables, "statements": slow}


def staging_bytes():
    out = sh(["docker", "run", "--rm", "-v", "taut_staging:/d:ro", "postgres:17-alpine", "du", "-sk", "/d"], check=False, timeout=120)
    m = re.match(r"\s*(\d+)", out)
    return int(m.group(1)) * 1024 if m else None


def metrics_report(samples):
    def h(name, **want):
        return histogram(samples, name, **want).summary()

    worker_graph = h("http_client_request_duration_seconds", service_name="taut-worker", taut_route_class="graph.messages", http_request_method="POST")
    proxy_graph = h("http_client_request_duration_seconds", service_name="taut-egress-proxy", taut_route_class="graph.messages", http_request_method="POST")
    throttled = sum(v for s, labels, v in samples if s == "http_client_request_duration_seconds_count"
                    and labels.get("service_name") == "taut-egress-proxy" and labels.get("http_response_status_code") == "429")
    stages = {}
    for name in ("extract", "transform", "files", "load", "team", "finalize", "members", "plan"):
        tx = h("taut_ledger_transaction_duration_seconds", taut_stage=name)
        batch = h("taut_batch_duration_seconds", taut_stage=name)
        if tx["count"] or batch["count"]:
            stages[name] = {"ledgerTransaction": tx, "batch": batch}
    # What a message import costs as the worker sees it, less what the
    # upstream took as the egress proxy sees it: time the proxy held the
    # request back to keep to the limits (and, a little, its own work).
    waited = None
    if worker_graph["count"] and proxy_graph["count"]:
        waited = max(0.0, worker_graph["sum"] - proxy_graph["sum"])
    return {
        "graphImportAsWorkerSeesIt": worker_graph,
        "graphImportUpstream": proxy_graph,
        "secondsHeldByEgressProxy": waited,
        "throttledResponsesSeenByEgressProxy": int(throttled),
        "ledgerTransactionAllStages": h("taut_ledger_transaction_duration_seconds"),
        "stages": stages,
        "postsImported": int(counter(samples, "taut_posts_total", outcome="imported")),
        "postsAdopted": int(counter(samples, "taut_posts_total", outcome="adopted")),
        "batchRetries": int(counter(samples, "taut_batch_retries_total")),
        "leasesLost": int(counter(samples, "taut_lease_lost_total")),
    }


def one_run(args, profile, workers, workdir):
    p = PROFILES[profile]
    spec = args.spec or p["spec"]
    if args.dms:
        spec += f",dms={args.dms}"
    latency, jitter = args.latency or p["latency"], args.jitter or p["jitter"]
    throttled = p["limits"] == "documented"
    lanes = workers * args.concurrency

    say()
    say(f"  ◆ taut load-test  {profile}, {workers} workers")
    say()
    step(f"generating the export (load:{spec})")
    start = time.monotonic()
    fixture = json.loads(sh([TAUT, "dev", "fixtures", "--json", "--load", spec, "--out", workdir]))
    facts = fixture["facts"]
    gen_seconds = time.monotonic() - start
    chats = facts["DMs"] + facts["MPIMs"]
    channels = facts["Channels"] - chats

    env = {
        "TAUT_LOADTEST_FIXTURE": fixture["fixture"],
        "TAUT_LOADTEST_LIMITS": p["limits"],
        "TAUT_LOADTEST_LATENCY": latency,
        "TAUT_LOADTEST_JITTER": jitter,
        "TAUT_LOADTEST_CONCURRENCY": str(args.concurrency),
    }
    if throttled:
        # The pace a real tenant gets: the product's defaults.
        env.update({"TAUT_LOADTEST_GRAPH_RPS": f"{TAUT_GRAPH_BUDGET:g}", "TAUT_LOADTEST_SHAREPOINT_RPS": "20",
                    "TAUT_LOADTEST_CHANNEL_RPS": f"{CHANNEL_START_RPS:g}", "TAUT_LOADTEST_CHANNEL_MAX": f"{CHANNEL_MAX_RPS:g}",
                    "TAUT_LOADTEST_CHAT_RPS": f"{CHAT_RPS:g}"})
    else:
        # Out of the way: the proxies' own pacing far above anything the
        # stack can do, so that what is measured is Taut.
        env.update({"TAUT_LOADTEST_GRAPH_RPS": "100000", "TAUT_LOADTEST_SHAREPOINT_RPS": "100000",
                    "TAUT_LOADTEST_CHANNEL_RPS": "10000", "TAUT_LOADTEST_CHANNEL_MAX": "20000", "TAUT_LOADTEST_CHAT_RPS": "10000"})
    stack = Stack(workers, env)
    step("starting a fresh stack")
    stack.down()
    stack.up()
    try:
        stack.psql("CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
        stack.psql("SELECT pg_stat_statements_reset()")
    except Failure:
        pass

    seed = json.loads(sh([TAUT, "dev", "seed"]))
    run_env = dict(os.environ, TAUT_ENTRA_CLIENT_SECRET=seed["clientSecret"], TAUT_SLACK_TOKEN=seed["slackToken"])
    config = os.path.join(workdir, "taut.yaml")
    name = f"load-{profile}-{workers}w-{int(time.time()) % 100000}"
    init = [TAUT, "init", "--yes", "--force", "--config", config, "--name", name, "--export", fixture["path"],
            "--tenant-id", seed["tenantId"], "--client-id", seed["clientId"], "--client-secret-env", "TAUT_ENTRA_CLIENT_SECRET",
            "--team", "Load Corp", "--owner", seed["owner"], "--unmapped-account", seed["unmappedAccount"],
            "--authority", FAKE, "--graph-url", FAKE, "--no-slack-api", "--include-private"]
    if chats:
        init.append("--include-direct-messages")
    sh(init, env=run_env)

    step("starting the import")
    started = time.monotonic()
    run = json.loads(sh([TAUT, "migrate", "--yes", "--detach", "--json", "--config", config], env=run_env))
    run_id = run["id"]
    upload_seconds = time.monotonic() - started
    sampler = Sampler(run_id)
    sampler.start()

    timeline, polls = [], []
    first_post = cancelled_at = None
    status = "running"
    box = args.minutes * 60.0
    last_note = 0.0
    while True:
        try:
            v = http_json(f"{CONTROLLER}/v1/runs/{run_id}", timeout=60)
        except (urllib.error.URLError, OSError) as e:
            raise Failure(f"the controller stopped answering: {e}") from e
        now = time.monotonic() - started
        prog = v.get("progress") or {}
        active = sum(1 for c in prog.get("channels") or [] if c.get("active"))
        timeline.append((now, prog.get("posted", 0), active))
        polls.append((now, prog.get("stages")))
        status = v["status"]
        if first_post is None and prog.get("posted", 0) > 0:
            first_post = now
        if now - last_note >= 30:
            last_note = now
            step(f"{human_seconds(now)}: {comma(prog.get('posted', 0))} of {comma(prog.get('posts', 0))} posts, {active} conversations importing")
        if status in ("completed", "failed", "cancelled", "finalized"):
            break
        if throttled and first_post is not None and now - first_post >= box and cancelled_at is None:
            cancelled_at = now
            step(f"time is up after {human_seconds(box)} of importing: cancelling the run")
            http_json(f"{CONTROLLER}/v1/runs/{run_id}/cancel", method="POST")
        if now > args.timeout * 60:
            raise Failure(f"the run did not finish in {args.timeout} minutes (status {status})")
        time.sleep(1.0)
    import_seconds = time.monotonic() - started
    if status == "failed":
        problems = [p.get("error", "") for p in (v.get("progress") or {}).get("problems") or []][:3]
        raise Failure("the run failed: " + "; ".join(problems))
    if cancelled_at is not None:
        # Let the workers put down what they were doing.
        stable, last = 0, -1
        while stable < 5:
            time.sleep(1.0)
            n = http_json(FAKE + "/_fake/summary", timeout=120)["stats"]["importsAccepted"]
            stable, last = (stable + 1, n) if n == last else (0, n)

    complete = status == "completed"
    step("checking the import against the fake tenant")
    check = verify(stack, run_id, facts, complete)
    step("measuring the database and staging")
    db = database(stack)
    staged = staging_bytes()

    finalize = None
    if complete and not args.no_finalize:
        step("finalizing")
        f_start = time.monotonic()
        sh([TAUT, "finalize", "--yes", "--detach", "--json", "--config", config], env=run_env, timeout=600)
        finalize = {"seconds": None, "purgeSeconds": None}
        # Finalized when every channel is out of migration mode and every
        # member is added; the staging is deleted after that.
        while time.monotonic() - f_start < args.timeout * 60:
            v = http_json(f"{CONTROLLER}/v1/runs/{run_id}", timeout=60)
            now = time.monotonic()
            polls.append((now - started, (v.get("progress") or {}).get("stages")))
            if v["status"] == "failed":
                raise Failure("finalizing failed")
            if v["status"] == "finalized":
                if finalize["seconds"] is None:
                    finalize["seconds"] = now - f_start
                if v.get("purgedAt"):
                    finalize["purgeSeconds"] = now - f_start - finalize["seconds"]
                    break
                if now - f_start - finalize["seconds"] > 180:
                    break
            time.sleep(0.5)
        finalize["status"] = v["status"]

    # The workers export their metrics every ten seconds.
    time.sleep(12)
    sampler.stop.set()
    sampler.join(timeout=60)
    with urllib.request.urlopen(METRICS, timeout=30) as resp:
        samples = parse_prometheus(resp.read().decode())
    containers, api = sampler.summary()
    stages = stage_times(polls)
    rate = rates(timeline, lanes)

    report = {
        "profile": profile,
        "workers": workers,
        "lanesPerWorker": args.concurrency,
        "fixture": fixture["fixture"],
        "export": {"bytes": fixture["bytes"], "messages": facts["Messages"], "channels": channels, "chats": chats,
                   "users": facts["Users"], "replies": facts["Replies"], "files": facts["Files"], "dayFiles": facts["DayFiles"],
                   "largestChannel": max(c["Messages"] for c in facts["PerChannel"].values()), "generateSeconds": gen_seconds},
        "fake": {"limits": p["limits"], "latency": latency, "jitter": jitter},
        "status": status,
        "timeBoxSeconds": box if throttled else None,
        "uploadSeconds": upload_seconds,
        "importSeconds": import_seconds,
        "rates": rate,
        "stages": stages,
        "finalize": finalize,
        "verification": check,
        "metrics": metrics_report(samples),
        "controllerApi": api,
        "controllerApiErrors": sampler.api_errors,
        "containers": containers,
        "postgres": db,
        "stagingBytes": staged,
        "lanes": {s["stage"]: s.get("lanes", 0) for s in (polls[-1][1] or [])},
    }
    if throttled:
        t = (cancelled_at - first_post) if cancelled_at and first_post else rate["loadSeconds"]
        report["bound"] = bound(t, lanes, channels, chats)
        touched = stack.rows(f"SELECT count(DISTINCT channel_id) FROM map_messages WHERE run_id = '{run_id}' AND state = 'posted'")
        report["bound"]["conversationsStarted"] = int(touched[0][0]) if touched else 0
        report["bound"]["achieved"] = check["ledgerPosted"]
        sizes = [c["Messages"] for c in facts["PerChannel"].values() if c["Kind"] in ("public", "private")]
        report["fullRunEstimate"] = {str(n): full_run(sizes, n * args.concurrency) for n in (2, 4, 8)}
    if not args.keep:
        stack.down()
    return report


# --- the report --------------------------------------------------------------


def table(rows, indent="    "):
    if not rows:
        return ""
    widths = [max(len(str(r[i])) for r in rows) for i in range(len(rows[0]))]
    lines = []
    for r in rows:
        cells = [str(c).ljust(widths[i]) if i == 0 or len(r) == 2 else str(c).rjust(widths[i]) for i, c in enumerate(r)]
        lines.append(indent + "   ".join(cells).rstrip())
    return "\n".join(lines)


def q3(h):
    if not h or not h.get("count"):
        return "-"
    return f"{human_seconds(h['p50'])} / {human_seconds(h['p95'])} / {human_seconds(h['p99'])}"


def render(report):
    r = report
    out = []
    e, m, v = r["export"], r["metrics"], r["verification"]
    out.append("")
    out.append(f"  ◆ taut load-test  {r['profile']}, {r['workers']} workers x {r['lanesPerWorker']} lanes")
    out.append("")
    kv = [
        ("Export", f"{comma(e['messages'])} messages in {comma(e['channels'])} channels"
                   + (f" and {comma(e['chats'])} direct conversations" if e["chats"] else "")
                   + f", {human_bytes(e['bytes'])}; largest channel {comma(e['largestChannel'])}"),
        ("Fake Graph", f"limits {r['fake']['limits']}, latency {r['fake']['latency']} + up to {r['fake']['jitter']}"),
        ("Run", f"{r['status']} after {human_seconds(r['importSeconds'])}"
                + (f" (time box {human_seconds(r['timeBoxSeconds'])} of importing)" if r["timeBoxSeconds"] else "")),
    ]
    rate = r["rates"]
    if rate["overall"]:
        parts = [f"{rate['overall']:.0f} overall"]
        if rate["steady"]:
            parts.append(f"{rate['steady']:.0f} while every lane was busy ({human_seconds(rate['steadySeconds'])})")
        if rate["peak"]:
            parts.append(f"{rate['peak']:.0f} at best over 30 s")
        kv.append(("Messages/s", ", ".join(parts)))
    out.append(table([(k, val) for k, val in kv]))
    out.append("")
    out.append("  Stages (wall time, lanes, batches)")
    rows = []
    for name in ("plan", "extract", "transform", "team", "files", "load", "finalize", "members"):
        s = r["stages"].get(name)
        if s:
            took = "under 1 s" if s["seconds"] == 0 else human_seconds(s["seconds"])
            rows.append((name, took, comma(s["lanes"]), comma(s["batches"])))
    out.append(table(rows))
    out.append("")
    out.append("  Latency (p50 / p95 / p99)")
    rows = [
        ("Graph import, as the worker sees it", q3(m["graphImportAsWorkerSeesIt"])),
        ("Graph import, upstream of the egress proxy", q3(m["graphImportUpstream"])),
        ("ledger transaction, all stages", q3(m["ledgerTransactionAllStages"])),
    ]
    for name, s in m["stages"].items():
        if s["ledgerTransaction"]["count"]:
            rows.append((f"ledger transaction, {name}", q3(s["ledgerTransaction"])))
    for name, a in r["controllerApi"].items():
        rows.append((f"controller API, {name} ({a['requests']} requests)",
                     f"{human_seconds(a['p50'])} / {human_seconds(a['p95'])} / {human_seconds(a['p99'])}" if a["requests"] else "-"))
    out.append(table(rows))
    out.append("")
    out.append("  Limits")
    held = m["secondsHeldByEgressProxy"]
    out.append(table([
        ("429 responses from Graph", comma(v["tenantThrottles"])),
        ("time the egress proxies added to imports (pacing, and their own work)",
         f"{human_seconds(held)} in all, {human_seconds(held / max(m['graphImportAsWorkerSeesIt']['count'], 1))} a message" if held is not None else "-"),
        ("batches retried, leases lost", f"{comma(m['batchRetries'])}, {comma(m['leasesLost'])}"),
    ]))
    if r.get("bound"):
        b = r["bound"]
        out.append("")
        out.append("  Theoretical bound")
        for line in b["arithmetic"]:
            out.append("    " + line)
        out.append(f"    imported: {comma(b['achieved'])} into {comma(b['conversationsStarted'])} conversations ="
                   f" {100 * b['achieved'] / b['tautMessages']:.0f}% of Taut's own bound,"
                   f" {100 * b['achieved'] / b['microsoftMessages']:.0f}% of Microsoft's;"
                   f" {100 * b['achieved'] / b['tautMessagesWithRamp']:.0f}% of the bound with the slow start")
        if r.get("fullRunEstimate"):
            out.append("")
            out.append("  The whole export under these limits, at best (channels only)")
            rows = [("workers", "time", "messages/s", "limited by")]
            for n, e2 in r["fullRunEstimate"].items():
                rows.append((n, human_seconds(e2["seconds"]), f"{e2['rate']:.1f}", e2["limitedBy"]))
            out.append(table(rows))
    out.append("")
    out.append("  Containers (peak memory of one, CPU of all in cores: mean / peak)")
    rows = []
    for role in sorted(r["containers"], key=lambda k: -r["containers"][k]["cpuPercentMean"]):
        c = r["containers"][role]
        rows.append((f"{role} x{c['containers']}", human_bytes(c["peakRssBytes"]), f"{c['cpuPercentMean'] / 100:.2f} / {c['cpuPercentPeak'] / 100:.2f}"))
    out.append(table(rows))
    out.append("")
    db = r["postgres"]
    out.append(f"  Postgres  {human_bytes(db['bytes'])}"
               + (f"   staging  {human_bytes(r['stagingBytes'])}" if r["stagingBytes"] else ""))
    out.append(table([(t["table"], human_bytes(t["bytes"]), comma(t["rows"]) + " rows") for t in db["tables"]]))
    if db["statements"]:
        out.append("")
        out.append("  Statements by total time (calls, total, mean)")
        out.append(table([(s["query"][:78], comma(s["calls"]), human_seconds(s["totalMs"] / 1000), human_seconds(s["meanMs"] / 1000))
                          for s in db["statements"]]))
    out.append("")
    if v["ok"]:
        what = "every message of the export is in the tenant exactly once" if r["status"] == "completed" else \
            "no duplicates; what the ledger records is in the tenant" + (
                f" ({v['tenantMinusLedger']} accepted by Graph when the run was cancelled, not yet recorded)" if v["tenantMinusLedger"] else "")
        out.append(f"  ✓ Correct  {what}: ledger {comma(v['ledgerPosted'])}, tenant {comma(v['tenantAccepted'])}")
    else:
        out.append(f"  ✗ MISMATCH  ledger {comma(v['ledgerPosted'])}, tenant {comma(v['tenantAccepted'])}, "
                   f"{v['conversationsThatDiffer']} conversations differ, {v['conversationsShortOfTheExport']} short of the export, "
                   f"{v['ledgerFailedOrHeld']} failed or held; {'; '.join(v['examples'])}")
    if r.get("finalize"):
        f = r["finalize"]
        out.append(f"  ✓ Finalized in {human_seconds(f['seconds'])}"
                   + (f"; staging deleted {human_seconds(f['purgeSeconds'])} later" if f.get("purgeSeconds") is not None else "; staging not yet deleted"))
    return "\n".join(out)


def render_scaling(reports):
    out = ["", "  ◆ taut load-test  scaling", ""]
    head = ["workers"] + [str(r["workers"]) for r in reports]

    def row(label, fn):
        return [label] + [fn(r) for r in reports]

    def f(x, spec="{:.0f}"):
        return spec.format(x) if x is not None else "-"

    rows = [
        head,
        row("import, wall time", lambda r: human_seconds(r["importSeconds"])),
        row("messages/s overall", lambda r: f(r["rates"]["overall"])),
        row("messages/s, every lane busy", lambda r: f(r["rates"]["steady"])),
        row("messages/s, best 30 s", lambda r: f(r["rates"]["peak"])),
        row("load stage", lambda r: human_seconds(r["stages"].get("load", {}).get("seconds"))),
        row("finalize", lambda r: human_seconds((r.get("finalize") or {}).get("seconds"))),
        row("transform stage", lambda r: human_seconds(r["stages"].get("transform", {}).get("seconds"))),
        row("Graph import p50 (worker)", lambda r: human_seconds(r["metrics"]["graphImportAsWorkerSeesIt"]["p50"])),
        row("ledger transaction p50 / p99", lambda r: human_seconds(r["metrics"]["ledgerTransactionAllStages"]["p50"]) + " / "
            + human_seconds(r["metrics"]["ledgerTransactionAllStages"]["p99"])),
        row("controller status p95", lambda r: human_seconds(r["controllerApi"]["status"]["p95"])),
        row("Postgres CPU, cores (mean)", lambda r: f(r["containers"].get("postgres", {}).get("cpuPercentMean", 0) / 100, "{:.2f}")),
        row("workers CPU, cores (mean)", lambda r: f(r["containers"].get("worker", {}).get("cpuPercentMean", 0) / 100, "{:.2f}")),
        row("fake Graph CPU, cores (mean)", lambda r: f(r["containers"].get("fakegraph", {}).get("cpuPercentMean", 0) / 100, "{:.2f}")),
        row("correct", lambda r: "yes" if r["verification"]["ok"] else "NO"),
    ]
    out.append(table(rows))
    return "\n".join(out)


# --- self-test ---------------------------------------------------------------


def self_test():
    """Checks of the arithmetic, so that the script cannot rot unnoticed. It
    needs no stack."""
    def eq(got, want, what, tol=1e-6):
        if (got is None) != (want is None) or (got is not None and abs(got - want) > tol):
            raise AssertionError(f"{what}: got {got}, want {want}")

    eq(quantile([5, 1, 3, 2, 4], 0.5), 3, "median")
    eq(quantile([1, 2, 3, 4], 0.99), 4, "p99")
    eq(quantile([], 0.5), None, "empty")
    eq(parse_duration("5ms"), 0.005, "5ms")
    eq(parse_duration("0s"), 0.0, "0s")
    eq(parse_duration("1.5s"), 1.5, "1.5s")

    page = """# TYPE d histogram
d_bucket{service_name="w",route="a",le="0.01"} 10
d_bucket{service_name="w",route="a",le="0.1"} 90
d_bucket{service_name="w",route="a",le="+Inf"} 100
d_sum{service_name="w",route="a"} 6.5
d_count{service_name="w",route="a"} 100
d_bucket{service_name="w",route="b",le="0.01"} 0
d_bucket{service_name="w",route="b",le="0.1"} 100
d_bucket{service_name="w",route="b",le="+Inf"} 100
d_sum{service_name="w",route="b"} 5
d_count{service_name="w",route="b"} 100
d_count{service_name="x",route="a"} 7
posts_total{outcome="imported",q="a \\"quoted\\" value"} 41
posts_total{outcome="adopted"} 1
"""
    samples = parse_prometheus(page)
    a = histogram(samples, "d", service_name="w", route="a")
    eq(a.count, 100, "count")
    eq(a.quantile(0.5), 0.01 + 0.09 * 40 / 80, "p50 interpolated")
    eq(a.quantile(0.99), 0.1, "p99 in the last bucket")
    eq(a.mean(), 0.065, "mean")
    both = histogram(samples, "d", service_name="w")
    eq(both.count, 200, "summed count")
    eq(both.quantile(0.5), 0.01 + 0.09 * 90 / 180, "summed p50")
    eq(counter(samples, "posts_total", outcome="imported"), 41, "counter with a quoted label")
    eq(histogram(samples, "d", service_name="nobody").quantile(0.5), None, "no series")

    # The ramp: 1/s, +0.5 per 30 messages, to 5/s.
    eq(ramp(30), 30, "30 s at 1/s")
    eq(ramp(30 + 20), 30 + 30, "then 1.5/s")
    full = sum(30 / (1 + 0.5 * i) for i in range(8))
    eq(ramp(full), 240, "240 messages to reach 5/s")
    eq(ramp(full + 10), 290, "then 5/s")
    eq(ramp_seconds(30), 30, "30 messages at 1/s")
    eq(ramp_seconds(240), full, "240 messages to reach 5/s")
    eq(ramp_seconds(290), full + 10, "then 5/s")
    eq(ramp(ramp_seconds(1234)), 1234, "ramp and ramp_seconds are inverses")
    fr = full_run([240] * 100, lanes=8)
    eq(fr["seconds"], 100 * full / 8, "small channels: the lanes limit")
    eq(full_run([100000], lanes=8)["seconds"], ramp_seconds(100000), "one channel: its own pace")
    eq(full_run([10000] * 100, lanes=32)["seconds"], 1e6 / 40, "many lanes: the budget")
    b = bound(300, lanes=8, channels=150)
    eq(b["tautRate"], 40, "8 lanes x 5 = the budget")
    eq(b["microsoftRate"], 40, "8 lanes x 5 under Microsoft's 50")
    eq(b["tautMessages"], 12000, "Taut's bound")
    eq(b["rampSeconds"], full, "ramp time")
    eq(b["tautMessagesWithRamp"], min(12000, 8 * ramp(300)), "with the ramp")
    eq(bound(300, lanes=32, channels=150)["microsoftRate"], 50, "the tenant limit")
    eq(bound(300, lanes=32, channels=3)["parallelConversations"], 3, "fewer channels than lanes")

    # Rates: 8 lanes busy for 20 s at 100/s, then one lane at 10/s for 20 s.
    tl = [(0, 0, 0), (5, 0, 0)] + [(5 + i, 100 * i, 8) for i in range(1, 21)] + [(25 + i, 2000 + 10 * i, 1) for i in range(1, 21)] + [(50, 2200, 0)]
    r = rates(tl, lanes=8)
    eq(r["overall"], 2200 / 40, "overall rate")
    eq(r["steady"], 100, "steady rate")
    eq(r["loadSeconds"], 40, "load time")
    eq(rates([(0, 0, 0), (1, 0, 0)], 8)["overall"], None, "nothing imported")

    polls = [(0, [{"stage": "load", "lanes": 2, "lanesDone": 0, "lanesActive": 0, "batchesDone": 0, "batches": 9}]),
             (3, [{"stage": "load", "lanes": 2, "lanesDone": 0, "lanesActive": 1, "batchesDone": 0, "batches": 9}]),
             (9, [{"stage": "load", "lanes": 2, "lanesDone": 2, "lanesActive": 0, "batchesDone": 9, "batches": 9}]),
             (10, [{"stage": "load", "lanes": 2, "lanesDone": 2, "lanesActive": 0, "batchesDone": 9, "batches": 9}])]
    eq(stage_times(polls)["load"]["seconds"], 6, "stage wall time")

    h = {"count": 10, "mean": 0.01, "p50": 0.008, "p95": 0.02, "p99": 0.03, "sum": 0.1}
    canned = {
        "profile": "throttled", "workers": 2, "lanesPerWorker": 4, "fixture": "load:x", "status": "cancelled",
        "export": {"bytes": 1 << 24, "messages": 120000, "channels": 150, "chats": 0, "users": 400, "replies": 1, "files": 1,
                   "dayFiles": 1, "largestChannel": 21000, "generateSeconds": 1.0},
        "fake": {"limits": "documented", "latency": "5ms", "jitter": "5ms"}, "timeBoxSeconds": 300, "uploadSeconds": 1.0,
        "importSeconds": 330.0, "rates": {"overall": 35.0, "steady": 38.0, "peak": 39.5, "loadSeconds": 300.0, "steadySeconds": 250.0},
        "stages": {"load": {"seconds": None, "lanes": 150, "batches": 30000}, "extract": {"seconds": 12.0, "lanes": 150, "batches": 30000}},
        "finalize": None,
        "verification": {"ok": True, "ledgerPosted": 10500, "tenantAccepted": 10503, "tenantThrottles": 12, "tenantMinusLedger": 3,
                         "conversationsThatDiffer": 3, "conversationsShortOfTheExport": 0, "ledgerFailedOrHeld": 0, "examples": []},
        "metrics": {"graphImportAsWorkerSeesIt": h, "graphImportUpstream": h, "secondsHeldByEgressProxy": 2100.0,
                    "throttledResponsesSeenByEgressProxy": 12, "ledgerTransactionAllStages": h,
                    "stages": {"load": {"ledgerTransaction": h, "batch": h}}, "postsImported": 10500, "postsAdopted": 0,
                    "batchRetries": 0, "leasesLost": 0},
        "controllerApi": {"status": {"requests": 300, "p50": 0.02, "p95": 0.05, "p99": 0.08, "max": 0.1},
                          "dashboard": {"requests": 0, "p50": None, "p95": None, "p99": None, "max": None}},
        "controllerApiErrors": 0,
        "containers": {"worker": {"containers": 2, "peakRssBytes": 3e8, "cpuPercentMean": 150.0, "cpuPercentPeak": 380.0},
                       "postgres": {"containers": 1, "peakRssBytes": 5e8, "cpuPercentMean": 90.0, "cpuPercentPeak": 200.0}},
        "postgres": {"bytes": 3 << 28, "tables": [{"table": "map_messages", "bytes": 1 << 28, "rows": 120000}],
                     "statements": [{"calls": 5, "totalMs": 1234.5, "meanMs": 2.5, "query": "SELECT 1"}]},
        "stagingBytes": 1 << 27, "lanes": {"load": 150},
        "bound": dict(bound(300, 8, 150), conversationsStarted=40, achieved=10500),
        "fullRunEstimate": {"2": full_run([240] * 100, 8)},
    }
    text = render(canned)
    for want in ("throttled, 2 workers x 4 lanes", "Theoretical bound", "88% of Taut's own bound", "✓ Correct", "map_messages",
                 "429 responses from Graph", "ledger transaction, load", "worker x2", "5 min 30 s", "Microsoft allows", "The whole export under these limits", "the number of lanes"):
        if want not in text:
            raise AssertionError(f"the report lacks {want!r}:\n{text}")
    side = render_scaling([dict(canned, workers=n, profile="ceiling") for n in (2, 4, 8)])
    for want in ("workers", "messages/s overall", "correct"):
        if want not in side:
            raise AssertionError(f"the comparison lacks {want!r}:\n{side}")
    json.dumps(canned)
    print("loadtest.py self-test: ok")


# --- main --------------------------------------------------------------------


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--profile", default=os.environ.get("PROFILE", "ceiling"), choices=["ceiling", "throttled", "scaling", "smoke"])
    ap.add_argument("--workers", type=int, default=int(os.environ.get("WORKERS", "2")), help="2, 4 or 8 (default 2)")
    ap.add_argument("--concurrency", type=int, default=4, help="lanes per worker (default 4, the product's)")
    ap.add_argument("--spec", default=os.environ.get("SPEC", ""), help="the load fixture, e.g. channels=300,messages=1000000")
    ap.add_argument("--dms", type=int, default=int(os.environ.get("DMS", "0")), help="add this many direct conversations")
    ap.add_argument("--minutes", type=float, default=float(os.environ.get("MINUTES", "5")), help="throttled: how long to import (default 5)")
    ap.add_argument("--latency", default=os.environ.get("LATENCY", ""), help="what the fake adds to every Graph request")
    ap.add_argument("--jitter", default=os.environ.get("JITTER", ""), help="and up to this much more, at random")
    ap.add_argument("--timeout", type=float, default=90, help="give up on a run after this many minutes")
    ap.add_argument("--out", default=os.environ.get("OUT", os.path.join(ROOT, "testdata", "loadtest")), help="where the JSON report goes")
    ap.add_argument("--no-finalize", action="store_true", help="do not finalize a complete run")
    ap.add_argument("--keep", action="store_true", help="leave the stack up afterwards")
    ap.add_argument("--self-test", action="store_true", help="check the script's arithmetic and exit; needs no stack")
    args = ap.parse_args()
    if args.self_test:
        self_test()
        return 0
    for name in (args.latency, args.jitter):
        if name:
            parse_duration(name)
    if not os.path.exists(TAUT):
        raise Failure("bin/taut is missing: run `make build`")
    if not shutil.which("docker"):
        raise Failure("docker is not installed")
    os.makedirs(args.out, exist_ok=True)

    workdir = tempfile.mkdtemp(prefix="taut-load-test.")
    reports = []
    try:
        if args.profile == "scaling":
            for workers in (2, 4, 8):
                reports.append(one_run(args, "ceiling", workers, workdir))
                say(render(reports[-1]))
            say(render_scaling(reports))
        else:
            reports.append(one_run(args, args.profile, args.workers, workdir))
            say(render(reports[-1]))
    finally:
        shutil.rmtree(workdir, ignore_errors=True)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    path = os.path.join(args.out, f"{args.profile}-{stamp}.json")
    with open(path, "w") as f:
        json.dump({"at": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "profile": args.profile, "runs": reports}, f, indent=2)
    say()
    say(f"  Report  {path}")
    return 0 if all(r["verification"]["ok"] for r in reports) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Failure as e:
        print(f"\n  ✗ {e}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
