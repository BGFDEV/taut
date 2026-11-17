#!/usr/bin/env bash
#
# Run every test, including the ones that need Postgres, S3 and NATS.
# Start those first with `make test-services`.

set -euo pipefail

export TAUT_TEST_DATABASE_URL='postgres://taut:taut-test@127.0.0.1:55432/taut_test?sslmode=disable'
export TAUT_TEST_S3_ENDPOINT='http://127.0.0.1:59000'
export TAUT_TEST_S3_ACCESS_KEY='taut-test'
export TAUT_TEST_S3_SECRET_KEY='taut-test-secret'
export TAUT_TEST_NATS_URL='nats://127.0.0.1:54222'

# The worker package's kill-and-resume tests take about eight minutes under
# the race detector on a small CI runner, past go test's ten-minute default.
exec go test -race -timeout 30m "$@" ./...
