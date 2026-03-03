# Multi-stage build: Go bridge binary + Ubuntu base with Claude CLI
# Produces a self-contained image that runs anywhere Docker runs

# Stage 1: Build the Go bridge binary
FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY bridge-go/go.mod bridge-go/go.sum ./
RUN go mod download

COPY bridge-go/ .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o pai-bridge .

# Stage 2: Ubuntu base with Claude CLI and bridge
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# System dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl ffmpeg jq \
    && rm -rf /var/lib/apt/lists/*

# Create unprivileged user for Claude subprocesses
RUN useradd -m -s /bin/bash claude

# Install bridge binary
COPY --from=builder /build/pai-bridge /usr/local/bin/pai-bridge
RUN chmod +x /usr/local/bin/pai-bridge

# Install entrypoint
COPY k8s/entrypoint.sh /usr/local/bin/entrypoint-bridge.sh
RUN chmod +x /usr/local/bin/entrypoint-bridge.sh

# Install Claude Code CLI
USER claude
RUN curl -fsSL https://claude.ai/install.sh | bash
USER root
RUN cp /home/claude/.local/bin/claude /usr/local/bin/claude && chmod +x /usr/local/bin/claude

# Create data mount point
RUN mkdir -p /mnt/pai-data && chown claude:claude /mnt/pai-data

ENTRYPOINT ["/usr/local/bin/entrypoint-bridge.sh"]
