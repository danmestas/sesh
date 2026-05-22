# syntax=docker/dockerfile:1.6
#
# Docker integration rig for sesh + sesh-channels + Claude Code + Oh My Pi.
#
# Stage 1 — sesh-builder: builds the sesh binary from the in-tree source.
# Stage 2 — runtime: oven/bun + Node 20, Claude Code CLI, Oh My Pi, and the
# two sesh-channels adapters (claude-nats-channel + omp-nats-channel) wired
# to load on startup. Mounts the operator's local Claude / OMP OAuth state
# at runtime — no API keys baked into the image.
#
# See test/integration/README.md for the full architecture + usage.

# ──────────────────────────────────────────────────────────────────────
# Stage 1: build sesh binary
# ──────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS sesh-builder

WORKDIR /src

# Pre-pull module deps using the lockfile so the actual source COPY below
# stays cache-friendly when only Go files change.
COPY go.mod go.sum ./
RUN go mod download

# Now bring in the rest of the tree. The build context is the repo root
# (see compose.yaml / scripts/run.sh — `context: ../..`).
COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/sesh ./cmd/sesh

# ──────────────────────────────────────────────────────────────────────
# Stage 2: runtime
# ──────────────────────────────────────────────────────────────────────
FROM oven/bun:1.3.14-debian AS runtime

# Node 20 (Claude Code needs >=18), jq, curl, ca-certs, git, dbus (for
# machine-id), openssl (for project-id), procps + util-linux (for `script`,
# which gives us a PTY wrapper so the interactive agents start cleanly
# under a non-TTY parent), sqlite3 (debug-time inspection of OMP's db).
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      curl ca-certificates gnupg jq git dbus openssl procps util-linux sqlite3 tini && \
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && \
    apt-get install -y --no-install-recommends nodejs && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Stable machine-id so the coord package's machine() function doesn't
# return the MachineLocal sentinel (see internal/coord/machine_*.go).
RUN dbus-uuidgen --ensure=/etc/machine-id && cat /etc/machine-id

# Claude Code CLI (binary: `claude`).
RUN npm install -g --no-progress @anthropic-ai/claude-code && claude --version

# Oh My Pi (binary: `omp`).
RUN bun install -g @oh-my-pi/pi-coding-agent && omp --version

# Bring in the sesh binary from Stage 1.
COPY --from=sesh-builder /out/sesh /usr/local/bin/sesh
RUN sesh --help >/dev/null 2>&1 || true

# Pull in the sesh-channels adapters. We only need the two we exercise
# (claude-nats-channel + omp-nats-channel) — pi/gemini/grok are skipped.
# Path inside the host is /Users/dmestas/projects/sesh-channels; the build
# context (set in compose.yaml) makes that available as the secondary
# build context `channels`.
COPY --from=channels claude-nats-channel /opt/sesh-channels/claude-nats-channel
COPY --from=channels omp-nats-channel    /opt/sesh-channels/omp-nats-channel

# `bun install` per adapter so the @agent-ops/sesh-channels + @synadia-ai/agents
# + @nats-io/transport-node deps resolve at runtime. Done at build time so
# `docker compose up` boot is fast.
RUN cd /opt/sesh-channels/claude-nats-channel && bun install --no-summary && \
    cd /opt/sesh-channels/omp-nats-channel    && bun install --no-summary

# Workspace where both agents run (cwd for `sesh up`). Bake project-id at
# build time — coordinateLoop in internal/refagent skips coordinate subjects
# without it (see coord/machine_linux.go + the plan's recon notes).
ENV WORKSPACE=/workspace
RUN mkdir -p ${WORKSPACE}/.sesh && \
    printf '%s\n' "$(openssl rand -hex 20)" > ${WORKSPACE}/.sesh/project-id

# Mount points for the operator's Claude + OMP OAuth — directories created
# here, files arrive at `docker run` via -v binds from scripts/stage-creds.sh.
RUN mkdir -p /root/.claude /root/.omp/agent /root/.sesh /var/log /var/artifacts

# Static configs baked into the image.
#   - claude.mcp.json → /workspace/.mcp.json (Claude reads it at startup;
#     CLAUDE_PLUGIN_ROOT is the env var the channel template uses, so we
#     hard-substitute the absolute path at the copy site below).
#   - omp-config.yml  → /root/.omp/agent/config.yml.
COPY test/integration/config/claude.mcp.json /workspace/.mcp.json
COPY test/integration/config/omp-config.yml  /root/.omp/agent/config.yml

# Harness — TS test program. Copy + bun install for fast container start.
COPY test/integration/harness /opt/harness
RUN cd /opt/harness && bun install --no-summary

# Entrypoint orchestrates: prelude (OAuth) → `sesh up --exec` x2 → harness.
COPY test/integration/entrypoint.sh /opt/entrypoint.sh
RUN chmod +x /opt/entrypoint.sh

# `USER` is what the adapters use for the `metadata.owner` subject token.
# Without this, owner falls back to "unknown" — registration would still
# work but harness's discovery filters break.
ENV USER=integ
ENV HOME=/root

WORKDIR /workspace

# tini handles signal forwarding for the multi-process tree (entrypoint.sh
# spawns 2× sesh up + the harness as children).
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/entrypoint.sh"]
