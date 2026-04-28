FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/trigger-api ./cmd/trigger-api
RUN go build -o /out/worker ./cmd/worker
RUN go build -o /out/linear-poller ./cmd/linear-poller

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/trigger-api /usr/local/bin/trigger-api
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/linear-poller /usr/local/bin/linear-poller
WORKDIR /app
