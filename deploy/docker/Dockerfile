# Generic base image for a standalone `plect serve` deployment — see
# deploy/docker/README.md for what it ships and how a downstream deployment
# extends it without forking this file.
#
# Every version below is an explicit pin: a base image digest, a
# checksum-verified download at an exact version, or (the source itself)
# the commit PLECT_SOURCE_SHA names. No `@latest`, no floating tag.
# Debian's own package versions are the one exception, pinned only via the
# base image digest — bookworm is a frozen stable release, so its packages
# move only via security updates.

# ---- source --------------------------------------------------------
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS source

RUN apt-get update -qq && apt-get install -y --no-install-recommends \
  ca-certificates=20250419~deb12u1 \
  git=1:2.39.5-0+deb12u3 \
  && rm -rf /var/lib/apt/lists/*

# Fetched by this exact commit from PLECT_SOURCE_REPO, not read from the
# local build context: a plain COPY would package whatever a local working
# tree happens to hold, so a wrong-but-well-formed PLECT_SOURCE_SHA (an
# older commit, a typo) could stamp source it never actually verified. A
# `git fetch` of one commit SHA either resolves to exactly that commit's
# tree or fails outright — there is no third outcome to assert against.
ARG PLECT_SOURCE_REPO=https://github.com/kecbigmt/plecture.git
ARG PLECT_SOURCE_SHA
RUN test -n "${PLECT_SOURCE_SHA}" && echo "${PLECT_SOURCE_SHA}" | grep -Eq '^[0-9a-f]{40}$' \
  || { echo "PLECT_SOURCE_SHA must be set to a 40-character commit SHA" >&2; exit 1; }
RUN git init -q /src \
  && git -C /src remote add origin "${PLECT_SOURCE_REPO}" \
  && git -C /src fetch --depth 1 origin "${PLECT_SOURCE_SHA}" \
  && git -C /src checkout -q --detach FETCH_HEAD \
  && test "$(git -C /src rev-parse HEAD)" = "${PLECT_SOURCE_SHA}"

# ---- builder ---------------------------------------------------------
FROM golang:1.25.6-bookworm@sha256:f4490d7b261d73af4543c46ac6597d7d101b6e1755bcdd8c5159fda7046b6b3e AS builder

COPY --from=source /src /opt/plect-repo
# Left empty (the default), the build stays a development build: Current
# keeps its "0.0.0-dev" placeholder, and the migration access gate
# (docs/design/sqlite-persistence.md) refuses to forward-migrate a
# storage.db it did not create — on purpose, for any image not built from
# a release-pinned PLECT_SOURCE_SHA. A packager building a release image
# sets PLECT_VERSION to the tag, matching what .github/workflows/release.yml
# stamps into its own archives.
ARG PLECT_VERSION
RUN mkdir -p /out/bin \
  && flags="" \
  && if [ -n "${PLECT_VERSION}" ]; then \
       flags="-X github.com/kecbigmt/plecture/app/internal/version.Current=${PLECT_VERSION}"; \
     fi \
  && go -C /opt/plect-repo/app build -ldflags "${flags}" -o /out/bin/plect ./cmd/plect

# `plect init` registers /opt/plect-repo (app/ and contracts/ ride along as
# plugins/'s siblings because some in-repo plugin executables' go.mod
# `replace`s a contracts/ module by a repo-root-relative path) as a
# `path://` catalog — which mounts the directory directly, so nothing at
# runtime needs network access to resolve it — enables the example plugin
# set, and builds their executables in the same step. The allowlist
# pattern is a placeholder: a downstream deployment's config.toml overlay
# replaces it (README's Extending section).
ENV HOME=/root
RUN /out/bin/plect init --yes \
  --config-home /etc/plect \
  --catalog-alias official \
  --catalog-source path:///opt/plect-repo \
  --catalog-subdir plugins \
  --enable tmux --enable claude --enable github \
  --allowlist '^https://github\.com/your-org/' \
  --workspace-dirs-root /var/lib/plect/workspace_dirs

COPY --from=source /src/deploy/docker/example-config/workflows/ /etc/plect/workflows/

# Fails the build loud if the example config doesn't actually load and
# resolve — this is the one place a broken example ships silently
# otherwise, since nothing else in this Dockerfile ever loads it.
RUN /out/bin/plect workflow show example_work --config-home /etc/plect >/dev/null

# ---- runtime -----------------------------------------------------------
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS runtime

# Re-declared: a build ARG's scope is per-stage even though its value comes
# from one `--build-arg` at the `docker build` invocation.
ARG PLECT_SOURCE_SHA
LABEL org.opencontainers.image.revision="${PLECT_SOURCE_SHA}"
RUN echo "${PLECT_SOURCE_SHA}" > /etc/plecture-source-revision

# runit: the outer supervisor — see README's Process model section for
# the s6-overlay/supervisord/runit choice.
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
  ca-certificates=20250419~deb12u1 \
  curl=7.88.1-10+deb12u15 \
  git=1:2.39.5-0+deb12u3 \
  jq=1.6-2.1+deb12u2 \
  procps=2:4.0.2-3 \
  runit=2.1.2-54 \
  tmux=3.3a-3 \
  # uuid-runtime: the claude/codex runtime tasks mint agent session ids
  # with uuidgen.
  uuid-runtime=2.38.1-5+deb12u3 \
  xz-utils=5.4.1-1+deb12u1 \
  && rm -rf /var/lib/apt/lists/* \
  # The runit package registers its own placeholder "default-syslog"
  # service (`sleep 6000`, via runit-helper); this container supervises
  # exactly one service.
  && rm -f /etc/service/default-syslog

# An upstream release tarball, not apt's rolling gh package, so the
# version is this image's own pin.
ARG GH_VERSION=2.98.0
ARG GH_SHA256=3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de
RUN curl -fsSL -o /tmp/gh.tar.gz \
  "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
  && echo "${GH_SHA256}  /tmp/gh.tar.gz" | sha256sum -c - \
  && tar -xzf /tmp/gh.tar.gz -C /usr/local --strip-components=1 "gh_${GH_VERSION}_linux_amd64/bin/gh" \
  && rm /tmp/gh.tar.gz

# Pinned tarball rather than apt's nodejs package or a floating NodeSource
# script — only to run the two npm-distributed agent CLIs below.
ARG NODE_VERSION=24.19.0
ARG NODE_SHA256=14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647
RUN curl -fsSL -o /tmp/node.tar.xz \
  "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
  && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
  && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 \
  && rm /tmp/node.tar.xz

ARG CLAUDE_CODE_VERSION=2.1.231
ARG CODEX_VERSION=0.149.1
RUN npm install -g --no-fund --no-audit \
  "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
  "@openai/codex@${CODEX_VERSION}" \
  && npm cache clean --force

# Fixed, non-default: an ECS EFS access point maps to a specific numeric
# owner, so this is part of the container's contract with its Terraform.
ARG PLECT_UID=10000
ARG PLECT_GID=10000
# bash, not nologin: tmux resolves default-shell from the passwd entry when
# $SHELL is absent (the supervised services run without one), and a nologin
# pane exits instantly — taking the freshly started tmux server down with it,
# so official.tmux.pane can never produce on a stock deployment.
RUN groupadd -g "${PLECT_GID}" plect \
  && useradd -u "${PLECT_UID}" -g "${PLECT_GID}" -M -s /bin/bash plect

COPY --from=builder /out/bin/plect /usr/local/bin/plect
# Only plugins/, the catalog's own subdir: app/ and contracts/ earned
# their keep only as the builder stage's go.mod-`replace` targets, never
# read by a `plect` invocation resolving this path catalog at runtime.
COPY --from=builder /opt/plect-repo/plugins /opt/plect-repo/plugins
COPY --from=builder /etc/plect /etc/plect
# `plect init` wrote these 0600 as the builder stage's root; the
# unprivileged `plect` user (service/plect-serve/run's `chpst -u`) needs
# to read them.
RUN chmod -R a+rX /etc/plect

COPY --from=source /src/deploy/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --from=source /src/deploy/docker/healthcheck.sh /usr/local/bin/plect-healthcheck
COPY --from=source /src/deploy/docker/service/ /etc/service/
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/plect-healthcheck /etc/service/plect-serve/run

# The runtime layout contract documented in README.md's Persistence
# section: everything under /var/lib/plect (the ECS volume mount point)
# except XDG_RUNTIME_DIR, deliberately not persisted.
ENV PLECT_CONFIG_HOME=/etc/plect \
  HOME=/var/lib/plect/home \
  XDG_DATA_HOME=/var/lib/plect/data \
  XDG_STATE_HOME=/var/lib/plect/state \
  XDG_RUNTIME_DIR=/run/plect \
  PLECT_BUS_SOCKET=/run/plect/bus.sock \
  TMPDIR=/tmp \
  PATH=/usr/local/bin:$PATH

VOLUME ["/var/lib/plect"]

# --unix-socket needs no port/host — the bus never listens on TCP.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/usr/local/bin/plect-healthcheck"]

# entrypoint.sh stays PID 1 rather than exec'ing away: modern bash (>= 4.4)
# reaps arbitrary reparented children when it is PID 1, the one property
# this image needs from an init on top of runit's own supervision.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
