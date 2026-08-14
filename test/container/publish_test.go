//go:build docker

package container

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The contract: the destination changes only on success, and it changes
// atomically whatever the two entry types are.
//
// rename(2) cannot replace a file with a directory (or the reverse) at all, and
// refuses a non-empty directory target - which is why publishing goes through a
// swap rather than a delete-then-rename, and why every combination is covered
// rather than just the common one.
func TestPublishReplacesEveryCombinationOfTypes(t *testing.T) {
	cases := []struct {
		name        string
		seed        string
		wantAtDest  string
		wantMissing string
	}{
		{
			name:       "a directory replaces an existing file",
			seed:       `mkdir -p /work/src && echo new > /work/src/f && echo original > /work/dest`,
			wantAtDest: "dest/f",
		},
		{
			name:        "a directory replaces an existing directory, without merging into it",
			seed:        `mkdir -p /work/src /work/dest && echo new > /work/src/f && echo old > /work/dest/keep`,
			wantAtDest:  "dest/f",
			wantMissing: "dest/keep",
		},
		{
			name:       "a file replaces an existing directory",
			seed:       `mkdir -p /work/dest && echo new > /work/src && echo old > /work/dest/keep`,
			wantAtDest: "dest",
		},
		{
			name:       "a new destination is one rename",
			seed:       `mkdir -p /work/src && echo new > /work/src/f`,
			wantAtDest: "dest/f",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			volume := volumeDir(t)
			seed(t, volume, testCase.seed)

			staging := "/work/.flux-op-" + operationID
			result := fluxOp(t, volume, "",
				append(baseArgs("--discard-staging", staging, "/work/dest", "--"),
					"cp", "-a", "-T", "/work/src", staging)...)

			if result.exit != 0 {
				t.Fatalf("exit %d:\n%s", result.exit, result.output)
			}
			if !exists(t, volume, testCase.wantAtDest) {
				t.Errorf("%s is not there\n%s", testCase.wantAtDest, tree(t, volume))
			}
			if testCase.wantMissing != "" && exists(t, volume, testCase.wantMissing) {
				t.Errorf("%s survived - the destination was merged into, not replaced\n%s",
					testCase.wantMissing, tree(t, volume))
			}
			requireNoArtefacts(t, volume)
		})
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

// An archive that carries a symlink and then writes through it reaches wherever
// the link points. Inside this container that is nowhere useful, but the result
// is published onto a volume that other code paths - and other nodes, through
// sync - do read.
func TestAResultContainingALinkIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && ln -s /etc/shadow /work/src/link`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--ordinary-only", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 4 {
		t.Fatalf("exit %d, want 4:\n%s", result.exit, result.output)
	}
	if exists(t, volume, "dest") {
		t.Error("the destination was published despite the refusal")
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

func TestAMoveOverAnExistingDestinationSwapsItAsideAndCleansUp(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo new > /work/photos/f && echo old > /work/out`)

	result := fluxOp(t, volume, "", baseArgs("/work/photos", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out/f"); got != "new" {
		t.Errorf("destination holds %q, want new", got)
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
