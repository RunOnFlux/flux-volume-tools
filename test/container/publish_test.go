//go:build docker

package container

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// How a collision at the destination is resolved, on the filesystem and kernel
// this actually runs on - the half a unit test cannot answer for, since the
// exchange, the no-replace refusal and the merge are three different rename
// operations and the executor's seccomp profile decides which arrive at all.
//
// This is the table the file-operation endpoints are documented against.
func TestPublishResolvesCollisionsByKind(t *testing.T) {
	cases := []struct {
		name        string
		seed        string
		merge       bool
		wantExit    int
		wantPresent []string
		wantAbsent  []string
		wantContent map[string]string
	}{
		{
			name:        "a new destination is one rename",
			seed:        `mkdir -p /work/src && echo new > /work/src/f`,
			wantExit:    0,
			wantPresent: []string{"dest/f"},
		},
		{
			name:        "a file replaces a file",
			seed:        `echo new > /work/src && echo old > /work/dest`,
			wantExit:    0,
			wantContent: map[string]string{"dest": "new"},
		},
		{
			name:        "a directory may not replace a file",
			seed:        `mkdir -p /work/src && echo new > /work/src/f && echo original > /work/dest`,
			wantExit:    6,
			wantContent: map[string]string{"dest": "original"},
		},
		{
			name:        "a file may not replace a directory",
			seed:        `mkdir -p /work/dest && echo new > /work/src && echo keepme > /work/dest/keep`,
			wantExit:    6,
			wantPresent: []string{"dest/keep"},
		},
		{
			name:        "a directory is not replaced wholesale without a merge",
			seed:        `mkdir -p /work/src /work/dest && echo new > /work/src/f && echo keepme > /work/dest/keep`,
			wantExit:    6,
			wantPresent: []string{"dest/keep"},
			wantAbsent:  []string{"dest/f"},
		},
		{
			name:        "a directory merges into a directory",
			seed:        `mkdir -p /work/src /work/dest && echo new > /work/src/added && echo fromsrc > /work/src/shared && echo old > /work/dest/kept && echo fromdst > /work/dest/shared`,
			merge:       true,
			wantExit:    0,
			wantPresent: []string{"dest/kept", "dest/added"},
			wantContent: map[string]string{"dest/shared": "fromsrc"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			volume := volumeDir(t)
			seed(t, volume, testCase.seed)

			staging := "/work/.flux-op-" + operationID
			extra := []string{"--discard-staging"}
			if testCase.merge {
				extra = append(extra, "--merge")
			}
			extra = append(extra, staging, "/work/dest", "--")
			result := fluxOp(t, volume, "",
				append(baseArgs(extra...), "cp", "-a", "-T", "/work/src", staging)...)

			if result.exit != testCase.wantExit {
				t.Fatalf("exit %d, want %d:\n%s", result.exit, testCase.wantExit, result.output)
			}
			for _, name := range testCase.wantPresent {
				if !exists(t, volume, name) {
					t.Errorf("%s is not there\n%s", name, tree(t, volume))
				}
			}
			for _, name := range testCase.wantAbsent {
				if exists(t, volume, name) {
					t.Errorf("%s survived\n%s", name, tree(t, volume))
				}
			}
			for name, want := range testCase.wantContent {
				if got := contents(t, volume, name); got != want {
					t.Errorf("%s holds %q, want %q", name, got, want)
				}
			}
			requireNoArtefacts(t, volume)
		})
	}
}

// Finding 2 on the real kernel: a symlink inside the volume makes two paths name
// one file, an exchange then moves nothing, and the cleanup a normal exchange
// does next would delete it. Expressed as a move - no command, the source is the
// result - which is the shape that reaches it in production.
func TestMovingAFileOntoItselfUnderAnotherNameIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo precious > /work/photos/a.jpg && ln -s photos /work/pics`)

	result := fluxOp(t, volume, "",
		baseArgs("/work/pics/a.jpg", "/work/photos/a.jpg", "--")...)

	if result.exit != 6 {
		t.Fatalf("exit %d, want 6:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "photos/a.jpg"); got != "precious" {
		t.Errorf("the file holds %q, want it left intact", got)
	}
}

func TestAFailedCommandLeavesTheDestinationAndReclaimsStaging(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/dest`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "", append(baseArgs("--discard-staging", staging, "/work/dest", "--"), "false")...)

	if result.exit == 0 {
		t.Fatalf("a failing command reported success:\n%s", result.output)
	}
	if got := contents(t, volume, "dest"); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	requireNoArtefacts(t, volume)
}

// Checked against what actually landed rather than against what an archive
// claims about itself: those numbers are written by whoever built it, so a bomb
// simply lies.
func TestAResultOverTheCeilingIsRefusedAndReclaimed(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && head -c 2000 /dev/zero > /work/src/big`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--max-bytes", "1000", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 3 {
		t.Fatalf("exit %d, want 3:\n%s", result.exit, result.output)
	}
	if exists(t, volume, "dest") {
		t.Error("the destination was published despite the refusal")
	}
	requireNoArtefacts(t, volume)
}

// A link in the result is published as a link, target and all. Nothing here
// reads through one, and the readers at the other end do not follow one either:
// FluxOS opens files on a volume with O_NOFOLLOW and lists them with lstat, and
// syncthing carries a link as a link.
func TestAResultContainingALinkIsPublished(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && ln -s /etc/shadow /work/src/link`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--data-only", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 0 {
		t.Fatalf("exit %d, want 0:\n%s", result.exit, result.output)
	}
	if target := inContainer(t, volume, "", `readlink /work/dest/link`); !strings.Contains(target.output, "/etc/shadow") {
		t.Errorf("the link was not published as the link it was: %s", target.output)
	}
	// Still a link rather than a copy of what it points at. Whether some reader
	// later follows it is that reader's business, and the readers that matter -
	// FluxOS and syncthing - do not.
	if entry := inContainer(t, volume, "", `test -L /work/dest/link && echo LINK || echo other`); !strings.Contains(entry.output, "LINK") {
		t.Errorf("the published entry is not a link: %s", entry.output)
	}
	requireNoArtefacts(t, volume)
}

// A move and a rename have NO command: the caller's source already IS the
// result, so publishing it is the whole operation. A usage check that demanded a
// command rejected every move, and nothing noticed for a whole branch.
func TestAMovePublishesWithNoCommandAtAll(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo hi > /work/photos/f`)

	result := fluxOp(t, volume, "", baseArgs("/work/photos", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if !exists(t, volume, "out/f") {
		t.Errorf("the move did not publish\n%s", tree(t, volume))
	}
	if exists(t, volume, "photos") {
		t.Error("the source survived the move")
	}
	requireNoArtefacts(t, volume)
}

func TestAMoveReplacesAFileOfTheSameKindAndCleansUp(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo new > /work/src && echo old > /work/out`)

	result := fluxOp(t, volume, "", baseArgs("/work/src", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out"); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
	if exists(t, volume, "src") {
		t.Error("the source was left behind after a move")
	}
	requireNoArtefacts(t, volume)
}

// A move that merges a directory into an existing one: no command, and no
// --discard-staging, because the source IS the caller's data - so the emptied
// source is removed by the publish rather than by the reclaim. What the
// destination already held that the source did not name is kept.
func TestAMoveMergesADirectoryAndRemovesTheSource(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src /work/out && echo new > /work/src/added && echo fromsrc > /work/src/shared && echo old > /work/out/kept && echo fromdst > /work/out/shared`)

	result := fluxOp(t, volume, "", baseArgs("--merge", "/work/src", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out/kept"); got != "old" {
		t.Errorf("out/kept holds %q - the merge did not keep what the source did not name", got)
	}
	if got := contents(t, volume, "out/added"); got != "new" {
		t.Errorf("out/added holds %q - the merge did not bring the new entry in", got)
	}
	if got := contents(t, volume, "out/shared"); got != "fromsrc" {
		t.Errorf("out/shared holds %q - a shared name was not overwritten by the source", got)
	}
	if exists(t, volume, "src") {
		t.Error("the emptied source was left behind after a move-merge")
	}
	requireNoArtefacts(t, volume)
}

// Staging is only ever discarded when the caller says it owns it. A move's
// operand is the user's own data, and discarding it on a failure would destroy
// the only copy.
func TestAFailureNeverDiscardsAnOperandTheCallerOwns(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo precious > /work/photos/f`)

	result := fluxOp(t, volume, "",
		baseArgs("--max-bytes", "1", "/work/photos", "/work/dest", "--")...)

	if result.exit != 3 {
		t.Fatalf("exit %d, want 3:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "photos/f"); got != "precious" {
		t.Errorf("the caller's own data holds %q, want precious", got)
	}
}

// Neither operand may contain the other, refused in the image and under the
// configuration a node runs it with.
//
// Displacing the destination takes everything beneath it, so a destination
// containing the staging path carries it away and the publish cannot finish -
// and completing it would mean deleting the rest of that folder, entries the
// caller never named. The interrupted state this used to reach is exercised in
// the unit tests, which can make a rename fail without an operation that could
// never have worked.
func TestPublishRefusesOperandsThatContainOneAnother(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/x/y/out/2024 && echo precious > /work/x/y/out/2024/photo && echo irreplaceable > /work/x/y/out/wedding`)

	result := fluxOp(t, volume, "", baseArgs("/work/x/y/out/2024", "/work/x/y/out", "--")...)

	if result.exit == 0 {
		t.Fatalf("publishing a directory over its own parent succeeded:\n%s", result.output)
	}

	// Refused before anything moved, which is the whole difference between an
	// operation that did not happen and one that took the caller's folder away.
	if got := contents(t, volume, "x/y/out/wedding"); got != "irreplaceable" {
		t.Errorf("a file the caller never named holds %q\n%s", got, tree(t, volume))
	}
	if got := contents(t, volume, "x/y/out/2024/photo"); got != "precious" {
		t.Errorf("the operand holds %q\n%s", got, tree(t, volume))
	}
	requireNoArtefacts(t, volume)
}

// Nothing is displaced over an operation that cannot be carried out, so there is
// nothing for a sweep to put back afterwards.
func TestAMissingStagingPathFailsBeforeAnythingMoves(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/x/y && echo precious > /work/x/y/out`)

	result := fluxOp(t, volume, "", baseArgs("/work/a/b/photos", "/work/x/y/out", "--")...)

	if result.exit == 0 {
		t.Fatalf("publishing a staging path that does not exist succeeded:\n%s", result.output)
	}
	if got := contents(t, volume, "x/y/out"); got != "precious" {
		t.Errorf("destination holds %q, want precious\n%s", got, tree(t, volume))
	}
	requireNoArtefacts(t, volume)
}

// tar -C and unzip -d both need the directory to exist already. A file copy must
// NOT ask for it: cp -T refuses to overwrite a directory with a non-directory.
func TestStagingIsCreatedForCommandsThatNeedIt(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && echo x > /work/src/f && tar -cf /work/a.tar -C /work src`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", staging, "/work/out", "--"),
			"tar", "-xf", "/work/a.tar", "-C", staging)...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out/src/f"); got != "x" {
		t.Errorf("extracted content is %q", got)
	}
}

// Cancelling an operation has to reach the command, not just the process
// supervising it.
//
// A container stop delivers SIGTERM to PID 1 only, so an unforwarded signal
// leaves the command writing into a staging directory nobody will publish. And
// an untrapped one kills the supervisor outright, so its cleanup never runs and
// the space stays spent on a volume the caller pays for until the next boot
// sweep.
//
// Driven with `docker stop`, which is what FluxOS issues - not by signalling a
// process on this side of the container.
func TestACancelledOperationStopsItsCommandAndReclaimsStaging(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/dest`)

	name := "flux-op-cancel-" + strings.ReplaceAll(t.Name(), "/", "-")
	staging := "/work/.flux-op-" + operationID

	argv := append([]string{"run", "--name", name}, executorConfig(volume)...)
	argv = append(argv, image(), "flux-op")
	argv = append(argv, baseArgs("--discard-staging", "--mkdir", staging, "/work/dest", "--", "sleep", "30")...)

	cmd := exec.Command("docker", argv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the container: %v", err)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })

	// Wait for the operation to actually be under way, so the stop cannot
	// arrive before there is anything to stop.
	deadline := time.Now().Add(30 * time.Second)
	for !exists(t, volume, ".flux-op-"+operationID) {
		if time.Now().After(deadline) {
			t.Fatal("the operation never created its staging directory")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if out, err := exec.Command("docker", "stop", "--time", "15", name).CombinedOutput(); err != nil {
		t.Fatalf("could not stop the container: %v\n%s", err, out)
	}

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if err == nil {
		t.Fatal("a cancelled operation reported success")
	}
	if !errors.As(err, &exitErr) {
		t.Fatalf("unexpected failure: %v", err)
	}
	if exitErr.ExitCode() != 143 {
		t.Errorf("exit %d, want 143 - which is what tells a cancelled operation from a failed one", exitErr.ExitCode())
	}

	if got := contents(t, volume, "dest"); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	requireNoArtefacts(t, volume)
}

func TestTheIdentifierAndVolumeRootAreRequired(t *testing.T) {
	volume := volumeDir(t)

	cases := [][]string{
		{"/work/.flux-op-1", "/work/dest", "--", "true"},
		{"--id", operationID, "/work/.flux-op-1", "/work/dest", "--", "true"},
		{"--root", "/work", "/work/.flux-op-1", "/work/dest", "--", "true"},
	}

	for _, argv := range cases {
		result := fluxOp(t, volume, "", argv...)
		if result.exit != 2 {
			t.Errorf("exit %d for %v, want 2:\n%s", result.exit, argv, result.output)
		}
		if exists(t, volume, "dest") {
			t.Error("a refused invocation touched the destination")
		}
	}
}

// --no-replace on the filesystem and kernel this actually runs on, which is the
// half a unit test cannot answer for: it is a different rename flag from the
// exchange, and the executor's seccomp profile decides whether the syscall
// arrives at all.
func TestNoReplaceRefusesAnOccupiedDestination(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && echo new > /work/src/f && echo original > /work/dest`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--no-replace", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 5 {
		t.Fatalf("exit %d, want 5:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	requireNoArtefacts(t, volume)
}

// Creating a folder: no command, staging made by flux-op, a name that must be
// free. The publish is the same one every other operation uses, which is what
// keeps a create from being a second way of writing to a volume.
func TestCreatingADirectory(t *testing.T) {
	volume := volumeDir(t)

	staging := "/work/.flux-op-" + operationID
	created := fluxOp(t, volume, "",
		baseArgs("--discard-staging", "--mkdir", "--no-replace", staging, "/work/photos", "--")...)

	if created.exit != 0 {
		t.Fatalf("exit %d:\n%s", created.exit, created.output)
	}
	if !exists(t, volume, "photos") {
		t.Fatalf("the folder was not created\n%s", tree(t, volume))
	}
	requireNoArtefacts(t, volume)

	// The same request again, which is an owner clicking the button twice. The
	// folder they already have is not disturbed, and the status says why.
	seed(t, volume, `echo theirs > /work/photos/holiday.jpg`)
	again := fluxOp(t, volume, "",
		baseArgs("--discard-staging", "--mkdir", "--no-replace", staging, "/work/photos", "--")...)

	if again.exit != 5 {
		t.Fatalf("exit %d, want 5:\n%s", again.exit, again.output)
	}
	if got := contents(t, volume, "photos/holiday.jpg"); got != "theirs" {
		t.Errorf("the folder that was already there holds %q", got)
	}
	requireNoArtefacts(t, volume)
}
