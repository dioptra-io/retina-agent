FROM docker.io/library/golang:1.26.1-bookworm AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o retina-agent ./cmd/retina-agent

FROM docker.io/library/debian:bookworm-slim

LABEL org.opencontainers.image.authors="Dioptra <contact@dioptra.io>"

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install --no-install-recommends --yes \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

RUN curl -L https://github.com/dioptra-io/caracal/releases/download/v0.15.3/caracal-linux-amd64 \
        > /usr/bin/caracal \
    && chmod +x /usr/bin/caracal

WORKDIR /app
COPY --from=builder /build/retina-agent .

ENTRYPOINT ["./retina-agent"]
