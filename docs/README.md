# Taut documentation

Start with the [project README](../README.md) for what Taut is and how to
try it locally. These pages go further.

> [!WARNING]
> Taut has not been run against a real Microsoft tenant. It is verified
> against fake Microsoft and Slack services that ship in this repository.
> [Known limitations](migration-guide.md#known-limitations) lists what
> that leaves open.

## What to read

| If you want to | Read |
|---|---|
| Understand the parts, the pipeline and why resume is safe | [How Taut works](how-it-works.md) |
| Plan and run a migration, step by step | [Migration guide](migration-guide.md) |
| Look up a command, a flag, an exit code or an environment variable | [Command reference](cli.md) |
| Write or review a `taut.yaml` | [Configuration](configuration.md) |
| Deploy on Kubernetes with the Helm chart | [Operations](OPERATIONS.md) |
| Know how fast it goes and what limits it | [Load test](LOADTEST.md) |
| Know why it is built this way | [Design notes](design-notes.md) |
| Contribute a change | [CONTRIBUTING.md](../CONTRIBUTING.md) |
| Report a security problem | [SECURITY.md](../SECURITY.md) |

## Common questions

| Question | Answer |
|---|---|
| Which Graph permissions and Slack scopes does it need? | [Permissions](migration-guide.md#permissions) |
| What does Teams not import, and what happens instead? | [What Teams cannot import](how-it-works.md#what-teams-cannot-import) |
| What happens if a worker dies mid-run? | [Resuming without duplicates](how-it-works.md#resuming-without-duplicates) |
| Where do credentials live? | [Security model](how-it-works.md#security-model) |
| `modern` or `legacy`? | [Load strategies](how-it-works.md#load-strategies) |
| Should direct messages be turned on? | [Direct messages](migration-guide.md#direct-messages) |
| People are still using Slack. Can I migrate now and catch up later? | [Migrating in passes](migration-guide.md#migrating-in-passes) |
| Do I need a cluster for a small workspace? | No: [Local mode](migration-guide.md#local-mode) |
| How long will it take? | [Sizing](OPERATIONS.md#5-sizing) and the [load test results](LOADTEST.md#results) |
| What can a script rely on? | [Output modes](cli.md#output-modes) and [exit codes](cli.md#exit-codes) |
