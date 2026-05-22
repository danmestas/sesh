#!/usr/bin/env bash
#
# entrypoint.sh — runs inside the container as PID 2 under tini.
#
# Sequence:
#   1. Verify the cred mounts the operator's stage-creds.sh staged.
#   2. Prelude:
#        - extract Claude OAuth token from credentials.json
#        - pre-accept Claude's folder-trust dialog for /workspace
#        - prove OMP's agent.db is readable
#   3. Spawn `sesh up --exec='claude ...'` for the implementer/claude
#      agent, then wait until `~/.sesh/hub.nats.url` exists so OMP's
#      second `sesh up` discovers the *same* hub instead of trying to
#      spawn its own.
#   4. Spawn `sesh up --exec='omp ...'` for the planner/omp agent.
#   5. Settle period — adapters take ~5s to register on the bus.
#   6. Run the test harness; pipe combined output to /var/artifacts/results.txt.
#   7. Snapshot session.json + per-agent logs into /var/artifacts/.
#   8. Tear down the children.
#
# Exit code = harness exit code. Non-fatal cleanup failures are swallowed
# so the artifact set always lands on the volume mount.

set -uo pipefail

ARTIFACTS=/var/artifacts
LOG_DIR=/var/log
mkdir -p "${ARTIFACTS}" "${LOG_DIR}"

log() { printf '[entrypoint] %s\n' "$*" >&2; }

# ── 1. Verify mounts ──────────────────────────────────────────────────
log "verify mounts"
if [ ! -f /root/.claude/.credentials.json ]; then
  log "FATAL: /root/.claude/.credentials.json missing — re-run scripts/stage-creds.sh"
  exit 2
fi
if [ ! -f /root/.omp/agent/agent.db ]; then
  log "FATAL: /root/.omp/agent/agent.db missing — re-run scripts/stage-creds.sh"
  exit 2
fi

# ── 2. Prelude (Claude OAuth + folder-trust + OMP smoke-check) ────────
log "prelude: extract Claude OAuth token"
CLAUDE_TOKEN=$(jq -r '.claudeAiOauth.accessToken // .accessToken // empty' \
                  /root/.claude/.credentials.json || true)
if [ -n "${CLAUDE_TOKEN}" ]; then
  export CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_TOKEN}"
  unset ANTHROPIC_API_KEY
  log "prelude: CLAUDE_CODE_OAUTH_TOKEN exported (len=${#CLAUDE_TOKEN})"
else
  log "prelude: WARNING — no claudeAiOauth.accessToken found; claude will rely on the mounted file directly"
fi

log "prelude: pre-accept folder-trust for /workspace"
mkdir -p /root/.claude
if [ -f /root/.claude.json ]; then EXISTING=$(cat /root/.claude.json); else EXISTING='{}'; fi
echo "${EXISTING}" | jq '
  .projects = (.projects // {}) |
  .projects["/workspace"] = ((.projects["/workspace"] // {}) + {"hasTrustDialogAccepted": true})
' > /root/.claude.json.tmp && mv /root/.claude.json.tmp /root/.claude.json

log "prelude: OMP agent.db check"
sqlite3 /root/.omp/agent/agent.db \
  'SELECT provider, credential_type FROM auth_credentials;' 2>&1 \
  | tee "${LOG_DIR}/omp-creds.txt" || log "prelude: WARNING — agent.db query failed"

# ── 3. Spawn Claude under sesh up ──────────────────────────────────────
log "spawn: claude under sesh up (session=smoke-test, role=implementer, class=active)"
cd /workspace

# We use `script -qfc` to give claude a real PTY — without it claude
# disables interactive mode features and the MCP boot path takes a
# different (sometimes broken) branch. `--input-format stream-json` +
# stdin closed means claude blocks waiting for further input rather than
# exiting after the first message; the MCP stdio server stays loaded
# the whole time so the adapter stays registered on the bus.
sesh up \
  --session=smoke-test \
  --role=implementer \
  --class=active \
  --exec='script -qfc "claude --dangerously-skip-permissions --append-system-prompt \"You are a quiet integration-test agent. When prompted via NATS, reply briefly and accurately. Do not initiate actions.\"" /dev/null' \
  > "${LOG_DIR}/claude.log" 2>&1 &
CLAUDE_PID=$!
log "spawn: claude sesh up PID=${CLAUDE_PID}"

# ── 4. Wait for the hub URL so OMP shares the same bus ────────────────
log "wait: ~/.sesh/hub.nats.url to appear"
for i in $(seq 1 60); do
  if [ -s /root/.sesh/hub.nats.url ]; then
    log "hub URL ready: $(cat /root/.sesh/hub.nats.url)"
    break
  fi
  sleep 0.5
done
if [ ! -s /root/.sesh/hub.nats.url ]; then
  log "FATAL: hub.nats.url never appeared — claude / sesh up failed to start"
  log "claude.log tail:"
  tail -n 80 "${LOG_DIR}/claude.log" >&2 || true
  cp -f "${LOG_DIR}/"*.log "${ARTIFACTS}/" 2>/dev/null || true
  kill ${CLAUDE_PID} 2>/dev/null || true
  exit 3
fi

# ── 5. Spawn OMP under sesh up ─────────────────────────────────────────
log "spawn: omp under sesh up (session=smoke-test, role=planner, class=active)"
sesh up \
  --session=smoke-test \
  --role=planner \
  --class=active \
  --exec='script -qfc "omp" /dev/null' \
  > "${LOG_DIR}/omp.log" 2>&1 &
OMP_PID=$!
log "spawn: omp sesh up PID=${OMP_PID}"

# ── 6. Settle ──────────────────────────────────────────────────────────
log "settle: 12s for both adapters to register"
sleep 12

# ── 7. Harness ─────────────────────────────────────────────────────────
log "harness: starting"
( cd /opt/harness && bun run harness.ts ) > "${ARTIFACTS}/results.txt" 2>&1
HARNESS_EXIT=$?
log "harness: exit=${HARNESS_EXIT}"

# ── 8. Snapshot artifacts ──────────────────────────────────────────────
log "snapshot: session.json + logs"
cp -f /workspace/.sesh/sessions/smoke-test.json "${ARTIFACTS}/session-smoke-test.json" 2>/dev/null \
  || log "snapshot: session JSON missing"
cp -f "${LOG_DIR}/"*.log "${ARTIFACTS}/" 2>/dev/null || true
cp -f /root/.sesh/hub.log "${ARTIFACTS}/hub.log" 2>/dev/null || true

# Capture final hub state for the report.
{
  echo "=== hub.nats.url ==="
  cat /root/.sesh/hub.nats.url 2>/dev/null || echo "(missing)"
  echo
  echo "=== hub.url ==="
  cat /root/.sesh/hub.url 2>/dev/null || echo "(missing)"
  echo
  echo "=== session JSON ==="
  cat /workspace/.sesh/sessions/smoke-test.json 2>/dev/null || echo "(missing)"
} > "${ARTIFACTS}/hub-state.txt"

# ── 9. Cleanup ─────────────────────────────────────────────────────────
log "cleanup: SIGINT to sesh up children"
kill -INT "${CLAUDE_PID}" "${OMP_PID}" 2>/dev/null || true
wait 2>/dev/null || true

exit "${HARNESS_EXIT}"
