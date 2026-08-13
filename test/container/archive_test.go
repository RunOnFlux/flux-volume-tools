//go:build docker

package container

import (
	"strings"
	"testing"
)

// Extraction is the one operation that runs attacker-supplied STRUCTURE rather
// than attacker-supplied bytes, and several of the things that stop it going
// wrong are not this program's doing - they are GNU tar's. That makes them worth
// a test here rather than a sentence in the README: the image pins coreutils and
// tar precisely so these hold, and a change in Alpine's packaging that swapped in
// a busybox applet would otherwise go unnoticed.
//
// These run flux-op exactly as the executor runs it for an extraction:
// --ordinary-only and a ceiling, into staging it created, publishing only on
// success.

const archiveCeiling = "10000000"

func extract(t *testing.T, volume, archive string) outcome {
	t.Helper()
	staging := "/work/.flux-op-" + operationID
	return fluxOp(t, volume, "", append(
		baseArgs("--discard-staging", "--mkdir", "--ordinary-only", "--max-bytes", archiveCeiling,
			staging, "/work/out", "--"),
		"tar", "xf", archive, "-C", staging)...)
}

// A member named ../../../../etc/evil is refused outright, so the operation
// fails and nothing is published.
func TestAnArchiveThatTraversesIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build && cd /work/build && echo pwned > payload &&
		tar cf /work/archive.tar --transform 's|^payload|../../../../etc/evil|' payload`)

	result := extract(t, volume, "/work/archive.tar")

	if result.exit == 0 {
		t.Fatalf("an archive that traverses extracted cleanly:\n%s", result.output)
	}
	if !strings.Contains(result.output, "..") {
		t.Errorf("the refusal did not name what was wrong:\n%s", result.output)
	}
	if exists(volume, "out") {
		t.Errorf("something was published\n%s", tree(volume))
	}
	requireNoArtefacts(t, volume)
}

// An absolute member name is stripped to a relative one, so it lands INSIDE the
// directory being extracted into rather than at the path it names.
func TestAnArchiveWithAnAbsoluteMemberLandsInsideTheTarget(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo pwned > /work/hostname &&
		tar cf /work/archive.tar -P /work/hostname`)

	result := extract(t, volume, "/work/archive.tar")

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	// Published under the destination, carrying its original path as a relative
	// one - not written to the path it asked for.
	if got := contents(t, volume, "out/work/hostname"); got != "pwned" {
		t.Errorf("the member landed as %q\n%s", got, tree(volume))
	}
}

// The classic: a symlink, then a member written THROUGH it. tar replaces the
// link with a real directory rather than following it, so the write lands inside
// the extraction and the link's target is untouched.
//
// This is the one case --ordinary-only cannot answer by itself. It runs after
// the command, so a write through a link would already have happened; what
// prevents it is tar.
func TestAnArchiveCannotWriteThroughItsOwnSymlink(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build && cd /work/build &&
		ln -s /etc escape && tar cf /work/archive.tar escape &&
		echo pwned > payload &&
		tar rf /work/archive.tar --transform 's|^payload|escape/passwd_probe|' payload`)

	result := extract(t, volume, "/work/archive.tar")

	// However it ends, the one thing that must be true is that the write did not
	// leave the extraction - checked from inside the same container, because a
	// fresh one would have a fresh /etc and prove nothing.
	reached := inContainer(t, volume, "", `test -e /etc/passwd_probe && echo REACHED || echo contained`)
	if strings.Contains(reached.output, "REACHED") {
		t.Fatalf("a member was written through a symlink and left the extraction:\n%s", result.output)
	}
}

// A FIFO is not a link, so nothing used to ask about it - and whatever opens one
// without O_NONBLOCK waits for a writer that never comes. tar carries and
// recreates them, so an archive is all it takes.
func TestAResultHoldingAFifoIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/out`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "", append(
		baseArgs("--discard-staging", "--mkdir", "--ordinary-only", staging, "/work/out", "--"),
		"sh", "-c", "mkfifo "+staging+"/pipe && echo data > "+staging+"/ordinary")...)

	if result.exit != 4 {
		t.Errorf("exit %d, want 4 - the code that says the result was not ordinary data\n%s",
			result.exit, result.output)
	}
	if got := contents(t, volume, "out"); got != "original" {
		t.Errorf("the destination holds %q, so a FIFO was published over it\n%s", got, tree(volume))
	}
	requireNoArtefacts(t, volume)
}

// A file occupies whole blocks, so an archive of many tiny files consumes
// thousands of times what it reports. The ceiling handed to an extraction is the
// volume's free space - a count of blocks - so measuring what the files say
// would pass an extraction that had already exceeded it many times over.
//
// The ceiling here sits between the two figures deliberately: the tree reports a
// few kilobytes for itself and occupies several megabytes, so a run that
// measures the wrong one publishes.
func TestAnArchiveOfManyTinyFilesIsMeasuredByWhatItOccupies(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build/many && cd /work/build/many &&
		i=0; while [ $i -lt 2000 ]; do printf x > "f$i"; i=$((i+1)); done &&
		cd /work/build && tar cf /work/archive.tar many`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "", append(
		baseArgs("--discard-staging", "--mkdir", "--ordinary-only", "--max-bytes", "1000000",
			staging, "/work/out", "--"),
		"tar", "xf", "/work/archive.tar", "-C", staging)...)

	if result.exit != 3 {
		t.Errorf("exit %d, want 3 - the code that says the result was over the ceiling\n%s",
			result.exit, result.output)
	}
	if exists(volume, "out") {
		t.Errorf("an extraction over the ceiling was published\n%s", tree(volume))
	}
	requireNoArtefacts(t, volume)
}
