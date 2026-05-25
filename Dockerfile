# syntax=docker/dockerfile:1.7

# --- Build stage --------------------------------------------------------------
FROM golang:1.23-alpine AS build
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
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/swarm-auto-drain /usr/local/bin/swarm-auto-drain
USER nonroot:nonroot
EXPOSE 7777
ENTRYPOINT ["/usr/local/bin/swarm-auto-drain"]
