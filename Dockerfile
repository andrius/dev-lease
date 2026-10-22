# syntax=docker/dockerfile:1

# The build image is pinned by manifest-list digest so the build is
# reproducible and multi-arch; bump deliberately by resolving the tag again:
#   docker buildx imagetools inspect golang:1.27
# The build stage runs on the build platform (no emulation on CI runners)
# and cross-compiles with GOOS/GOARCH, which buildx provides.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:957861185dbf29d93668ace41e011d091c6e779953d47f0316ef2cc58e28fe2b AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /dev-lease ./cmd/dev-lease
# The state directory belongs to the non-root uid the binary drops to.
RUN mkdir -p /data && chown 65532:65532 /data

# The runtime image is empty: the binary is static and needs nothing from a
# base (no CA certificates, no time zones, no user lookup, no /tmp; the
# state file's temp file sits next to it on /data). The serve container
# holds the Docker socket, so it carries no shell and no other tools.
FROM scratch
# The release reads these values (.github/scripts/image-meta.sh) and sets
# them as annotations on the index and the platform manifests too, with
# version, revision and created; a value must not contain a comma.
LABEL org.opencontainers.image.title="dev-lease" \
      org.opencontainers.image.description="Stops the Docker Compose stacks nobody uses and removes them after a grace period. Leases per stack with a web dashboard and an MCP server for AI agents." \
      org.opencontainers.image.url="https://github.com/andrius/dev-lease" \
      org.opencontainers.image.source="https://github.com/andrius/dev-lease" \
      org.opencontainers.image.documentation="https://github.com/andrius/dev-lease#readme" \
      org.opencontainers.image.licenses="MIT"
# serve must start as root: it reads the Docker socket's group and drops to
# uid 65532 keeping only that group, before any Docker call.
USER 0
COPY --from=build --chown=65532:65532 /data /data
# The documented client commands run through
# `docker exec <container> dev-lease ...`, which finds the binary on PATH.
ENV PATH=/usr/local/bin
COPY --from=build /dev-lease /usr/local/bin/dev-lease
COPY LICENSE /LICENSE
ENTRYPOINT ["/usr/local/bin/dev-lease"]
