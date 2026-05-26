# syntax=docker/dockerfile:1.7

# --- Build stage --------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache modules.
COPY go.mod go.sum* ./
RUN go mod download

# Build statically-linked binary.
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/swarm-auto-drain \
    ./cmd/swarm-auto-drain

# --- Runtime stage ------------------------------------------------------------
# Runs as root (not nonroot) because:
#   1. The bind-mounted /var/run/docker.sock requires either root or the
#      `docker` group, which distroless doesn't carry.
#   2. openUnixListener calls os.Chown(parent, -1, UNIX_SOCKET_GID) on the
#      bind-mounted /var/run/swarm-auto-drain — only root can change
#      ownership to a group it doesn't belong to.
#   3. The kernel SO_PEERCRED gate (identity.HostTrust) requires the
#      *connecting* process to be UID 0, not the daemon itself, but the
#      daemon needs root to enforce that boundary on its socket.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/swarm-auto-drain /usr/local/bin/swarm-auto-drain
USER root:root
EXPOSE 7777
# ENTRYPOINT pins the binary; CMD supplies the default subcommand.
# This lets operators run ad-hoc subcommands against the image without
# installing the binary on a workstation, e.g.:
#   docker run --rm ghcr.io/matchory/swarm-auto-drain:vX.Y.Z stack > stack.yml
ENTRYPOINT ["/usr/local/bin/swarm-auto-drain"]
CMD ["run"]
