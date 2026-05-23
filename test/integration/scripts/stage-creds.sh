#!/usr/bin/env bash
#
# stage-creds.sh — macOS host script.
#
# Reads the operator's local Claude + OMP credentials, copies them to a
# tmpdir, and writes the absolute paths to a .env file consumed by
# compose.yaml. Idempotent — safe to re-run.
#
# Source pattern: darken/cmd/darken/creds.go (Claude keychain) +
# OMP rig pattern from the plan (copy ~/.omp/agent/agent.db* so SQLite
# WAL/SHM can checkpoint inside the container without touching the
# operator's live state).

set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "stage-creds: only supports macOS (security CLI + ~/.omp default layout)"
  echo "stage-creds: on Linux, edit the .env file manually"
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
RIG_DIR="${REPO_ROOT}/test/integration"
ENV_FILE="${RIG_DIR}/.env"

TMP_ROOT="$(mktemp -d -t sesh-integ-creds.XXXXXX)"
echo "stage-creds: tmpdir=${TMP_ROOT}"

# ── 1. Claude OAuth (keychain first, fallback to ~/.claude/.credentials.json) ──
# Claude Code 2.1.x migrated to file-backed creds on some installs; check
# both paths. The container reads from the file at /root/.claude/.credentials.json
# either way.
CLAUDE_CREDS="${TMP_ROOT}/claude-credentials.json"
if security find-generic-password -s "Claude Code-credentials" -w \
     > "${CLAUDE_CREDS}" 2>/dev/null && [[ -s "${CLAUDE_CREDS}" ]]; then
  chmod 600 "${CLAUDE_CREDS}"
  echo "stage-creds: claude OAuth blob (keychain) → ${CLAUDE_CREDS} ($(wc -c < "${CLAUDE_CREDS}" | tr -d ' ') bytes)"
elif [[ -f "${HOME}/.claude/.credentials.json" ]]; then
  cp "${HOME}/.claude/.credentials.json" "${CLAUDE_CREDS}"
  chmod 600 "${CLAUDE_CREDS}"
  echo "stage-creds: claude OAuth blob (file) → ${CLAUDE_CREDS} ($(wc -c < "${CLAUDE_CREDS}" | tr -d ' ') bytes)"
else
  echo "stage-creds: WARNING — no Claude OAuth found (neither keychain nor ~/.claude/.credentials.json)"
  echo "stage-creds: did you sign in to Claude Code on this Mac?"
  exit 2
fi

# ── 1b. Claude config (.claude.json) — copy operator's so the wizard
#        + theme + telemetry-opt-in screens are all pre-resolved. We
#        scrub a few fields that don't make sense in the container.
CLAUDE_CFG="${TMP_ROOT}/claude.json"
if [[ -f "${HOME}/.claude.json" ]]; then
  jq '
    .projects = {} |
    .projects["/workspace"] = {
      "hasTrustDialogAccepted": true,
      "hasCompletedProjectOnboarding": true,
      "projectOnboardingSeenCount": 99,
      "enabledMcpjsonServers": ["nats"],
      "disabledMcpjsonServers": [],
      "mcpContextUris": [],
      "allowedTools": []
    } |
    .hasCompletedOnboarding = true |
    .hasSeenWelcome = true |
    .bypassPermissionsModeAccepted = true |
    del(.mcpServers)
  ' "${HOME}/.claude.json" > "${CLAUDE_CFG}" 2>/dev/null \
    && chmod 600 "${CLAUDE_CFG}"
  echo "stage-creds: claude config → ${CLAUDE_CFG}"
else
  echo "stage-creds: WARNING — ${HOME}/.claude.json missing; first-run wizard may block"
  echo '{"hasCompletedOnboarding":true,"theme":"dark","projects":{"/workspace":{"hasTrustDialogAccepted":true}}}' > "${CLAUDE_CFG}"
fi

# ── 2. OMP agent.db (copy, don't bind — SQLite WAL/SHM mutates) ────────
OMP_SRC="${HOME}/.omp/agent"
OMP_DST="${TMP_ROOT}/omp-agent"
if [[ ! -f "${OMP_SRC}/agent.db" ]]; then
  echo "stage-creds: WARNING — ${OMP_SRC}/agent.db not found; have you run omp locally yet?"
  exit 3
fi
mkdir -p "${OMP_DST}"
# Copy db + WAL + SHM if present. Use -a to preserve attrs; the container
# remounts the dir RW.
cp -a "${OMP_SRC}/." "${OMP_DST}/"
chmod 700 "${OMP_DST}"
echo "stage-creds: omp dir → ${OMP_DST} ($(ls "${OMP_DST}" | wc -l | tr -d ' ') files)"

# Quick sanity peek — what auth rows did we copy?
if command -v sqlite3 >/dev/null 2>&1; then
  echo "stage-creds: omp auth rows:"
  sqlite3 "${OMP_DST}/agent.db" \
    "SELECT provider, credential_type FROM auth_credentials;" 2>/dev/null \
    | sed 's/^/  /' || true
fi

# ── 3. Stage a slim copy of sesh-channels ──────────────────────────────
# Docker named-build-contexts transfer the *whole* directory to BuildKit
# even when the Dockerfile COPYs a single subdir, so we mirror only the
# two adapters we exercise (no node_modules, no other agents). The mirror
# is a fresh tmpdir so it stays clean across runs.
CHANNELS_SRC="${SESH_CHANNELS_SRC:-${HOME}/projects/sesh-channels}"
CHANNELS_DST="${TMP_ROOT}/sesh-channels"
if [[ ! -d "${CHANNELS_SRC}" ]]; then
  echo "stage-creds: WARNING — \$SESH_CHANNELS_SRC=${CHANNELS_SRC} not found"
  exit 4
fi
mkdir -p "${CHANNELS_DST}/claude-nats-channel" "${CHANNELS_DST}/omp-nats-channel"
rsync -a --exclude='node_modules' --exclude='.git' \
  "${CHANNELS_SRC}/claude-nats-channel/" "${CHANNELS_DST}/claude-nats-channel/"
rsync -a --exclude='node_modules' --exclude='.git' \
  "${CHANNELS_SRC}/omp-nats-channel/"    "${CHANNELS_DST}/omp-nats-channel/"
# Plugin-mode (PR #108): the Dockerfile COPYs .claude-plugin/marketplace.json
# from the channels build context. Mirror it into the slim copy.
if [[ -d "${CHANNELS_SRC}/.claude-plugin" ]]; then
  rsync -a "${CHANNELS_SRC}/.claude-plugin/" "${CHANNELS_DST}/.claude-plugin/"
fi
echo "stage-creds: channels (slim) → ${CHANNELS_DST}"

# ── 4. Stage a slim copy of EdgeSync ───────────────────────────────────
# sesh's go.mod has `replace github.com/danmestas/EdgeSync => ../EdgeSync`
# so we ship that source tree as a separate build context. Drop `.git`
# (130MB), `bin/`, and other heavy dirs already excluded by EdgeSync's
# own .dockerignore — rsync mirrors them again here just in case.
EDGESYNC_SRC="${EDGESYNC_SRC:-${HOME}/projects/EdgeSync}"
EDGESYNC_DST="${TMP_ROOT}/EdgeSync"
if [[ ! -d "${EDGESYNC_SRC}" ]]; then
  echo "stage-creds: WARNING — \$EDGESYNC_SRC=${EDGESYNC_SRC} not found"
  exit 5
fi
mkdir -p "${EDGESYNC_DST}"
rsync -a \
  --exclude='.git' --exclude='bin' --exclude='tmp' --exclude='.worktrees' \
  --exclude='*.fossil' --exclude='*.log' --exclude='dst' --exclude='sim' \
  --exclude='libfossil' --exclude='fossil' --exclude='bridge' \
  --exclude='iroh-sidecar' --exclude='edgesync' --exclude='docs' \
  --exclude='testdata' --exclude='deploy' --exclude='scripts' \
  --exclude='node_modules' --exclude='.claude' --exclude='.github' \
  "${EDGESYNC_SRC}/" "${EDGESYNC_DST}/"
echo "stage-creds: EdgeSync (slim) → ${EDGESYNC_DST}"

# ── 5. Artifacts dir (mounted, not bind-of-tmp so results persist) ─────
ARTIFACTS="${RIG_DIR}/artifacts"
mkdir -p "${ARTIFACTS}"
echo "stage-creds: artifacts dir → ${ARTIFACTS}"

# ── 6. Write .env for compose ──────────────────────────────────────────
cat > "${ENV_FILE}" <<EOF
# Generated by test/integration/scripts/stage-creds.sh — do not commit.
# Re-run that script to refresh.
HOST_CLAUDE_CREDS=${CLAUDE_CREDS}
HOST_CLAUDE_CFG=${CLAUDE_CFG}
HOST_OMP_DIR=${OMP_DST}
HOST_ARTIFACTS_DIR=${ARTIFACTS}
SESH_CHANNELS_DIR=${CHANNELS_DST}
EDGESYNC_DIR=${EDGESYNC_DST}
EOF

echo "stage-creds: wrote ${ENV_FILE}"
echo
echo "next:  cd ${RIG_DIR} && docker compose up --build"
echo "       (or:  bash scripts/run.sh)"
