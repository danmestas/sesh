#!/usr/bin/env bash
#
# entrypoint.sh — runs inside the container as PID 2 under tini, as user
# `integ` (non-root because claude refuses --dangerously-skip-permissions
# under root for security reasons).
#
# Sequence:
#   1. Verify the cred mounts the operator's stage-creds.sh staged.
#   2. Prelude: extract Claude OAuth token, pre-accept Claude's folder-trust
#      dialog, smoke-check OMP's agent.db.
#   3. Single `sesh up --exec` spawns BOTH claude and omp under a tiny
#      bash wrapper that:
#        - exports per-process SESH_ROLE (implementer for claude, planner
#          for omp), since both processes inherit env from one sesh up
#          invocation
#        - launches each agent under `script -qfc` to give them a real PTY
#          (both refuse to boot cleanly otherwise)
#        - waits on whichever child exits first
#      A single `sesh up` is required because `cli/session.go:ClaimSession`
#      holds the session label exclusively; two parallel `sesh up`s on the
#      same label collide. The plan's "two parallel sesh up" pattern needs
#      either separate sessions or this single-wrapper variant.
#   4. Settle period — adapters take ~5-10s to register on the bus.
#   5. Run the test harness; pipe combined output to /var/artifacts/results.txt.
#   6. Snapshot session.json + per-agent logs into /var/artifacts/.
#   7. Tear down the children.

set -uo pipefail

HOME_DIR="${HOME:-/home/integ}"
ARTIFACTS=/var/artifacts
LOG_DIR=/var/log
mkdir -p "${ARTIFACTS}" "${LOG_DIR}"

log() { printf '[entrypoint] %s\n' "$*" >&2; }

# ── 1. Verify mounts ──────────────────────────────────────────────────
log "verify mounts (HOME=${HOME_DIR})"
if [ ! -f "${HOME_DIR}/.claude/.credentials.json" ]; then
  log "FATAL: ${HOME_DIR}/.claude/.credentials.json missing — re-run scripts/stage-creds.sh"
  exit 2
fi
if [ ! -f "${HOME_DIR}/.omp/agent/agent.db" ]; then
  log "FATAL: ${HOME_DIR}/.omp/agent/agent.db missing — re-run scripts/stage-creds.sh"
  exit 2
fi

# ── 2. Prelude (Claude OAuth + folder-trust + OMP smoke-check) ────────
log "prelude: extract Claude OAuth token"
CLAUDE_TOKEN=$(jq -r '.claudeAiOauth.accessToken // .accessToken // empty' \
                  "${HOME_DIR}/.claude/.credentials.json" || true)
if [ -n "${CLAUDE_TOKEN}" ]; then
  export CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_TOKEN}"
  unset ANTHROPIC_API_KEY
  log "prelude: CLAUDE_CODE_OAUTH_TOKEN exported (len=${#CLAUDE_TOKEN})"
else
  log "prelude: WARNING — no claudeAiOauth.accessToken found; claude will rely on the mounted file directly"
fi

log "prelude: claude config mounted at ${HOME_DIR}/.claude.json (from operator's local config)"
if [ ! -f "${HOME_DIR}/.claude.json" ]; then
  log "WARNING — .claude.json mount missing; claude may block on the first-run wizard"
fi

log "prelude: OMP agent.db check"
sqlite3 "${HOME_DIR}/.omp/agent/agent.db" \
  'SELECT provider, credential_type FROM auth_credentials;' 2>&1 \
  | tee "${LOG_DIR}/omp-creds.txt" || log "prelude: WARNING — agent.db query failed"

# The host's ~/.omp/agent mount overlays the in-image config.yml, which
# means the operator's local extensions: list (paths that don't exist in
# the container) gets used and our nats-channel never loads. Rewrite the
# config.yml in-place inside the container — the mount is RW.
log "prelude: rewrite OMP config.yml so extensions point at the in-container channel"
cat > "${HOME_DIR}/.omp/agent/config.yml" <<EOF
extensions:
  - /opt/sesh-channels/omp-nats-channel/extensions/nats-channel.ts
EOF

# ── 3. Single sesh up → both agents under one wrapper ──────────────────
log "spawn: single sesh up --exec, wrapping claude + omp"
cd /workspace

# The exec runs under sh -c (sesh's `spawnHarness` uses sh unconditionally),
# so we trampoline into bash to get `wait -n`. Inside the bash wrapper:
#   - claude runs under `script -qfc` so it gets a PTY
#     - SESH_ROLE=implementer overrides the sesh-supplied env for THIS
#       subshell only; SESH_CLASS=active inherited from outer sesh up
#   - omp likewise, with SESH_ROLE=planner
#   - `wait -n` returns when either exits — drives the whole sesh up
#     down cleanly on either agent's death.
# The outer sesh up gets a neutral --role=worker; per-process env
# overrides are what land in metadata.role.
# A small launch script is easier to maintain than a multi-quoted inline
# blob inside sesh's --exec. Both agents run under `script -qfc` (PTY
# wrapper) so they think they're on a TTY. Stdin is held open by a
# `sleep infinity` redirected into each `script` so neither exits on
# EOF.
cat > /tmp/launch-agents.sh <<'WRAP'
#!/usr/bin/env bash
# Launched by sesh up --exec under sh -c. Inherits SESH_SESSION + NATS_URL
# from sesh; we override SESH_ROLE per child via subshell env.
set -o pipefail
echo "[exec-wrapper] starting; SESH_SESSION=${SESH_SESSION:-} SESH_ROLE=${SESH_ROLE:-} SESH_CLASS=${SESH_CLASS:-} USER=${USER:-} NATS_URL=${NATS_URL:-}" >&2

# Two fifos — keep claude/omp stdin held open with `sleep infinity > fifo`
# in the background, so neither sees EOF and exits.
mkfifo /tmp/claude.fifo /tmp/omp.fifo
# Hold both FIFOs open by writing zero bytes forever. For claude we also
# auto-feed "2\n" (the "Yes, I accept" choice in claude's Bypass-Permissions
# warning dialog) so the wizard advances without operator input. The
# claude.json field `bypassPermissionsModeAccepted` is not authoritative
# across all claude versions, so we belt-and-braces both paths.
(
  # Wait long enough for claude to render the dialog, then type "2" + Enter,
  # then sit and hold the FIFO open so claude doesn't EOF.
  sleep 6
  printf '2\n'
  sleep infinity
) > /tmp/claude.fifo &
( sleep infinity > /tmp/omp.fifo )    &

(
  export SESH_ROLE=implementer
  export SESH_CLASS=active
  echo "[claude-side] SESH_SESSION=$SESH_SESSION SESH_ROLE=$SESH_ROLE SESH_CLASS=$SESH_CLASS HOME=$HOME PATH=$PATH NATS_URL=$NATS_URL" >&2
  # `--strict-mcp-config` + `--mcp-config` skips the .mcp.json auto-discovery
  # path entirely (which would otherwise present a 1/2/3 trust dialog the
  # first time claude sees a new MCP server in the project). The explicit
  # config we pass is treated as operator-supplied and pre-trusted.
  exec script -qfc "claude --dangerously-skip-permissions --strict-mcp-config --mcp-config /opt/claude.mcp.json" /dev/null < /tmp/claude.fifo
) > /var/log/claude.log 2>&1 &
CLAUDE=$!
echo "[exec-wrapper] claude pid=$CLAUDE" >&2

(
  export SESH_ROLE=planner
  export SESH_CLASS=active
  # omp-nats-channel/extensions/nats-channel.ts only reads NATS_SESSION_NAME
  # for the session token — it does NOT consult SESH_SESSION like
  # claude-nats-channel/server.ts does (claude calls discoverSessionLabel
  # which checks SESH_SESSION; OMP falls straight through to basename(cwd)).
  # Work around by setting NATS_SESSION_NAME explicitly. This is an
  # adapter-inconsistency finding for sesh-channels (see FINDINGS).
  export NATS_SESSION_NAME="${SESH_SESSION:-}"
  echo "[omp-side] SESH_SESSION=$SESH_SESSION NATS_SESSION_NAME=$NATS_SESSION_NAME SESH_ROLE=$SESH_ROLE SESH_CLASS=$SESH_CLASS HOME=$HOME PATH=$PATH NATS_URL=$NATS_URL" >&2
  exec script -qfc "omp" /dev/null < /tmp/omp.fifo
) > /var/log/omp.log 2>&1 &
OMP=$!
echo "[exec-wrapper] omp pid=$OMP" >&2

# Wait for whichever child dies first, then kill the other and exit
# with the first exit code. sesh up's parent observes our exit and
# brings the session down.
wait -n $CLAUDE $OMP
EC=$?
echo "[exec-wrapper] first exited (status=$EC); killing siblings" >&2
kill $CLAUDE $OMP 2>/dev/null || true
wait
exit $EC
WRAP
chmod +x /tmp/launch-agents.sh

sesh up \
  --session=smoke-test \
  --role=worker \
  --class=active \
  --exec='/tmp/launch-agents.sh' \
  > "${LOG_DIR}/sesh.log" 2>&1 &
SESH_PID=$!
log "spawn: sesh up PID=${SESH_PID}"

# ── 4. Wait for the hub URL + cache it for the harness ────────────────
# The hub daemon auto-exits when its last leaf disconnects (sesh's
# Auto-shutdown mode) — so if sesh up dies, hub.nats.url disappears.
# Snapshot the URL at first sighting into /var/artifacts so the harness
# has something to read even if the hub turns over.
log "wait: ~/.sesh/hub.nats.url to appear"
HUB_URL=""
for i in $(seq 1 60); do
  if [ -s "${HOME_DIR}/.sesh/hub.nats.url" ]; then
    HUB_URL=$(cat "${HOME_DIR}/.sesh/hub.nats.url")
    log "hub URL ready: ${HUB_URL}"
    cp -f "${HOME_DIR}/.sesh/hub.nats.url" "${ARTIFACTS}/hub.nats.url"
    break
  fi
  sleep 0.5
done
if [ -z "${HUB_URL}" ]; then
  log "FATAL: hub.nats.url never appeared — sesh up failed to bind"
  log "sesh.log tail:"
  tail -n 80 "${LOG_DIR}/sesh.log" >&2 || true
  cp -f "${LOG_DIR}/"*.log "${ARTIFACTS}/" 2>/dev/null || true
  kill ${SESH_PID} 2>/dev/null || true
  exit 3
fi

log "settle: 20s for both adapters to boot + register"
sleep 20

# Verify the adapters are alive before running the harness. If the wrapper
# died (claude/omp exited), grab the logs and bail early — that's a useful
# finding, not a 90-second timeout.
if ! kill -0 ${SESH_PID} 2>/dev/null; then
  log "FATAL: sesh up PID ${SESH_PID} no longer alive — agents must have died"
  log "claude.log tail:"
  tail -n 50 /var/log/claude.log >&2 || true
  log "omp.log tail:"
  tail -n 50 /var/log/omp.log >&2 || true
  cp -f /var/log/*.log "${ARTIFACTS}/" 2>/dev/null || true
  exit 4
fi

# ── 5. Harness ─────────────────────────────────────────────────────────
log "harness: starting"
( cd /opt/harness && bun run harness.ts ) > "${ARTIFACTS}/results.txt" 2>&1
HARNESS_EXIT=$?
log "harness: exit=${HARNESS_EXIT}"

# ── 6. Snapshot artifacts ──────────────────────────────────────────────
log "snapshot: session.json + logs"
cp -f /workspace/.sesh/sessions/smoke-test.json "${ARTIFACTS}/session-smoke-test.json" 2>/dev/null \
  || log "snapshot: session JSON missing"
cp -f "${LOG_DIR}/"*.log "${ARTIFACTS}/" 2>/dev/null || true
cp -f "${HOME_DIR}/.sesh/hub.log" "${ARTIFACTS}/hub.log" 2>/dev/null || true

{
  echo "=== hub.nats.url ==="
  cat "${HOME_DIR}/.sesh/hub.nats.url" 2>/dev/null || echo "(missing)"
  echo
  echo "=== hub.url ==="
  cat "${HOME_DIR}/.sesh/hub.url" 2>/dev/null || echo "(missing)"
  echo
  echo "=== session JSON ==="
  cat /workspace/.sesh/sessions/smoke-test.json 2>/dev/null || echo "(missing)"
} > "${ARTIFACTS}/hub-state.txt"

# ── 7. Cleanup ─────────────────────────────────────────────────────────
log "cleanup: SIGINT to sesh up"
kill -INT "${SESH_PID}" 2>/dev/null || true
wait 2>/dev/null || true

exit "${HARNESS_EXIT}"
