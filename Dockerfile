# syntax=docker/dockerfile:1

# Base images pinned by manifest-list digest so the build is reproducible
# and multi-arch; bump deliberately by resolving the tag again:
#   docker buildx imagetools inspect golang:1.27
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12
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

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
LABEL org.opencontainers.image.title="dev-lease" \
      org.opencontainers.image.description="TTL leases for local Docker Compose stacks" \
      org.opencontainers.image.source="https://github.com/andrius/dev-lease" \
      org.opencontainers.image.licenses="MIT"
# serve must start as root: it reads the Docker socket's group and drops to
# uid 65532 keeping only that group, before any Docker call.
USER 0
COPY --from=build --chown=65532:65532 /data /data
# /usr/local/bin is on the distroless PATH: the documented client commands
# run through `docker exec <container> dev-lease ...`.
COPY --from=build /dev-lease /usr/local/bin/dev-lease
COPY LICENSE /LICENSE
ENTRYPOINT ["/usr/local/bin/dev-lease"]
