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
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/linear-poller /usr/local/bin/linear-poller
COPY --from=build /out/gitea-poller /usr/local/bin/gitea-poller
WORKDIR /app
# Default to the worker so `docker run <image>` is useful out of the box (#370).
# CMD (not ENTRYPOINT) keeps it overridable: Compose's `command: ["worker"]`
# and the poller services' `command: ["linear-poller", ...]` replace it without
# argument duplication, and `docker run <image> linear-poller ...` still works.
CMD ["worker"]
