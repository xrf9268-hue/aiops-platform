ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/worker ./cmd/worker
RUN go build -o /out/linear-poller ./cmd/linear-poller
RUN go build -o /out/gitea-poller ./cmd/gitea-poller

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client && rm -rf /var/lib/apt/lists/*
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
COPY --from=build /out/linear-poller /usr/local/bin/linear-poller
COPY --from=build /out/gitea-poller /usr/local/bin/gitea-poller
WORKDIR /app
USER ${AIOPS_UID}:${AIOPS_GID}
