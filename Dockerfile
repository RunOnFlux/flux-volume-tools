# Minimal userland for FluxOS app-volume file operations.
#
# The FluxOS volume executor runs each filesystem operation - copy, move, compress,
# extract, mkdir, remove - in a throwaway container built from this image, with only
# the target app volume bind-mounted, a read-only rootfs and no network. Nothing here
# is a long-running service, and the image holds no FluxOS code.
#
# Pinned to a minor tag rather than a patch: the scheduled rebuild picks up Alpine
# patch releases (CVE fixes) without ever jumping a minor or major version.
# Consumers pin the resulting manifest digest, so nodes still receive a
# byte-identical image.
FROM alpine:3.24

# Alpine ships busybox applets for all of these. Two are not sufficient:
#
#   tar    busybox tar has no --no-same-owner, so an archive's recorded uids land
#          verbatim and extracted files can end up unreadable by the app that owns
#          the volume.
#   unzip  busybox's applet has no zip64 support, capping archives at 4 GB.
#
# coreutils is installed for GNU cp/mv semantics throughout. busybox does implement
# -T, so this one is about predictable behaviour rather than a missing flag.
RUN apk add --no-cache coreutils tar unzip

# No default command by design: the executor always supplies argv, and an accidental
# `docker run` of this image should do nothing.
ENTRYPOINT []
CMD ["/bin/false"]
