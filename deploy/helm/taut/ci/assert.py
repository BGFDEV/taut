#!/usr/bin/env python3
"""Assert the security properties of a rendered Taut chart.

    helm template taut deploy/helm/taut -f ci/values.yaml | ci/assert.py -

Checks what docs/how-it-works.md ("Security model") promises about a worker pod, against
the YAML that would actually be applied:

  * only the token broker can read the Microsoft and Slack credentials;
  * the worker container has no such credential, in its environment or
    mounted, and does not share the token broker's socket;
  * the egress proxy listens on 127.0.0.1 and holds no secret but its own
    NATS credentials file;
  * no NATS credential is passed through the environment: the URL is a plain
    value without a password, credentials and keys are mounted files, and
    each container mounts only its own identity's credentials;
  * every container has a read-only root filesystem, drops all
    capabilities, cannot escalate, and runs as a non-root user;
  * none of the insecure switches is present unless the render was made
    with dev.insecure (--insecure);
  * no NetworkPolicy egress rule but DNS allows every destination: each
    names its peers, and one that names all of IPv4 leaves out the private,
    link-local (cloud metadata) and carrier-grade NAT ranges.

Exit status 0 when everything holds, 1 with one line per violation.
Needs PyYAML.
"""

import argparse
import re
import sys

import yaml

INSECURE_ENV = {
    "TAUT_CONTROLLER_INSECURE",
    "TAUT_ALLOW_TEST_ENDPOINTS",
    "TAUT_INSECURE_NO_SIDECARS",
    "TAUT_TOKEN_ALLOW_ENDPOINTS",
    "TAUT_ENTRA_AUTHORITY",
    "TAUT_GRAPH_URL",
    "TAUT_SLACK_FILES_URL",
}
# Secrets that must arrive as mounted files (NAME_FILE), never as values.
FILE_ONLY = {
    "TAUT_POSTGRES_URL",
    "TAUT_STAGING_KEY",
    "TAUT_S3_ACCESS_KEY",
    "TAUT_S3_SECRET_KEY",
    "TAUT_CONTROLLER_TOKEN",
}
CREDENTIAL_ENV = re.compile(r"^TAUT_(ENTRA|SLACK|TOKEN)_")

errors = []


def fail(msg):
    errors.append(msg)


def env_of(c):
    return {e["name"]: e for e in c.get("env") or []}


def mounts_of(c):
    return {m["name"]: m for m in c.get("volumeMounts") or []}


def all_containers(pod):
    return (pod.get("initContainers") or []) + (pod.get("containers") or [])


def secret_refs_of_volume(vol):
    """(secret name, key or None for the whole Secret) pairs a volume exposes."""
    out = set()
    sources = []
    if "secret" in vol:
        s = vol["secret"]
        sources.append({"name": s.get("secretName"), "items": s.get("items")})
    for src in (vol.get("projected") or {}).get("sources") or []:
        if "secret" in src:
            sources.append(src["secret"])
    for s in sources:
        items = s.get("items")
        if not items:
            out.add((s.get("name"), None))
        for it in items or []:
            out.add((s.get("name"), it["key"]))
    return out


def secret_refs_of_env(c):
    out = set()
    for e in c.get("env") or []:
        ref = (e.get("valueFrom") or {}).get("secretKeyRef")
        if ref:
            out.add((ref["name"], ref["key"]))
    for ef in c.get("envFrom") or []:
        if "secretRef" in ef:
            out.add((ef["secretRef"]["name"], None))
    return out


def overlaps(refs, credentials):
    """True when refs can read any (name, key) in credentials."""
    names = {n for n, _ in credentials}
    return any((n, k) in credentials or (k is None and n in names) for n, k in refs)


def check_hardening(where, pod):
    psc = pod.get("securityContext") or {}
    if pod.get("automountServiceAccountToken") is not False:
        fail(f"{where}: automountServiceAccountToken is not false")
    for c in all_containers(pod):
        name = f"{where}/{c['name']}"
        sc = c.get("securityContext") or {}
        if sc.get("readOnlyRootFilesystem") is not True:
            fail(f"{name}: readOnlyRootFilesystem is not true")
        if (sc.get("capabilities") or {}).get("drop") != ["ALL"]:
            fail(f"{name}: does not drop ALL capabilities")
        if (sc.get("capabilities") or {}).get("add"):
            fail(f"{name}: adds capabilities")
        if sc.get("allowPrivilegeEscalation") is not False:
            fail(f"{name}: allowPrivilegeEscalation is not false")
        if sc.get("privileged"):
            fail(f"{name}: privileged")
        if not (sc.get("runAsNonRoot") or psc.get("runAsNonRoot")):
            fail(f"{name}: runAsNonRoot is not set")
        if sc.get("runAsUser", psc.get("runAsUser")) in (0, None):
            fail(f"{name}: no non-zero runAsUser")
        seccomp = (sc.get("seccompProfile") or psc.get("seccompProfile") or {}).get("type")
        if seccomp != "RuntimeDefault":
            fail(f"{name}: seccomp profile is {seccomp}, not RuntimeDefault")
        res = c.get("resources") or {}
        if not res.get("requests") or not res.get("limits"):
            fail(f"{name}: resource requests and limits are not both set")
        for var in env_of(c):
            if var in FILE_ONLY:
                fail(f"{name}: {var} is passed in the environment; it must be a mounted file ({var}_FILE)")


def check_worker_pod(pod, args):
    where = "worker"
    vols = {v["name"]: v for v in pod.get("volumes") or []}
    init = {c["name"]: c for c in pod.get("initContainers") or []}
    main = {c["name"]: c for c in pod.get("containers") or []}
    by_name = {**init, **main}

    if "worker" not in main:
        fail("worker: no container named worker")
        return
    worker = main["worker"]

    scratch = (vols.get("scratch") or {}).get("emptyDir")
    if scratch is None or not scratch.get("sizeLimit"):
        fail("worker: the scratch volume is not an emptyDir with a sizeLimit")
    if "scratch" not in mounts_of(worker):
        fail("worker/worker: scratch is not mounted")
    if not pod.get("terminationGracePeriodSeconds", 0) >= 30:
        fail("worker: terminationGracePeriodSeconds is under 30")

    credentials = secret_refs_of_volume(vols.get("credentials") or {})
    if not credentials:
        fail("worker: no credentials volume, or it references no Secret")

    if args.mode == "nosidecars":
        if not args.insecure:
            fail("worker: rendered without sidecars but not with dev.insecure")
        if set(by_name) != {"worker"}:
            fail(f"worker: expected only the worker container, found {sorted(by_name)}")
        return

    # --- the three containers and how they are run ---------------------------
    for sidecar in ("token-broker", "egress-proxy"):
        if args.mode == "native":
            c = init.get(sidecar)
            if c is None:
                fail(f"worker: {sidecar} is not an init container (native sidecar)")
            elif c.get("restartPolicy") != "Always":
                fail(f"worker/{sidecar}: restartPolicy is not Always")
        else:
            c = main.get(sidecar)
            if c is None:
                fail(f"worker: {sidecar} is not a container (legacySidecars)")
            elif "restartPolicy" in c:
                fail(f"worker/{sidecar}: restartPolicy set on an ordinary container")
    if set(by_name) != {"worker", "egress-proxy", "token-broker"}:
        fail(f"worker: unexpected containers {sorted(by_name)}")
        return
    if args.mode == "native" and list(init) != ["token-broker", "egress-proxy"]:
        fail("worker: sidecars must start in the order token-broker, egress-proxy")
    broker, proxy = by_name["token-broker"], by_name["egress-proxy"]

    # --- only the token broker can read the credentials ----------------------
    for name, c in by_name.items():
        m = mounts_of(c)
        if name == "token-broker":
            if "credentials" not in m:
                fail("worker/token-broker: does not mount the credentials")
            elif m["credentials"].get("readOnly") is not True:
                fail("worker/token-broker: credentials are not mounted read-only")
            continue
        if "credentials" in m:
            fail(f"worker/{name}: mounts the credentials volume")
        if overlaps(secret_refs_of_env(c), credentials):
            fail(f"worker/{name}: environment references a credential Secret")
        for vname in m:
            if vname != "credentials" and overlaps(secret_refs_of_volume(vols.get(vname) or {}), credentials):
                fail(f"worker/{name}: volume {vname} exposes a credential Secret")

    # --- the worker ----------------------------------------------------------
    wenv = env_of(worker)
    for var in wenv:
        if CREDENTIAL_ENV.match(var) and not (args.insecure and var in INSECURE_ENV):
            fail(f"worker/worker: has {var}")
    if "sockets" in mounts_of(worker):
        fail("worker/worker: mounts the token broker's socket volume")
    for vname, m in mounts_of(worker).items():
        if vname not in ("backing", "scratch", "nats-worker"):
            fail(f"worker/worker: unexpected volume mount {vname}")
    proxy_url = (wenv.get("TAUT_PROXY_URL") or {}).get("value", "")
    if not re.fullmatch(r"http://127\.0\.0\.1:\d+", proxy_url):
        fail(f"worker/worker: TAUT_PROXY_URL is {proxy_url!r}, not the loopback proxy")
    if "TAUT_INSECURE_NO_SIDECARS" in wenv:
        fail("worker/worker: TAUT_INSECURE_NO_SIDECARS is set although sidecars are present")

    # --- the egress proxy ----------------------------------------------------
    penv = env_of(proxy)
    addr = (penv.get("TAUT_PROXY_ADDR") or {}).get("value", "")
    if not re.fullmatch(r"127\.0\.0\.1:\d+", addr):
        fail(f"worker/egress-proxy: TAUT_PROXY_ADDR is {addr!r}; it must bind 127.0.0.1")
    elif proxy_url and proxy_url != "http://" + addr:
        fail("worker/egress-proxy: TAUT_PROXY_ADDR and the worker's TAUT_PROXY_URL differ")
    if proxy.get("ports"):
        fail("worker/egress-proxy: declares container ports")
    if "sockets" not in mounts_of(proxy) or not set(mounts_of(proxy)) <= {"sockets", "nats-egress-proxy"}:
        fail(f"worker/egress-proxy: mounts {sorted(mounts_of(proxy))}, expected sockets and at most its NATS files")
    for var, e in penv.items():
        if "valueFrom" in e and not var.startswith("OTEL_"):
            fail(f"worker/egress-proxy: {var} comes from a Secret or field reference")
    if "TAUT_NATS_URL" not in penv:
        fail("worker/egress-proxy: TAUT_NATS_URL is not set; budgets would not be shared")
    for var in ("TAUT_BUDGET_GRAPH_RPS", "TAUT_BUDGET_SHAREPOINT_RPS", "TAUT_BUDGET_SLACK_RPS"):
        if var not in penv:
            fail(f"worker/egress-proxy: {var} is not set")

    # --- the token broker ----------------------------------------------------
    benv = env_of(broker)
    for var in ("TAUT_TOKEN_TENANTS", "TAUT_TOKEN_CLIENTS", "TAUT_TOKEN_CLOUDS"):
        if not (benv.get(var) or {}).get("value"):
            fail(f"worker/token-broker: {var} is empty; the broker is not pinned")
    if (benv.get("TAUT_TOKEN_SOCKET") or {}).get("value") != (penv.get("TAUT_TOKEN_SOCKET") or {}).get("value"):
        fail("worker: the token broker and the egress proxy disagree on TAUT_TOKEN_SOCKET")
    if set(mounts_of(broker)) != {"credentials", "sockets"}:
        fail(f"worker/token-broker: mounts {sorted(mounts_of(broker))}")
    for name, c in (("token-broker", broker), ("egress-proxy", proxy)):
        for p in ("startupProbe", "livenessProbe"):
            cmd = ((c.get(p) or {}).get("exec") or {}).get("command") or []
            if cmd[:3] != ["/usr/local/bin/taut", "dev", "healthcheck"]:
                fail(f"worker/{name}: no exec {p} using the binary's health check")
    for p in ("startupProbe", "livenessProbe"):
        if ((worker.get(p) or {}).get("httpGet") or {}).get("path") != "/healthz":
            fail(f"worker/worker: no httpGet {p} on /healthz")
    if "TAUT_WORKER_HEALTH_ADDR" not in wenv:
        fail("worker/worker: TAUT_WORKER_HEALTH_ADDR is not set")
    sockets = (vols.get("sockets") or {}).get("emptyDir")
    if sockets is None or sockets.get("medium") != "Memory":
        fail("worker: the sockets volume is not an in-memory emptyDir")
    return credentials


NATS_DIR = "/run/secrets/taut-nats"


def check_nats(pods, args):
    """No NATS credential in any environment; one identity per container."""
    creds = {}  # container -> (secret, key) of its credentials file
    for where, pod in pods.items():
        vols = {v["name"]: v for v in pod.get("volumes") or []}
        for c in all_containers(pod):
            name = f"{where}/{c['name']}"
            env = env_of(c)
            for var, e in env.items():
                if not var.startswith("TAUT_NATS_"):
                    continue
                if "valueFrom" in e:
                    fail(f"{name}: {var} comes from a Secret; NATS credentials must be mounted files")
                if var == "TAUT_NATS_URL" and "@" in str(e.get("value", "")):
                    fail(f"{name}: TAUT_NATS_URL carries credentials")
                if var.endswith("_FILE") and not str(e.get("value", "")).startswith(NATS_DIR + "/"):
                    fail(f"{name}: {var} does not point into {NATS_DIR}")
            if not args.insecure or c["name"] != "worker":
                for var, e in env.items():
                    ref = (e.get("valueFrom") or {}).get("secretKeyRef")
                    broker_env = c["name"] == "token-broker" and var in ("TAUT_ENTRA_CERTIFICATE", "TAUT_ENTRA_CLIENT_SECRET", "TAUT_SLACK_TOKEN")
                    if ref and not broker_env and not var.startswith("OTEL_"):
                        fail(f"{name}: {var} is a Secret passed through the environment")
            for vname, m in mounts_of(c).items():
                if m.get("mountPath") != NATS_DIR:
                    continue
                if c["name"] == "token-broker":
                    fail(f"{name}: mounts NATS files; the token broker does not use NATS")
                if m.get("readOnly") is not True:
                    fail(f"{name}: NATS files are not mounted read-only")
                mine = []
                for src in (vols.get(vname, {}).get("projected") or {}).get("sources") or []:
                    sec = src.get("secret") or {}
                    if not sec.get("items"):
                        fail(f"{name}: volume {vname} exposes a whole Secret")
                    mine += [(sec.get("name"), it["key"]) for it in sec.get("items") or [] if it["path"] == "nats.creds"]
                if len(mine) > 1:
                    fail(f"{name}: mounts more than one NATS credentials file")
                if mine:
                    creds[name] = mine[0]
            if "TAUT_NATS_CREDS_FILE" in env and name not in creds:
                fail(f"{name}: TAUT_NATS_CREDS_FILE is set but no credentials file is mounted")
    if len(set(creds.values())) != len(creds):
        fail(f"NATS identities are shared between containers: {creds}")


def check_controller_pod(dep, pod, args, credentials):
    spec = dep["spec"]
    if spec.get("replicas") != 1:
        fail(f"controller: replicas is {spec.get('replicas')}, not 1")
    if (spec.get("strategy") or {}).get("type") != "Recreate":
        fail("controller: strategy is not Recreate")
    vols = {v["name"]: v for v in pod.get("volumes") or []}
    for c in all_containers(pod):
        env = env_of(c)
        for var in env:
            if CREDENTIAL_ENV.match(var) or var.startswith("TAUT_PROXY_"):
                fail(f"controller/{c['name']}: has {var}")
        if not args.insecure and "TAUT_CONTROLLER_TOKEN_FILE" not in env:
            fail("controller: TAUT_CONTROLLER_TOKEN_FILE is not set")
        if credentials:
            refs = set(secret_refs_of_env(c))
            for vname in mounts_of(c):
                refs |= secret_refs_of_volume(vols.get(vname) or {})
            if overlaps(refs, credentials):
                fail(f"controller/{c['name']}: can read a Microsoft or Slack credential")
        for p in ("startupProbe", "livenessProbe", "readinessProbe"):
            if p not in c:
                fail(f"controller/{c['name']}: no {p}")


# What an egress rule that allows "the internet" must leave out.
NOT_PUBLIC = {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}


def check_egress(name, policy):
    """No rule but DNS may allow every destination, private ranges included."""
    for i, rule in enumerate(policy["spec"].get("egress") or []):
        ports = {p.get("port") for p in rule.get("ports") or []}
        where = f"NetworkPolicy {name}: egress rule {i} (ports {sorted(ports, key=str)})"
        if not rule.get("ports"):
            fail(f"{where} allows every port")
        peers = rule.get("to")
        if not peers:
            if ports != {53}:
                fail(f"{where} has no `to`: it allows every destination")
            continue
        for peer in peers:
            if not peer:
                fail(f"{where} has an empty peer: it allows every destination")
                continue
            block = peer.get("ipBlock")
            if not block:
                continue
            cidr = block.get("cidr", "")
            if cidr.endswith("/0"):
                if ":" in cidr:
                    fail(f"{where} allows all of IPv6")
                elif not NOT_PUBLIC <= set(block.get("except") or []):
                    missing = sorted(NOT_PUBLIC - set(block.get("except") or []))
                    fail(f"{where} allows {cidr} without leaving out {', '.join(missing)}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file", help="rendered chart output, or - for stdin")
    ap.add_argument("--mode", choices=["native", "legacy", "nosidecars"], default="native")
    ap.add_argument("--insecure", action="store_true", help="the render was made with dev.insecure=true")
    ap.add_argument("--no-network-policy", action="store_true", help="the render was made with networkPolicy.enabled=false")
    args = ap.parse_args()

    stream = sys.stdin if args.file == "-" else open(args.file)
    docs = [d for d in yaml.safe_load_all(stream) if d]
    kinds = {}
    for d in docs:
        kinds.setdefault(d["kind"], []).append(d)

    deployments = {d["metadata"]["labels"].get("app.kubernetes.io/component"): d for d in kinds.get("Deployment", [])}
    if set(deployments) != {"controller", "worker"}:
        fail(f"expected a controller and a worker Deployment, found {sorted(map(str, deployments))}")
        return report()

    wpod = deployments["worker"]["spec"]["template"]["spec"]
    cpod = deployments["controller"]["spec"]["template"]["spec"]
    check_hardening("worker", wpod)
    check_hardening("controller", cpod)
    credentials = check_worker_pod(wpod, args) or set()
    check_controller_pod(deployments["controller"], cpod, args, credentials)
    check_nats({"worker": wpod, "controller": cpod}, args)

    if not args.insecure:
        for comp, d in deployments.items():
            for c in all_containers(d["spec"]["template"]["spec"]):
                for var in env_of(c):
                    if var in INSECURE_ENV:
                        fail(f"{comp}/{c['name']}: {var} is set without dev.insecure")
        if kinds.get("Secret"):
            fail("a Secret was rendered from plaintext values without dev.insecure")

    for sa in kinds.get("ServiceAccount", []):
        if sa.get("automountServiceAccountToken") is not False:
            fail(f"ServiceAccount {sa['metadata']['name']}: token automount is not off")

    policies = {p["metadata"]["name"]: p for p in kinds.get("NetworkPolicy", [])}
    if args.no_network_policy:
        if policies:
            fail("NetworkPolicies rendered although networkPolicy.enabled=false")
    else:
        deny = [p for n, p in policies.items() if n.endswith("-default-deny")]
        if not deny:
            fail("no default-deny NetworkPolicy")
        else:
            s = deny[0]["spec"]
            if s.get("ingress") or s.get("egress") or set(s.get("policyTypes") or []) != {"Ingress", "Egress"}:
                fail("the default-deny NetworkPolicy allows something")
        worker = [p for n, p in policies.items() if n.endswith("-worker")]
        if not worker:
            fail("no NetworkPolicy for the worker pods")
        elif worker[0]["spec"].get("ingress"):
            fail("the worker NetworkPolicy allows ingress")
        for n, p in policies.items():
            check_egress(n, p)
        ctl = [p for n, p in policies.items() if n.endswith("-controller")]
        if not ctl:
            fail("no NetworkPolicy for the controller")
        else:
            for rule in ctl[0]["spec"].get("ingress") or []:
                if not rule.get("from"):
                    fail("the controller NetworkPolicy allows ingress from anywhere")
    return report()


def report():
    if errors:
        for e in errors:
            print("FAIL " + e, file=sys.stderr)
        return 1
    print("ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
