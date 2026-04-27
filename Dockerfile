FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/trigger-api ./cmd/trigger-api
RUN go build -o /out/worker ./cmd/worker

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/trigger-api /usr/local/bin/trigger-api
COPY --from=build /out/worker /usr/local/bin/worker
WORKDIR /app
