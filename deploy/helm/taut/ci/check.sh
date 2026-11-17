#!/usr/bin/env bash
#
# Lint the chart, render it under every supported combination of values,
# validate the output against the Kubernetes schemas, assert the worker
# pod's security properties (assert.py), and check that unusable values
# stop the render with a message that says what to do.
#
#   deploy/helm/taut/ci/check.sh
#
# Needs helm, python3 with PyYAML, and Go (to run kubeconform). Set
# KUBECONFORM=skip to leave the schema validation out, for example offline.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)"
CI="$CHART/ci"
KUBE_VERSION="${KUBE_VERSION:-1.31.0}"
KUBECONFORM="${KUBECONFORM:-go run github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0}"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

failed=0

conform() {
  if [[ "$KUBECONFORM" == skip ]]; then return 0; fi
  # CiliumNetworkPolicy is a CRD with no schema in the default catalogue.
  $KUBECONFORM -strict -summary -kubernetes-version "$1" -skip CiliumNetworkPolicy "$2"
}

# render <name> <kube-version> <assert.py flags> -- <helm args>
render() {
  local name="$1" kube="$2" flags="$3"
  shift 4
  echo "== $name"
  if ! helm lint "$CHART" --kube-version "$kube" "$@" >"$OUT/$name.lint" 2>&1; then
    cat "$OUT/$name.lint"; echo "FAIL $name: helm lint"; failed=1; return
  fi
  if ! helm template taut "$CHART" --namespace taut --kube-version "$kube" "$@" >"$OUT/$name.yaml" 2>"$OUT/$name.err"; then
    cat "$OUT/$name.err"; echo "FAIL $name: helm template"; failed=1; return
  fi
  # shellcheck disable=SC2086
  if ! python3 "$CI/assert.py" $flags "$OUT/$name.yaml"; then
    echo "FAIL $name: assertions"; failed=1
  fi
  if ! conform "$kube" "$OUT/$name.yaml"; then
    echo "FAIL $name: kubeconform"; failed=1
  fi
}

# refuse <name> <expected text> <helm args>: the render must fail and say why.
refuse() {
  local name="$1" want="$2"
  shift 2
  echo "== refuses: $name"
  if helm template taut "$CHART" --namespace taut --kube-version "$KUBE_VERSION" "$@" >/dev/null 2>"$OUT/refuse.err"; then
    echo "FAIL $name: rendered, but should have been refused"; failed=1; return
  fi
  if ! grep -qF -- "$want" "$OUT/refuse.err"; then
    echo "FAIL $name: refused, but without the expected message \"$want\":"; cat "$OUT/refuse.err"; failed=1
  fi
}

base=(-f "$CI/values.yaml")

render certificate      "$KUBE_VERSION" ""                              -- "${base[@]}"
render client-secret    "$KUBE_VERSION" ""                              -- "${base[@]}" -f "$CI/client-secret.yaml"
render networkpolicy-off "$KUBE_VERSION" "--no-network-policy"          -- "${base[@]}" -f "$CI/networkpolicy-off.yaml"
render networkpolicy-pinned "$KUBE_VERSION" ""                          -- "${base[@]}" -f "$CI/networkpolicy-pinned.yaml"
render ingress          "$KUBE_VERSION" ""                              -- "${base[@]}" -f "$CI/ingress.yaml"
render hpa              "$KUBE_VERSION" ""                              -- "${base[@]}" -f "$CI/hpa.yaml"
render full             "$KUBE_VERSION" ""                              -- "${base[@]}" -f "$CI/full.yaml"
render legacy-sidecars  1.28.0          "--mode legacy"                 -- "${base[@]}" -f "$CI/legacy-sidecars.yaml"
render dev-insecure     "$KUBE_VERSION" "--insecure"                    -- "${base[@]}" -f "$CI/dev-insecure.yaml"
render dev-nosidecars   "$KUBE_VERSION" "--insecure --mode nosidecars"  -- "${base[@]}" -f "$CI/dev-nosidecars.yaml"
render values-dev       "$KUBE_VERSION" ""                              -- -f "$CHART/values-dev.yaml" \
  --set entra.tenantId=00000000-0000-0000-0000-000000000001 --set entra.clientId=00000000-0000-0000-0000-000000000002

refuse "no values at all"              "is required"
refuse "no client ID"                  "entra.clientId is required"              "${base[@]}" --set entra.clientId=
refuse "no credential Secret"          "entra.credential.existingSecret is required" "${base[@]}" --set entra.credential.existingSecret=
refuse "no Postgres Secret"            "postgres.existingSecret is required"     "${base[@]}" --set postgres.existingSecret=
refuse "no staging key Secret"         "stagingKey.existingSecret is required"   "${base[@]}" --set stagingKey.existingSecret=
refuse "no S3 Secret"                  "s3.existingSecret is required"           "${base[@]}" --set s3.existingSecret=
refuse "no bucket"                     "s3.bucket is required"                   "${base[@]}" --set s3.bucket=
refuse "S3 endpoint with a path"       "with no path and no credentials"         "${base[@]}" --set s3.endpoint=https://s3.example.com/bucket
refuse "no controller token Secret"    "controllerToken.existingSecret is required" "${base[@]}" --set controllerToken.existingSecret=
refuse "no NATS"                       "nats.url is required"                       "${base[@]}" --set nats.url=
refuse "NATS password in a value"      "nats.url contains credentials"           "${base[@]}" --set nats.url=nats://u:p@nats:4222
refuse "unknown cloud"                 "/entra/cloud"                            "${base[@]}" --set entra.cloud=mars
refuse "two controllers"               "/controller/replicas"                    "${base[@]}" --set controller.replicas=2
refuse "NATS client cert without key"  "nats.tls.certKey is set without nats.tls.keyKey" "${base[@]}" --set nats.tls.existingSecret=x --set nats.tls.certKey=tls.crt
refuse "NATS URL via extraEnv"         "worker.extraEnv may not set TAUT_NATS_URL" \
  "${base[@]}" --set 'worker.extraEnv[0].name=TAUT_NATS_URL' --set-string 'worker.extraEnv[0].value=nats://u:p@nats:4222'
refuse "ingress without a host"        "controller.ingress.hosts is empty"       "${base[@]}" --set controller.ingress.enabled=true
refuse "Kubernetes 1.28"               "need 1.29 or later"                      "${base[@]}" --kube-version 1.28.0
refuse "controllerInsecure alone"      "dev.controllerInsecure is set but dev.insecure is not true" "${base[@]}" --set dev.controllerInsecure=true
refuse "allowTestEndpoints alone"      "dev.allowTestEndpoints is set but dev.insecure is not true" "${base[@]}" --set dev.allowTestEndpoints=true
refuse "noSidecars alone"              "dev.noSidecars is set but dev.insecure is not true"         "${base[@]}" --set dev.noSidecars=true
refuse "tokenAllowEndpoints alone"     "dev.tokenAllowEndpoints is set but dev.insecure is not true" "${base[@]}" --set 'dev.tokenAllowEndpoints={http://x:1}'
refuse "graphUrl alone"                "dev.graphUrl is set but dev.insecure is not true"           "${base[@]}" --set dev.graphUrl=http://x:1
refuse "http proxy destination"        "is plain http"                           "${base[@]}" --set 'egressProxy.extraAllow={graph=http://fake:8080}'
refuse "insecure switch via extraEnv (controller)" "controller.extraEnv may not set TAUT_CONTROLLER_INSECURE" \
  "${base[@]}" --set 'controller.extraEnv[0].name=TAUT_CONTROLLER_INSECURE' --set-string 'controller.extraEnv[0].value=1'
refuse "insecure switch via extraEnv (worker)" "worker.extraEnv may not set TAUT_INSECURE_NO_SIDECARS" \
  "${base[@]}" --set 'worker.extraEnv[0].name=TAUT_INSECURE_NO_SIDECARS' --set-string 'worker.extraEnv[0].value=1'
refuse "proxy address via extraEnv"    "egressProxy.extraEnv may not set TAUT_PROXY_ADDR" \
  "${base[@]}" --set 'egressProxy.extraEnv[0].name=TAUT_PROXY_ADDR' --set-string 'egressProxy.extraEnv[0].value=0.0.0.0:8900'
refuse "dev secret without its value"  "secrets.postgresUrl is empty"            "${base[@]}" --set postgres.existingSecret= --set secrets.create=true

# The assertions themselves: a policy with a rule that allows every
# destination must not pass. The chart no longer renders one, so one is made
# by taking the `to` off a rendered rule.
echo "== assert.py refuses an egress rule without peers"
python3 - "$OUT/certificate.yaml" "$OUT/open.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
for d in docs:
    if d["kind"] == "NetworkPolicy" and d["metadata"]["name"].endswith("-worker"):
        for rule in d["spec"]["egress"]:
            if {p["port"] for p in rule["ports"]} == {443}:
                rule.pop("to", None)
yaml.safe_dump_all(docs, open(sys.argv[2], "w"))
PY
if python3 "$CI/assert.py" "$OUT/open.yaml" >"$OUT/open.out" 2>&1 || ! grep -q "allows every destination" "$OUT/open.out"; then
  cat "$OUT/open.out"; echo "FAIL assert.py accepted an egress rule that allows every destination"; failed=1
fi
echo "== assert.py refuses all of IPv4 without the private ranges left out"
python3 - "$OUT/certificate.yaml" "$OUT/private.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
for d in docs:
    if d["kind"] == "NetworkPolicy" and d["metadata"]["name"].endswith("-controller"):
        for rule in d["spec"]["egress"]:
            for peer in rule.get("to") or []:
                (peer.get("ipBlock") or {}).pop("except", None)
yaml.safe_dump_all(docs, open(sys.argv[2], "w"))
PY
if python3 "$CI/assert.py" "$OUT/private.yaml" >"$OUT/private.out" 2>&1 || ! grep -q "without leaving out" "$OUT/private.out"; then
  cat "$OUT/private.out"; echo "FAIL assert.py accepted 0.0.0.0/0 with the private ranges in it"; failed=1
fi

if [[ $failed -ne 0 ]]; then
  echo "chart checks FAILED"
  exit 1
fi
echo "chart checks passed"
