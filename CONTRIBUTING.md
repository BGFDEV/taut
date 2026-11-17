# Contributing to Taut

Thanks for wanting to help. Taut moves a company's whole message history from
Slack into Microsoft Teams, so the bar is correctness first: a change that is
fast or pretty but can lose or duplicate a message will not be merged.

## Ways to help

- **Run it against a real tenant and tell us what happened.** Everything is
  verified against fake Microsoft and Slack services; reports from real
  tenants (a test tenant is ideal) are the most valuable contribution right
  now. Open an issue with the "Tenant report" template.
- **Report a bug** with the steps, the command output and `taut doctor`.
- **Fix something.** Issues labelled `good first issue` are small and
  self-contained.
- **Improve the docs**, especially where an error message or a guide left you
  without a next step.

For anything larger than a bug fix, open an issue first so we can agree on the
approach before you spend time on it.

## Never put real data in an issue or a pull request

Do not attach Slack exports, message content, tenant IDs, user names, email
addresses, tokens or certificates. Redact them, or reproduce the problem with
the generated fixtures (`make fixtures`). If a report needs real data to make
sense, describe its shape instead.

Security problems do not go in public issues. See [SECURITY.md](SECURITY.md).

## Setting up

You need Go and Docker. The Go toolchain named in `go.mod` is fetched
automatically.

```bash
make build
```

```bash
make test
```

`make test` runs the unit tests with the race detector and needs nothing else.
The full suite, including the tests that kill workers mid-import, needs
Postgres, S3-compatible storage and NATS, which run in containers:

```bash
make test-services
```

```bash
make test-all
```

The local-mode tests start a PostgreSQL server of their own. They use one
installed on your machine and are skipped by `make test` when there is
none. `make test-all` lets them download one from Maven Central instead;
set `TAUT_TEST_LOCAL_DOWNLOAD=0` to have them skipped there too.

To see a whole migration run locally against the fakes:

```bash
make up
```

```bash
make migrate-demo
```

`make help` lists every target.

## Before you open a pull request

```bash
make fmt
```

```bash
make lint
```

```bash
make test-all
```

If you touched the Helm chart, also run `make helm-lint`. If you changed what
the CLI or the terminal UI prints on purpose, run `make golden` and include the
updated snapshot files in the same commit so the reviewer can see the change.

## What we look for in a change

- **Small commits that each do one thing**, with a message that says what
  changed and why. The subject line follows the existing style:
  `feat(worker): ...`, `fix(cli): ...`, `docs: ...`, `test(store): ...`.
- **A test that fails without your change.** Anything touching the import path
  (the worker, the store, the Graph client) needs a test that survives a
  worker being killed, because that is the promise the product makes. The
  existing chaos tests in `internal/worker` show how.
- **The fake matches the documentation.** If you teach the Graph client
  something new, teach `internal/fakegraph` the same behaviour and cite the
  Microsoft documentation you relied on.
- **A design note for a decision.** If the change picks one design over
  another, add a note to [docs/design-notes.md](docs/design-notes.md) in the
  same style as the others: the constraint, the choice and what it costs.
- **Errors that help.** Every user-facing error says what happened, why, and
  the exact next step.
- **No secrets and no message content** in logs, traces, metrics, the config
  file or test fixtures.
- **Plain names.** Use the terms an engineer running a migration would use
  (controller, worker, finalize), not invented ones.

## Licence of contributions

Taut is licensed under the [Apache License 2.0](LICENSE). By submitting a
contribution you agree that it is licensed under the same terms (section 5 of
the licence). There is no separate contributor agreement to sign.

Please sign off your commits (`git commit -s`) to certify that you wrote the
change or have the right to submit it, as described by the
[Developer Certificate of Origin](https://developercertificate.org/).

## Conduct

This project follows a [code of conduct](CODE_OF_CONDUCT.md). Be kind and
assume good faith.
