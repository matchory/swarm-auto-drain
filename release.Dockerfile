# syntax=docker/dockerfile:1.7
#
# Release-time image: copies the prebuilt static binary from the
# matrix build job in .github/workflows/release.yml. The per-arch
# binaries are staged into binaries/linux/<arch>/swarm-auto-drain
# before `docker buildx build --platform linux/amd64,linux/arm64`
# resolves TARGETARCH per platform.
#
# For `docker build .` from a workstation, use the top-level Dockerfile
# which builds Go from source.

FROM gcr.io/distroless/static:nonroot

ARG TARGETARCH

LABEL org.opencontainers.image.title="swarm-auto-drain" \
      org.opencontainers.image.description="Kured-style safe automated reboots for Docker Swarm." \
      org.opencontainers.image.license="MIT" \
      org.opencontainers.image.url="https://github.com/matchory/swarm-auto-drain"

COPY --link binaries/linux/${TARGETARCH}/swarm-auto-drain /usr/local/bin/swarm-auto-drain

# Root, not nonroot — see the top-level Dockerfile for the full
# rationale (docker.sock access, Chown of the bind-mounted socket
# directory, SO_PEERCRED gating).
USER root:root
EXPOSE 7777
ENTRYPOINT ["/usr/local/bin/swarm-auto-drain"]
CMD ["run"]
