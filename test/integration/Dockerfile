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

# sesh has a `replace github.com/danmestas/EdgeSync => ../EdgeSync` in
# go.mod, so we need EdgeSync's source available at the relative path Go
# expects. Bring it in via the `edgesync` build context (configured in
# compose.yaml) at /EdgeSync, and place the sesh tree at /src — the
# relative `../EdgeSync` then resolves correctly during go mod download.
WORKDIR /
COPY --from=edgesync . /EdgeSync/
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

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

# Oh My Pi (binary: `omp`). We install with bun into /usr/local-style
# globals so the binary lands on a path readable by the non-root `integ`
# user introduced below. The default `bun install -g` puts binaries in
# /root/.bun/bin/, which integ cannot read.
ENV BUN_INSTALL=/usr/local/bun-global
ENV PATH=${BUN_INSTALL}/bin:${PATH}
RUN bun install -g @oh-my-pi/pi-coding-agent && \
    chmod -R a+rX ${BUN_INSTALL} && \
    omp --version

# Bring in the sesh binary from Stage 1.
COPY --from=sesh-builder /out/sesh /usr/local/bin/sesh
RUN sesh --help >/dev/null 2>&1 || true

# Pull in the sesh-channels adapters. We only need the two we exercise
# (claude-nats-channel + omp-nats-channel) — pi/gemini/grok are skipped.
# Path inside the host is /Users/dmestas/projects/sesh-channels; the build
# context (set in compose.yaml) makes that available as the secondary
# build context `channels`.
#
# `.claude-plugin/marketplace.json` is required so `claude plugin marketplace
# add /opt/sesh-channels` can resolve the marketplace manifest. Without it
# the plugin-mode install path below (and the pre-baked plugin enablement
# JSONs) have nothing to point at.
COPY --from=channels .claude-plugin      /opt/sesh-channels/.claude-plugin
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

# Create a non-root user; claude refuses --dangerously-skip-permissions
# as root for security reasons, so the agent processes (and the harness)
# run as `integ` instead.
RUN useradd -ms /bin/bash -u 1500 integ

# Mount points for the operator's Claude + OMP OAuth — directories created
# here, files arrive at `docker run` via -v binds from scripts/stage-creds.sh.
# Owned by integ so the bind mount and writes line up.
RUN mkdir -p /home/integ/.claude /home/integ/.omp/agent /home/integ/.sesh \
             /var/log /var/artifacts && \
    chown -R integ:integ /home/integ /var/log /var/artifacts /workspace /opt/sesh-channels

# OMP extension config — drives nats-channel adapter load on OMP startup.
COPY --chown=integ:integ test/integration/config/omp-config.yml  /home/integ/.omp/agent/config.yml

# ── Plugin-mode install for claude-nats-channel ────────────────────────
# Mirrors the operator's production invocation:
#   claude --dangerously-load-development-channels plugin:nats-channel@sesh-channels
# which expects the plugin to be registered in ~/.claude/plugins/ as a
# user-scope install from a `directory`-source marketplace.
#
# Operator-equivalent commands:
#   claude plugin marketplace add ~/projects/sesh-channels
#   claude plugin install nats-channel@sesh-channels -s user
#
# Both prompt for confirmation today (no `-y` / `--yes` flag), so they can't
# run unattended inside `docker build`. Instead we pre-bake the three JSON
# files claude-code reads at startup — known_marketplaces.json,
# installed_plugins.json, config.json — to the on-disk shape the operator's
# `~/.claude/plugins/` has after running the CLI commands. Verified by
# direct inspection of the operator's local install.
#
# This makes the rig install:
#   * deterministic (every build produces identical plugin state)
#   * idempotent (rebuilds overwrite the same files with identical content)
#   * self-contained (no install at runtime; no network access required for
#     plugin registration)
#
# If a future claude-code release adds a `--yes` flag to `plugin install`,
# swap this block for the two CLI commands above and delete the JSONs.
RUN mkdir -p /home/integ/.claude/plugins && chown -R integ:integ /home/integ/.claude
COPY --chown=integ:integ test/integration/config/claude-plugins/known_marketplaces.json /home/integ/.claude/plugins/known_marketplaces.json
COPY --chown=integ:integ test/integration/config/claude-plugins/installed_plugins.json  /home/integ/.claude/plugins/installed_plugins.json
COPY --chown=integ:integ test/integration/config/claude-plugins/config.json             /home/integ/.claude/plugins/config.json

# Skip the "Bypass Permissions mode" warning dialog at startup. Without this,
# claude-code 2.1.126 renders a TTY-only dialog that requires "2\n" input,
# which the rig was previously feeding via a FIFO timing kludge. The
# managed-settings key dismisses the dialog at the source. (F4.3)
COPY --chown=integ:integ test/integration/config/claude-settings.json /home/integ/.claude/settings.json

# Harness — TS test program. Copy + bun install for fast container start.
COPY --chown=integ:integ test/integration/harness /opt/harness
RUN cd /opt/harness && bun install --no-summary && chown -R integ:integ /opt/harness

# Entrypoint orchestrates: prelude (OAuth) → `sesh up --exec` x2 → harness.
COPY test/integration/entrypoint.sh /opt/entrypoint.sh
RUN chmod +x /opt/entrypoint.sh

# `USER` is what the adapters use for the `metadata.owner` subject token.
# Without this, owner falls back to "unknown" — registration would still
# work but harness's discovery filters break.
ENV USER=integ
ENV HOME=/home/integ

USER integ

WORKDIR /workspace

# tini handles signal forwarding for the multi-process tree (entrypoint.sh
# spawns 2× sesh up + the harness as children).
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/entrypoint.sh"]
