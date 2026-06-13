ARG GO_VERSION=1.25.11
FROM node:22-bookworm AS dashboard
WORKDIR /src/cmd/worker/dashboard
COPY cmd/worker/dashboard/package.json cmd/worker/dashboard/package-lock.json ./
RUN npm ci
COPY cmd/worker/dashboard/ ./
RUN npm run build

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
# Copy go.sum alongside go.mod so `go mod download` can verify module
# checksums, and so the cached dependency layer is keyed on both files
# (matching the CI cache key) — a go.sum-only change now invalidates it (#369).
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
COPY --from=dashboard /src/cmd/worker/dashboard/dist ./cmd/worker/dashboard/dist
# Stamp main.version from a build ARG (#796): .dockerignore drops .git, so the
# Go toolchain's vcs.revision fallback is unavailable in the image. Pass
# --build-arg VERSION=<tag> to stamp a real version; defaults to "devel" for a
# plain `docker build`.
ARG VERSION=devel
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/worker ./cmd/worker

FROM debian:bookworm-slim AS worker
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends ca-certificates git openssh-client ripgrep wget && rm -rf /var/lib/apt/lists/*
# Run as a dedicated unprivileged user. The worker prepares git workspaces and
# runs agent/hook/verify/git commands, so root-in-container would widen the
# blast radius of any compromised runner or hostile repository content (#365).
# The named workspaces volume mounts empty onto /workspaces and inherits this
# directory's aiops ownership, and /home/aiops/.ssh is pre-created 0700 so the
# Compose deploy-key binds land in a directory ssh will accept.
#
# AIOPS_UID/AIOPS_GID must match the host owner of the bind-mounted deploy key:
# the key is mounted read-only with its host ownership/permissions (0600)
# preserved, so the in-container user can only read it when their UID matches
# the host UID that ran ssh-keygen. Default 1000 covers the common single-user
# Linux host; operators on a different UID rebuild with
# `--build-arg AIOPS_UID=$(id -u) --build-arg AIOPS_GID=$(id -g)` (Compose reads
# these from .env). See deploy/ssh/README.md.
ARG AIOPS_UID=1000
ARG AIOPS_GID=1000
# A colliding GID is harmless (group identity does not affect ssh key
# resolution), so reuse an existing group for it. The UID, however, must be
# unused: OpenSSH resolves ~/.ssh via getpwuid(), so the worker can only find
# the mounted deploy key when its UID owns /home/aiops. Rather than silently
# mis-resolve to a system account's home on a UID collision, fail the build
# with guidance. Host UIDs >=1000 (the documented default and override range)
# are unused in debian-slim, so the common path always creates aiops cleanly.
RUN set -eux; \
    if ! getent group "${AIOPS_GID}" >/dev/null; then groupadd --gid "${AIOPS_GID}" aiops; fi; \
    if getent passwd "${AIOPS_UID}" >/dev/null; then \
        echo "AIOPS_UID=${AIOPS_UID} already exists in the base image; pick an unused UID (e.g. your host id -u, normally >=1000) so the worker owns /home/aiops for ssh key resolution" >&2; \
        exit 1; \
    fi; \
    useradd --no-log-init --create-home --home-dir /home/aiops --uid "${AIOPS_UID}" --gid "${AIOPS_GID}" --shell /bin/bash aiops; \
    mkdir -p /app /workspaces /home/aiops/.ssh; \
    chown -R "${AIOPS_UID}:${AIOPS_GID}" /app /workspaces /home/aiops; \
    chmod 700 /home/aiops/.ssh
ENV HOME=/home/aiops
COPY --from=build /out/worker /usr/local/bin/worker
COPY deploy/aiops-secret-entrypoint.sh /usr/local/bin/aiops-secret-entrypoint
RUN chmod 0755 /usr/local/bin/aiops-secret-entrypoint
WORKDIR /app
USER ${AIOPS_UID}:${AIOPS_GID}
# Default to the worker so `docker run <image>` is useful out of the box (#370).
# CMD (not ENTRYPOINT) keeps it overridable for diagnostics without argument
# duplication in Compose.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD wget -qO- "http://127.0.0.1:${AIOPS_HEALTHCHECK_PORT:-4000}/livez" >/dev/null 2>&1 || exit 1
CMD ["worker"]

FROM worker AS codex-worker
ARG AIOPS_UID=1000
ARG AIOPS_GID=1000
USER root
COPY --from=build /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
ARG CODEX_CLI_VERSION=0.137.0
ARG TARGETARCH
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) codex_arch="x86_64-unknown-linux-musl"; codex_sha="b364488097efd12da3b5ffb1b0099cabd8b377b1f97043158f7f771b1456953f" ;; \
      arm64) codex_arch="aarch64-unknown-linux-musl"; codex_sha="8756c80ad058199676d058bbd919812466d796886e7574a57c4e007f766e707c" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}; supported: amd64, arm64" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/openai/codex/releases/download/rust-v${CODEX_CLI_VERSION}/codex-package-${codex_arch}.tar.gz"; \
    wget -qO /tmp/codex.tar.gz "${url}"; \
    echo "${codex_sha}  /tmp/codex.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/codex; \
    tar -xzf /tmp/codex.tar.gz -C /opt/codex; \
    ln -sf /opt/codex/bin/codex /usr/local/bin/codex; \
    rm -f /tmp/codex.tar.gz; \
    go version; \
    command -v gofmt; \
    codex --version
# gh CLI is required by the codex-worker image so the documented
# `aiops-secret-entrypoint` path can wire `/run/secrets/github_token` into
# `gh auth setup-git` and `worker --doctor --github-issue` can run inside the
# container. The plain `worker` target does not need it.
ARG GH_CLI_VERSION=2.92.0
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) gh_arch="linux_amd64"; gh_sha="b57848131bdf0c229cd35e1f2a51aa718199858b2e728410b37e89a428943ec4" ;; \
      arm64) gh_arch="linux_arm64"; gh_sha="c2248526dd0160c08d3fccca2332c3c1a07c15a78b23978e77735f1b5a18cfee" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}; supported: amd64, arm64" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_${gh_arch}.tar.gz"; \
    wget -qO /tmp/gh.tar.gz "${url}"; \
    echo "${gh_sha}  /tmp/gh.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/gh.tar.gz -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_CLI_VERSION}_${gh_arch}/bin/gh" /usr/local/bin/gh; \
    rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_${gh_arch}"; \
    gh --version
USER ${AIOPS_UID}:${AIOPS_GID}

# Keep `docker build .` on the baseline worker image; codex-worker is opt-in.
FROM worker AS default
