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

# The local-mode tests start a PostgreSQL server of their own. They use one
# that is installed on this machine. Where none is, this lets them download
# one from Maven Central into the user cache directory, as
# local.postgres.download does; set it to 0 to have those tests skip instead.
export TAUT_TEST_LOCAL_DOWNLOAD="${TAUT_TEST_LOCAL_DOWNLOAD:-1}"

# TAUT_TEST_PACKAGES narrows the run to some packages; CI uses it to give the
# slow kill-and-resume packages a runner each. The timeout is well past go
# test's ten-minute default, which the worker package exceeds under the race
# detector.
# shellcheck disable=SC2086
exec go test -race -timeout 60m "$@" ${TAUT_TEST_PACKAGES:-./...}
