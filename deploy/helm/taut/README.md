# Taut Helm chart

Runs Taut on Kubernetes: one controller and a Deployment of workers, each
worker pod holding the worker, its egress proxy and its token broker.
Postgres, NATS and an S3-compatible bucket are external; the chart takes
their connection settings and bundles none of them.

The operator guide, including the commands that create the Secrets, is
[docs/OPERATIONS.md](../../../docs/OPERATIONS.md).

## Requirements

- Kubernetes 1.29 or later (native sidecars). On 1.25 to 1.28 set
  `legacySidecars: true`.
- Helm (the chart is checked with Helm 4.3).
- Postgres, NATS (core NATS; JetStream is not used) and an S3 bucket. The
  Compose stack and the tests use Postgres 17 and NATS 2.15.
- A CNI that enforces NetworkPolicy, if the policies are to mean anything.

## Install

```bash
helm install taut deploy/helm/taut --namespace taut --create-namespace -f my-values.yaml
```

The smallest working `my-values.yaml` is [ci/values.yaml](ci/values.yaml).

## Values an operator must set

| Value | What |
|---|---|
| `entra.tenantId`, `entra.clientId` | The tenant and application the token broker is pinned to |
| `entra.credential.existingSecret` | Secret with the certificate (`certificate.pem`) or, with `entra.credential.type: clientSecret`, the client secret (`client-secret`) |
| `postgres.existingSecret` | Secret with the connection URL (`url`) |
| `nats.url` | The NATS server URL, without credentials |
| `s3.bucket`, `s3.existingSecret` | Bucket, and a Secret with `access-key` and `secret-key`; `s3.endpoint` unless it is AWS S3 |
| `stagingKey.existingSecret` | Secret with the staging key (`key`) |
| `controllerToken.existingSecret` | Secret with the API token (`token`) |

Optional but usual: `nats.credentials.existingSecret` (one NATS credentials
file per identity: `controller.creds`, `worker.creds`, `egress-proxy.creds`),
`nats.tls.existingSecret` (CA bundle, client certificate),
`slack.token.existingSecret` (when the config file enables the Slack API), `entra.cloud`, `budgets.*`, `worker.replicas`,
`worker.scratch.sizeLimit`, `telemetry.endpoint`,
`networkPolicy.controllerIngressFrom`, and the `to` lists under
`networkPolicy` that say where each dependency is. They are optional only
for dependencies on public addresses: with an empty `to` a dependency inside
the cluster or on a private network is not reachable.

Rendering fails, naming the value, when a required one is missing. Every
value is described in [values.yaml](values.yaml) and checked by
[values.schema.json](values.schema.json).

## What each container holds

| Container | Secrets it can read | How |
|---|---|---|
| controller | Postgres URL, staging key, S3 keys, API token; its NATS credentials | files under `/run/secrets/taut-backing` and `/run/secrets/taut-nats` |
| worker | Postgres URL, staging key, S3 keys; its NATS credentials | files under `/run/secrets/taut-backing` and `/run/secrets/taut-nats` |
| egress-proxy | its NATS credentials | files under `/run/secrets/taut-nats` |
| token-broker | Entra certificate or client secret, Slack token | files under `/run/secrets/taut` |

By default no secret is passed through the environment
(`tokenBroker.credentialEnv` opts in for the token broker). Each container that uses NATS
mounts its own credentials file and no other.

The worker container mounts neither the credentials nor the volume holding
the token broker's socket. The egress proxy listens on `127.0.0.1` only.
`ci/assert.py` checks these properties on the rendered output.

## Probes

| Container | Probe |
|---|---|
| controller | `GET /healthz` (startup, liveness), `GET /readyz` (readiness: Postgres, NATS and the bucket accept connections) |
| worker | `GET /healthz` on `worker.healthPort` (startup, liveness) |
| egress-proxy | exec `taut dev healthcheck http://127.0.0.1:8900/_proxy/healthz` |
| token-broker | exec `taut dev healthcheck unix:///run/taut/token/token.sock:/healthz` |

The kubelet's HTTP probes come from the pod's own node, which NetworkPolicy
does not filter, so the worker policy allows no ingress.

## Network policy

On by default: a default-deny policy for the release's pods, then one policy
each for the controller and the workers. A NetworkPolicy applies to a pod,
not to a container, so it cannot tell the worker container from the egress
proxy beside it. The separation inside the pod comes from which container
can read the credentials and from the egress proxy's own destination list.
Limiting the worker pods to named hosts needs a CNI extension; with Cilium,
set `networkPolicy.cilium.enabled: true`.

## Telemetry

Set `telemetry.endpoint` to an OTLP/gRPC collector. Taut pushes; it has no
metrics endpoint to scrape, so there is no ServiceMonitor.

## Development toggles

`dev.*` and `secrets.create` exist for throwaway stacks built on fake
Microsoft and Slack services. The `dev.*` toggles do nothing, and fail the
render, unless `dev.insecure: true` is also set; the release notes then
carry a warning. The same switches cannot be passed through `extraEnv`.

## Checking the chart

```bash
make helm-lint
```

lints the chart, renders it under every value set in `ci/`, validates the
result with kubeconform, runs `ci/assert.py`, and checks that unusable values
are refused with a clear message.
