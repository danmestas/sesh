#!/usr/bin/env bash
#
# run.sh — host entry point for the integration rig.
#
# Stages credentials, builds the image, runs the container, surfaces logs,
# returns the harness's exit code. Safe to re-run; idempotent.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
RIG_DIR="${REPO_ROOT}/test/integration"
cd "${RIG_DIR}"

echo "==> stage credentials"
bash scripts/stage-creds.sh

echo
echo "==> docker compose up --build (will block until harness exits)"
# --abort-on-container-exit makes compose return the harness's exit code.
# --build forces rebuild so source changes pick up.
docker compose up --build --abort-on-container-exit --exit-code-from sesh-integ
RC=$?

echo
echo "==> artifacts:"
ls -la artifacts/ 2>/dev/null || echo "(no artifacts dir)"

echo
echo "==> harness exit code: ${RC}"
exit ${RC}
