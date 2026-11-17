## What this changes and why

<!-- One or two sentences. Link the issue if there is one. -->

## How it was tested

<!-- Which tests cover it, and what you ran. For import-path changes, say which kill/chaos test exercises it. -->

## Checklist

- [ ] `make lint` and `make test-all` pass
- [ ] A test fails without this change
- [ ] Snapshot files updated (`make golden`) if CLI or terminal output changed on purpose
- [ ] `make helm-lint` passes if the chart changed
- [ ] `docs/design-notes.md` has a note if this picks one design over another
- [ ] README or docs updated if behaviour, flags, permissions or configuration changed
- [ ] No secrets, message content or real identifiers in code, tests, fixtures or logs
