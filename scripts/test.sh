#!/bin/sh
# Everything CI checks, in one command.
#
# Both halves, deliberately. What flux-op DECIDES is in ./cmd; what the IMAGE
# does is in ./test/container, and that half needs a build tag and a built image
# - so `go test ./...` runs none of it and reports success. A stale test helper
# sat failing there for a whole session behind exactly that gap.
set -eu

cd "$(dirname "$0")/.."

say() { printf '\n== %s\n' "$1"; }

say "gofmt"
unformatted="$(gofmt -l cmd test)"
if [ -n "$unformatted" ]; then
	echo "not gofmt'd: $unformatted" >&2
	exit 1
fi

say "go vet"
go vet ./...
# The container tests are behind a build tag, so the plain vet above never sees
# them. They are the half most likely to rot: nothing compiles them by default.
go vet -tags docker ./...

say "unit tests"
go test ./cmd/... -race -count=1

say "cross-compilation"
# Both architectures are published, and identity() is per-platform - so a change
# that only builds where it was written is a change that breaks half the fleet.
for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o /dev/null ./cmd/flux-op
	echo "linux/$arch ok"
done
# Exercises the !linux build, which is how this stays testable on a Mac.
go build -o /dev/null ./cmd/flux-op
echo "$(go env GOOS)/$(go env GOARCH) ok"

if ! command -v docker >/dev/null 2>&1; then
	echo
	echo "docker is not available, so what the IMAGE does was not tested." >&2
	echo "That is half the suite. Do not read this as a pass." >&2
	exit 1
fi

say "building the image under test"
docker build -t flux-volume-tools:test .

say "container tests"
# -count=1 because the image is an input the test cache cannot see: with a warm
# cache a rebuilt image reports the previous result without starting a container.
go test -tags docker -count=1 ./test/container/

say "all green"
