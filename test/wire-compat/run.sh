#!/usr/bin/env bash
# Wire-compat runner for sesh-ref-agent against @synadia-ai/agents.
#
# This script:
#   1. Starts a local nats-server on a random port (if not already at $NATS_URL).
#   2. Builds and starts sesh-ref-agent against that broker.
#   3. Installs node_modules in test/wire-compat/ if needed.
#   4. Runs wire-compat.ts via `tsx`.
#   5. Cleans up.
#
# The runner is intentionally self-contained — no global Go/NATS state is
# required beyond a `nats-server` binary on $PATH and a recent `node`.
# Designed to be invoked from CI or by a human checking the contract.

set -euo pipefail
cd "$(dirname "$0")"

cleanup() {
  if [[ -n "${AGENT_PID:-}" ]]; then kill "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; fi
  if [[ -n "${NATS_PID:-}" ]]; then kill "$NATS_PID" 2>/dev/null || true; wait "$NATS_PID" 2>/dev/null || true; fi
  if [[ -n "${TMPDIR_RUN:-}" ]]; then rm -rf "$TMPDIR_RUN"; fi
}
trap cleanup EXIT

TMPDIR_RUN="$(mktemp -d)"

# 1. Broker. If NATS_URL is already set we assume it's reachable.
if [[ -z "${NATS_URL:-}" ]]; then
  if ! command -v nats-server >/dev/null 2>&1; then
    echo "wire-compat: no nats-server on \$PATH and NATS_URL unset" >&2
    exit 2
  fi
  PORT=$(( (RANDOM % 1000) + 4300 ))
  nats-server -p "$PORT" -a 127.0.0.1 >"$TMPDIR_RUN/nats.log" 2>&1 &
  NATS_PID=$!
  export NATS_URL="nats://127.0.0.1:$PORT"
  # Wait up to 5s for the broker to accept connections.
  for _ in $(seq 1 50); do
    if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then break; fi
    sleep 0.1
  done
fi

# 2. Build and start the agent. Owner is fixed so identity is predictable
# inside the SDK assertions.
(cd ../.. && go build -o "$TMPDIR_RUN/sesh-ref-agent" ./cmd/sesh-ref-agent)
SESH_OWNER=wireci "$TMPDIR_RUN/sesh-ref-agent" --agent=echo >"$TMPDIR_RUN/agent.log" 2>&1 &
AGENT_PID=$!

# Give the agent up to 3s to register on $SRV.PING.agents.
for _ in $(seq 1 30); do
  if command -v nats >/dev/null 2>&1 && \
     nats --server="$NATS_URL" req --timeout=200ms '$SRV.PING.agents' '' 2>/dev/null | grep -q '"name":"agents"'; then
    break
  fi
  sleep 0.1
done

# 3. Wire-compat dependencies — installed once, cached in node_modules.
if [[ ! -d node_modules ]]; then
  echo "wire-compat: installing node deps..."
  npm install --silent --no-audit --no-fund --loglevel=error \
    @nats-io/transport-node @synadia-ai/agents tsx typescript >/dev/null
fi

# 4. Run.
NATS_URL="$NATS_URL" npx --no-install tsx wire-compat.ts "$@"
