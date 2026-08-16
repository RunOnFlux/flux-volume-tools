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
flux-op --id <id> --root <dir> [--discard-staging] [--mkdir] [--max-bytes N] [--ordinary-only] \
        [--no-replace] <staging> <destination> -- [command [args...]]
```

The command writes into `<staging>`, never into `<destination>`. Only on success
is the result moved into place. A command that fails, a container that is killed,
and a node that loses power all leave `<destination>` exactly as it was.

**The command may be empty**, and that is how a move and a rename are expressed:
the caller's source already *is* the result, so publishing it is the whole
operation. With `--mkdir` and no command, creating a folder is the same shape:
staging is made here and published under the name the caller asked for.

**`--no-replace` publishes only onto a free name**, and exits **5** when the name
is taken. Without it a publish replaces whatever is at the destination, which is
what an upload and an overwriting move mean.

The refusal is the rename's own — `RENAME_NOREPLACE`, the same call as the
exchange below with the opposite flag — so it is not a look followed by a move.
A caller that checked first would be answering for a moment that has passed: the
application whose volume this is runs throughout and can take the name in
between. Here the kernel compares and moves under the parent directory's lock, so
"it already exists" is true of the instant nothing was written. It is also the
only form that treats a file, an empty directory and a dangling symlink alike,
each of which occupies the name.

Exit **5** rather than a message, because the caller shows an app owner a
different sentence for a name in use than for an operation that failed, and a
status is the one part of a failure that does not depend on which tool inside
this image produced it.

`--id` and `--root` name what an interrupted publish leaves behind and where —
`<root>/.flux-op-<id>`. Neither is derived from `<staging>`, because `<staging>`
is not always something this script created: for a move it is the caller's own
path, at whatever depth they keep it. A name derived from it is indistinguishable
from a folder the user chose, and a location derived from it lands outside the
one directory the sweep reads.

`--discard-staging` says the staging operand is scratch this operation created,
so a FAILURE may throw it away. **Without it, a failure never deletes staging** —
for a move that operand is the only copy of the caller's data. Opt-in rather than
opt-out, because the safe behaviour has to be the one a forgetful caller gets.

It says nothing about success. Once the exchange below has happened the staging
name holds the caller's PREVIOUS data and the destination holds what they asked
for, so that entry is removed either way.

A cancellation arrives as `SIGTERM` (docker `stop`, not `kill`). The command runs
as a child so this script survives to trap it, forwards the signal — a container
stop reaches only PID 1 — and reclaims staging before exiting. `SIGKILL` bypasses
all of that, and the startup sweep remains the backstop for it.

Publishing is ONE step: `renameat2(RENAME_EXCHANGE)` swaps the staging entry and
the destination. There is no in-between state to be interrupted in — either the
entries are swapped or they are not, and the destination holds something complete
either way. A plain `mv` cannot do this: `rename(2)` refuses a non-empty directory
as its target and cannot replace a file with a directory at all, so it would have
to delete first, and a crash in that window loses the destination outright.

Where the platform offers no atomic exchange the publish refuses rather than
falling back. A guarantee that holds on some nodes and not others, decided by
something no caller can see, is worse than none.

Leftovers are named for a startup sweep to recognise:

| Left behind | Means | Recovery |
|---|---|---|
| `.flux-op-<id>` | the operation never completed, or was interrupted around the exchange | delete; nobody is waiting for it, and the destination is complete either way |

That is the whole recovery rule, and it is unconditional: no marker to parse, no
recorded identity to compare, and nothing read from a directory the app owner can
also write to.

Recovery must not depend on identifying an entry, and that is not a stylistic
preference. On ext4, measured over 200 back-to-back creations, a reused inode
number came back **every** time and the creation time collided **108** times — so
a sweep deciding whether to delete somebody's only copy on that evidence is
deciding on a coincidence. Removing the window the identity existed to survive is
what makes the question go away.

`--max-bytes` caps what the command may leave in staging, and `--ordinary-only`
refuses a result holding anything that is not ordinary data — symlinks and hard
links, which reach outside the result, and FIFOs, sockets and device nodes,
which are not data at all. A FIFO matters as much as a link: whatever opens one
without `O_NONBLOCK` waits for a writer that never comes, and `tar` both carries
and recreates them. Both are checked **after**
the command runs rather than from what an archive declares about itself: those
figures are written by whoever built the archive, so a bomb simply lies about
them. Staging is discarded on breach and the destination is never touched.

A failure anywhere before the publish removes staging immediately rather than
leaving it for the startup sweep — a refused extraction gives the volume its
space back at once, which matters because size is often why it was refused. That
cleanup is disarmed the moment the swap begins, since past that point the same
names refer to the caller's previous data.

This is **atomic visibility, not durability.** You will never see a half-written
result at the destination path. A power cut seconds after a copy can still leave
files whose contents had not reached disk — that is true of `cp` on Linux
generally and is not something this changes.

It lives in the image rather than in the caller so one container does the work
*and* the publish: the "did it succeed" and "put it in place" decisions cannot
drift apart, and no second container spawn is needed per operation. Operands
arrive as positional parameters and are never interpolated into a command
string.

## What a hostile archive cannot do

Extraction is the one operation that runs attacker-supplied *structure* rather
than attacker-supplied bytes, so it is worth recording what stops each classic
archive trick — and which layer actually stops it, because several are not this
program's doing.

| Attempt | Outcome | What stops it |
|---|---|---|
| member named `../../../../etc/evil` | refused, nothing extracted | GNU tar — *"Member name contains '..'"* |
| member named `/etc/hostname` | extracted as `etc/hostname` **inside** the target | GNU tar strips the leading `/` |
| symlink, then a member written *through* it | the link is replaced by a real directory; nothing reaches the target | GNU tar |
| a symlink left in the result | refused | `--ordinary-only` |
| a hard link to data outside the result | refused | `--ordinary-only` |
| a FIFO or socket | refused | `--ordinary-only` |
| a device node | cannot be created at all | `CAP_MKNOD` is dropped; FluxOS also mounts the volume `nodev` |
| a setuid binary | the bit survives extraction, and is **inert** | FluxOS mounts the app volume `nosuid` |
| an archive of many tiny files | refused once what it **occupies** exceeds the ceiling | `--max-bytes`, measured on what landed rather than on what the archive declares |
| anything reaching off the volume | nowhere to land | no network, read-only rootfs, the volume is the only mount |

The tiny-files row is measured the way `du` reports by default, not `du -sb`. A file
occupies whole blocks, so twenty thousand one-byte files are 20 KB by their own
account and 82 MB on an ext4 volume — and the ceiling this is compared against is
the volume's free space, which is a count of blocks. Measuring what the files say
would compare two different kinds of number, and pass an extraction that had
already exceeded the limit four thousand times over.

Two of these are worth knowing rather than assuming.

**The symlink defences are tar's, not ours.** `--ordinary-only` refuses a link
*after* the command has run, so it catches a link left in the result — but a
write *through* one would already have happened by then. What prevents it is GNU
tar replacing the link with a directory. That is a reason the GNU pinning in the
Contract below matters more than it appears: busybox tar is not the same program.

**The setuid bit is preserved, and that is fine.** tar restores it and this
program does not strip it, because the mount is where it is neutralised — one
place, covering every route such a file can arrive by, including a plain copy.

Verified by running each case in a container configured exactly as the executor
configures one, on a filesystem that can hold the bits being tested. A bind mount
from a macOS host silently drops setuid, which makes that row look safe when it
is not being tested at all.

## Contract

The image guarantees these binaries, with GNU / Info-ZIP semantics:

| Binary | Package | Why not the busybox applet |
|---|---|---|
| `cp`, `mv` | `coreutils` | predictable GNU semantics (busybox does implement `-T`) |
| `tar` | `tar` | busybox tar has no `--no-same-owner` |
| `unzip` | `unzip` | busybox's applet has no zip64, capping archives at 4 GB |
| `zip` | `zip` | busybox has no zip applet at all |
| `gzip` | busybox | applet is sufficient |

`mv` is guaranteed but never driven: a move is a publish whose source is already
the result, so `flux-op` renames rather than running anything. It is in the
contract because the image promises it, and the test holds the image to it —
`cp`'s guarantee was checked by grepping `--help` for `-T`, which busybox also
lists and also implements, so the one case that existed to catch coreutils being
dropped passed without it.

There is no entrypoint. The executor always supplies argv.

## Consuming it

Pin the **manifest list** digest, never a tag:

```
ghcr.io/runonflux/flux-volume-tools@sha256:<manifest-list-digest>
```

The image is published for `linux/amd64` and `linux/arm64`. Pinning the manifest
list digest resolves to the right architecture on each node; pinning a per-architecture
digest instead would work on x86 and fail on every arm node.

## Testing

```
./scripts/test.sh
```

Runs everything CI checks: formatting, `go vet` — twice, because the container
tests sit behind a build tag the plain run never compiles — the unit tests with
`-race`, both published architectures plus the host build, then it builds the
image and runs the container suite against it.

The two halves answer different questions and neither substitutes for the other:

| | what it covers |
|---|---|
| `go test ./cmd/...` | what `flux-op` **decides** — the exchange, the ceiling, the recovery rule, what is refused |
| `go test -tags docker ./test/container/` | what the **image** does, in a container configured exactly as the executor configures it |

Run the second half through the script rather than by hand. It needs a build tag
*and* a freshly built image, so `go test ./...` runs none of it and still reports
success — which is how a stale helper in that suite once sat failing for a whole
session while the suite was believed green.

`-count=1` is not optional for the container half: the image is an input the test
cache cannot see, so rebuilding it and running again reports the previous result
without ever starting a container. The script passes it.

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
shipping a busybox applet into production.

**The image that is published is the image that was tested.** Each architecture is
built once, pushed by digest with no tag on it, and then pulled back out of the
registry and tested through it. Only once both have passed does a final job
assemble the manifest list from those digests — so a tag never names bytes that
nothing ran. It used to build a second time to publish, and since the Dockerfile
pins a minor Alpine tag and installs unpinned packages, two builds minutes apart
were not required to agree.

A digest carrying no tag is not published in any useful sense — nothing can
resolve to it without already knowing it — so a failed run leaves an unreferenced
digest and no tag.

The run summary prints the whole pin, ready to paste: the tag and both
per-architecture image ids. The ids are the digests of each image's own *config*,
which is what FluxOS verifies against, and they cannot be derived from the
manifest list digest alone.

Actions are pinned to commit SHAs rather than to major tags. A major tag is
mutable and every job holds `packages: write`.
