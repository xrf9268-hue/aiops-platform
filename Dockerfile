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
RUN useradd --create-home --uid 10001 --shell /bin/bash aiops \
    && mkdir -p /app /workspaces /home/aiops/.ssh \
    && chown -R aiops:aiops /app /workspaces /home/aiops \
    && chmod 700 /home/aiops/.ssh
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/linear-poller /usr/local/bin/linear-poller
COPY --from=build /out/gitea-poller /usr/local/bin/gitea-poller
WORKDIR /app
USER aiops
