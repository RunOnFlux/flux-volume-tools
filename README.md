# flux-volume-tools

Minimal container image used by FluxOS to perform file operations on an
application's volume.

```
ghcr.io/runonflux/flux-volume-tools
```

## What it is for

FluxOS exposes a file API over an application's volume — the dashboard file
browsers for WordPress, Minecraft, Palworld and Rust are built on it. Those
operations previously ran on the **host** as root. This image lets them run inside a
throwaway container instead, with only the target volume mounted.

Each operation gets a fresh container configured with:

| Setting | Effect |
|---|---|
| the app's volume bind-mounted, nothing else | a path that escapes the mount has nowhere to land |
| `ReadonlyRootfs: true` | writes outside the volume fail rather than succeeding into a discarded layer |
| `NetworkMode: none` | a malicious archive cannot phone home |
| `CapDrop: ALL` then `CapAdd: CHOWN, FOWNER, DAC_OVERRIDE` | see below — dropping all three breaks `cp -a` |
| pids and memory limits | bounds a runaway archive |
| `AutoRemove: true` | no stopped container is left for a prune to find |

Containment therefore comes from the container having nowhere to escape *to*,
rather than from a sequence of path checks each caller must remember to run.

Measured on a node, copying a directory owned by uid 33:

```
CapDrop: ALL                            -> files land 0:0
CapDrop: ALL + CHOWN,FOWNER,DAC_OVERRIDE -> files land 33:33 (matches source)
```

`cp -a` cannot restore ownership without `CAP_CHOWN`, and it does not fail when it
can't — it exits 0 having written root-owned files. An app running as a non-root
user then loses access to its own data, silently. Those three capabilities are the
minimum for `cp -a` to mean what it says; the rest stay dropped.

Extraction is the mirror image: archive-recorded uids are attacker-supplied, so
`tar --no-same-owner` is used to ignore them (verified: files land `0:0` regardless
of what the archive claims), and ownership is then set to match the destination's
parent.

A second benefit: FluxOS must hold `/var/run/docker.sock` to function at all, so
routing filesystem work through Docker adds no new privilege — whereas running
`sudo cp` on the host requires sudoers rules that the planned demotion of FluxOS to
an unprivileged system user is meant to remove.

## `flux-op` — publishing a result atomically

```
flux-op [--mkdir] <staging> <destination> -- <command> [args...]
```

The command writes into `<staging>`, never into `<destination>`. Only on success
is the result moved into place. A command that fails, a container that is killed,
and a node that loses power all leave `<destination>` exactly as it was.

Publishing goes through a swap — move the old entry aside, move the new one in,
delete the old — rather than a delete-then-rename. `rename(2)` refuses a
non-empty directory as its target and cannot replace a file with a directory at
all, so `mv` alone would have to delete first, and a crash in that window loses
the destination outright. Both renames in the swap are atomic, so the worst a
crash leaves is the previous data under `.flux-old-*`.

Leftovers are named for a startup sweep to recognise:

| Left behind | Means | Recovery |
|---|---|---|
| `.flux-op-*` | the operation never completed | delete; nobody is waiting for it |
| `.flux-old-<id>`, destination missing | crash between the two renames | rename it back |
| `.flux-old-<id>`, destination present | the swap completed | delete |
| `.flux-old-<id>.dest` | records where `.flux-old-<id>` belongs | delete with it |

The `.dest` marker is written *before* the old entry is moved aside. Without it a
crash between the two renames would leave the caller's only copy of that data
under a name that says nothing about where it came from, and the sweep would
delete it.

This is **atomic visibility, not durability.** You will never see a half-written
result at the destination path. A power cut seconds after a copy can still leave
files whose contents had not reached disk — that is true of `cp` on Linux
generally and is not something this changes.

It lives in the image rather than in the caller so one container does the work
*and* the publish: the "did it succeed" and "put it in place" decisions cannot
drift apart, and no second container spawn is needed per operation. Operands
arrive as positional parameters and are never interpolated into a command
string.

## Contract

The image guarantees these binaries, with GNU / Info-ZIP semantics:

| Binary | Package | Why not the busybox applet |
|---|---|---|
| `cp`, `mv` | `coreutils` | predictable GNU semantics (busybox does implement `-T`) |
| `tar` | `tar` | busybox tar has no `--no-same-owner` |
| `unzip` | `unzip` | busybox's applet has no zip64, capping archives at 4 GB |
| `gzip` | busybox | applet is sufficient |

There is no entrypoint. The executor always supplies argv.

## Consuming it

Pin the **manifest list** digest, never a tag:

```
ghcr.io/runonflux/flux-volume-tools@sha256:<manifest-list-digest>
```

The image is published for `linux/amd64` and `linux/arm64`. Pinning the manifest
list digest resolves to the right architecture on each node; pinning a per-architecture
digest instead would work on x86 and fail on every arm node.

## Releasing

`.github/workflows/build.yml` builds both architectures and pushes to GHCR on:

- a push to `main` (tagged `latest`)
- a version tag `v*` (tagged with the version)
- a weekly schedule, which is how Alpine patch releases reach the image
- manual dispatch

It requires no secrets — GHCR publishing uses the automatic `GITHUB_TOKEN` with
`packages: write`.

Every build runs a smoke test first that asserts each binary above is the expected
implementation, so a change in Alpine's packaging fails the build rather than
shipping a busybox applet into production. The published manifest digest is written
to the workflow run summary; that is the value to pin in FluxOS.
