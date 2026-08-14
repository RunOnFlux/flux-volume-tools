//go:build docker

package container

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The command must receive what the caller sent.
//
// The shell implementation ran it as a background job, and POSIX assigns
// /dev/null as the standard input of an asynchronous list when job control is
// off. So an upload piped into the container arrived nowhere, the command
// exited 0 having read an empty stream, and an empty file was published as a
// complete one.
func TestACommandReceivesTheCallersStandardInput(t *testing.T) {
	volume := volumeDir(t)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "the caller's bytes",
		append(baseArgs("--discard-staging", staging, "/work/dest", "--"),
			"dd", "of="+staging, "status=none")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); got != "the caller's bytes" {
		t.Errorf("destination holds %q - the command was handed the wrong descriptor", got)
	}
}

// An upload streamed straight into staging, with no child process at all. A
// command reading a stream cannot tell a truncated one from a complete one, so
// removing the command is what makes a short upload unpublishable.
func TestAStreamedUploadIsPublished(t *testing.T) {
	volume := volumeDir(t)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "uploaded content",
		baseArgs("--discard-staging", "--from-stdin", staging, "/work/dest", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); got != "uploaded content" {
		t.Errorf("destination holds %q", got)
	}
	requireNoArtefacts(t, volume)
}

func TestAStreamedUploadReplacesAnExistingDestination(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo old > /work/dest`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "new",
		baseArgs("--discard-staging", "--from-stdin", staging, "/work/dest", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
	requireNoArtefacts(t, volume)
}

// The ceiling is exact on this path, because flux-op is the writer. A command
// produces its own bytes, so its result can only be measured after the fact -
// an extraction fills the volume and is then refused. Here nothing over the
// limit ever reaches the disk, which matters because the volume being filled is
// the one the application's own database sits on.
func TestAStreamedUploadStopsAtTheCeilingAndPublishesNothing(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/dest`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, strings.Repeat("x", 4000),
		baseArgs("--discard-staging", "--from-stdin", "--max-bytes", "1000", staging, "/work/dest", "--")...)

	if result.exit != 3 {
		t.Fatalf("exit %d, want 3:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	requireNoArtefacts(t, volume)
}

func TestAStreamedUploadTakesNoCommand(t *testing.T) {
	volume := volumeDir(t)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "x",
		append(baseArgs("--from-stdin", staging, "/work/dest", "--"), "true")...)

	if result.exit != 2 {
		t.Fatalf("exit %d, want 2:\n%s", result.exit, result.output)
	}
	if exists(t, volume, "dest") {
		t.Error("a refused invocation published something")
	}
}

// Backpressure across the bind mount, at a size where the transfer is not one
// buffer. Proven at 256 MB by hand against a node; kept here at a size that is
// still meaningful without making every CI run carry it.
func TestALargeStreamArrivesIntact(t *testing.T) {
	volume := volumeDir(t)
	staging := "/work/.flux-op-" + operationID

	const megabytes = 16
	payload := strings.Repeat(strings.Repeat("x", 1024*1024), megabytes)

	result := fluxOp(t, volume, payload,
		baseArgs("--discard-staging", "--from-stdin", staging, "/work/dest", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "dest"); len(got) != megabytes*1024*1024 {
		t.Errorf("destination holds %d bytes, want %d", len(got), megabytes*1024*1024)
	}
	requireNoArtefacts(t, volume)
}

func TestAnEmptyUploadIsAnEmptyFile(t *testing.T) {
	volume := volumeDir(t)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "",
		baseArgs("--discard-staging", "--from-stdin", staging, "/work/dest", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if !exists(t, volume, "dest") {
		t.Error("an empty upload produced no file at all")
	}
	if got := contents(t, volume, "dest"); got != "" {
		t.Errorf("destination holds %q, want nothing", got)
	}
}

// The twin of TestACancelledOperationStopsItsCommandAndReclaimsStaging, for the
// path that has no command at all.
//
// A stop reaches flux-op itself rather than a child, and during an upload there
// is no child to forward it to - so unless flux-op handles the signal, the
// default disposition ends the process where it stands and the deferred reclaim
// never runs. What is left is a partial upload at a name the app owner cannot
// see, on a volume with a fixed size, until the next boot sweep.
//
// The stream stays OPEN and goes quiet, because that is what a stalled transfer
// looks like from in here: flux-op is parked in a read with nothing coming, so
// nothing it could poll would ever be looked at.
func TestACancelledUploadStopsAndReclaimsStaging(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/dest`)

	name := "flux-op-cancel-upload-" + strings.ReplaceAll(t.Name(), "/", "-")
	staging := "/work/.flux-op-" + operationID

	argv := append([]string{"run", "--name", name}, executorConfig(volume)...)
	argv = append(argv, image(), "flux-op")
	argv = append(argv, baseArgs("--discard-staging", "--from-stdin", staging, "/work/dest", "--")...)

	cmd := exec.Command("docker", argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the container: %v", err)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })

	if _, err := io.WriteString(stdin, "partial upload"); err != nil {
		t.Fatalf("could not send the first bytes: %v", err)
	}

	// Under way before it is stopped, so the stop cannot land on an operation
	// that has not started.
	deadline := time.Now().Add(30 * time.Second)
	for !exists(t, volume, ".flux-op-"+operationID) {
		if time.Now().After(deadline) {
			t.Fatalf("the upload never created its staging file\n%s", tree(t, volume))
		}
		time.Sleep(100 * time.Millisecond)
	}

	if out, err := exec.Command("docker", "stop", "--time", "15", name).CombinedOutput(); err != nil {
		t.Fatalf("could not stop the container: %v\n%s", err, out)
	}

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if err == nil {
		t.Fatal("a cancelled upload reported success")
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
