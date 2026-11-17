# Security policy

Taut handles private messages and holds tenant-wide Microsoft Graph
permissions while it runs, so we treat security reports as the highest
priority work in the project.

## Reporting a vulnerability

**Do not open a public issue, discussion or pull request for a security
problem.**

Report it privately through GitHub: on the repository page, open the
**Security** tab and choose **Report a vulnerability**. This creates a private
advisory that only the maintainers can see.

Please include:

- what an attacker can do, and what they need to already have to do it;
- the version or commit you tested;
- steps to reproduce, ideally against the local stack (`make up`), which uses
  fake Microsoft and Slack services;
- any suggested fix.

Never include real tokens, certificates, tenant IDs or message content in a
report. If a real secret was exposed while you were testing, rotate it first.

## What to expect

- An acknowledgement within 5 working days.
- An assessment and a plan, or an explanation of why we do not consider it a
  vulnerability, within 15 working days.
- A fix released before the details are made public, with credit to you in the
  advisory unless you prefer otherwise.

This is a volunteer-maintained project; these are targets we intend to meet,
not a contractual commitment.

## Supported versions

Until the first tagged release, only the `main` branch receives fixes. After
that, the latest minor release does.

## What counts

In scope, for example:

- a token, certificate or other secret reaching a log, a trace, a metric, the
  audit log, the config file or command output;
- message content or participant details reaching somewhere an operator does
  not need them;
- a message or a direct-message conversation being imported somewhere with a
  wider audience than it had in Slack;
- a worker reaching a host or a Graph endpoint that the egress proxy should
  have refused, or obtaining a token it should not hold;
- staging data left unencrypted, or not purged after `taut finalize` without
  `--retain`;
- tampering with the audit log that `taut audit --verify` does not detect;
- untrusted text from a Slack export changing what the terminal displays.

Out of scope:

- the fake services (`internal/fakegraph`, `internal/fakeslack`) and the local
  development stack, which use throwaway credentials on purpose;
- running with `TAUT_INSECURE_NO_SIDECARS`, which is a development switch the
  Helm chart refuses;
- problems that require an attacker who already controls the Kubernetes
  cluster, the database or the Entra app registration.

## How Taut is meant to be deployed

The security model, the least-privilege permission set and the hardening
choices are described in the [README](README.md),
[docs/how-it-works.md](docs/how-it-works.md#security-model) and
[docs/OPERATIONS.md](docs/OPERATIONS.md). If the code does not do what those
documents say, that is a bug worth reporting here.
