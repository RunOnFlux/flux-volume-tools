# Minimal userland for FluxOS app-volume file operations.
#
# The FluxOS volume executor runs each filesystem operation - copy, move, compress,
# extract, upload, mkdir, remove - in a throwaway container built from this image,
# with only the target app volume bind-mounted, a read-only rootfs and no network.
# Nothing here is a long-running service, and the image holds no FluxOS code.

# Built on the NATIVE platform and cross-compiled to the target, rather than
# compiled under emulation once per architecture. flux-op needs no cgo, so this
# costs a single environment variable and keeps the arm64 build as fast as the
# amd64 one.
#
# Pinned to a minor tag, like the runtime below.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
# Dependencies resolved in their own layer, so a change to the source does not
# refetch them. go.sum is what pins them: x/sys is the only one, and flux-op
# needs it for statx, which is the syscall that reports an object's creation
# time and whether the filesystem actually keeps one.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd

ARG TARGETARCH
# -trimpath so the binary records no build-host paths, and no debug information,
# because nothing debugs this in place: it runs in a container that is gone
# moments later.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /flux-op ./cmd/flux-op

# Pinned to a minor tag rather than a patch: the scheduled rebuild picks up Alpine
# patch releases (CVE fixes) without ever jumping a minor or major version.
# Consumers pin the resulting manifest digest, so nodes still receive a
# byte-identical image.
FROM alpine:3.24

# Alpine ships busybox applets for most of these. Three are not sufficient:
#
#   tar    busybox tar has no --no-same-owner, so an archive's recorded uids land
#          verbatim and extracted files can end up unreadable by the app that owns
#          the volume.
#   unzip  busybox's applet has no zip64 support, capping archives at 4 GB.
#   zip    busybox has no zip applet at all, and creating archives is half of
#          what this image is for.
#
# coreutils is installed for GNU cp/mv semantics throughout. busybox does implement
# -T, so this one is about predictable behaviour rather than a missing flag.
RUN apk add --no-cache coreutils tar zip unzip

# Runs a command into a staging directory - or writes the caller's own stream
# into one - and publishes the result with an atomic rename, so an operation that
# fails, is cancelled, or is interrupted by a power cut leaves the caller's data
# exactly as it was. Living in the image means one container does the work AND
# the publish; see the source for why that matters.
#
# Statically linked, so nothing installed above can change how it behaves.
COPY --from=build --chmod=0755 /flux-op /usr/local/bin/flux-op

# No default command by design: the executor always supplies argv, and an accidental
# `docker run` of this image should do nothing.
ENTRYPOINT []
CMD ["/bin/false"]
