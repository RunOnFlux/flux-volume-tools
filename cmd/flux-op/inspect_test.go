package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func timeoutAfterSeconds(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

func TestInspectCountsEveryFileInTheTree(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a"), strings.Repeat("x", 100))
	write(t, filepath.Join(root, "nested", "b"), strings.Repeat("x", 250))

	before, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.bytes < 350 {
		t.Errorf("measured %d bytes, want at least the 350 of file content", before.bytes)
	}
	if before.hasIrregular {
		t.Error("a tree with no links reported links")
	}

	// Directories count towards the total as well as their contents, which is
	// what `du -sb` reports and what the ceiling has always been expressed
	// against. So adding a file grows the measurement by at least its own size,
	// and by however much the directory entry cost on top - which is fixed on
	// ext4 and varies on other filesystems, so it is not a figure to assert.
	write(t, filepath.Join(root, "nested", "c"), strings.Repeat("x", 40))
	after, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if grew := after.bytes - before.bytes; grew < 40 {
		t.Errorf("adding 40 bytes grew the measurement by %d, which does not account for the file", grew)
	}
}

// A symlink is refused rather than followed. An app owner can write one into
// their own volume, and a walk that followed it would leave the volume: a link
// to / measures the host, and a link to .. never finishes.
func TestInspectFindsASymlinkAndDoesNotFollowIt(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "big")
	write(t, outside, strings.Repeat("x", 100000))
	write(t, filepath.Join(root, "small"), "x")

	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.hasIrregular {
		t.Error("the symlink was not reported")
	}
	if result.bytes > 10000 {
		t.Errorf("measured %d bytes - the walk followed the link and measured what is behind it", result.bytes)
	}
}

// A directory symlink is the shape that never terminates if it is followed.
func TestInspectDoesNotDescendThroughALinkedDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "f"), "x")
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}

	done := make(chan inspection, 1)
	go func() {
		result, _ := inspect(root)
		done <- result
	}()

	select {
	case result := <-done:
		if !result.hasIrregular {
			t.Error("the linked directory was not reported as a link")
		}
	case <-timeoutAfterSeconds(10):
		t.Fatal("the walk did not finish - it descended through the link")
	}
}

// Hard links are counted once, as du counts them, so a tree naming the same
// data twice is not measured as twice the space it occupies.
func TestInspectCountsHardLinkedDataOnceAndReportsIt(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	write(t, original, strings.Repeat("x", 5000))

	before, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.hasIrregular {
		t.Fatal("a tree with one ordinary file reported links")
	}

	if err := os.Link(original, filepath.Join(root, "second-name")); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}

	after, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !after.hasIrregular {
		t.Error("the hard link was not reported")
	}
	if grew := after.bytes - before.bytes; grew >= 5000 {
		t.Errorf("the measurement grew by %d - the same data was counted twice", grew)
	}
}

// A staging path that does not exist means the command produced nothing.
// Reporting zero bytes for it would let an empty result publish over the
// caller's data.
func TestInspectRefusesAMissingResult(t *testing.T) {
	if _, err := inspect(filepath.Join(t.TempDir(), "never-created")); err == nil {
		t.Error("a missing result measured successfully")
	}
}

func TestInspectMeasuresASingleFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "uploaded.bin")
	write(t, file, strings.Repeat("x", 1234))

	result, err := inspect(file)
	if err != nil {
		t.Fatal(err)
	}
	if result.bytes != 1234 {
		t.Errorf("measured %d bytes, want 1234", result.bytes)
	}
	if result.hasIrregular {
		t.Error("a plain file reported links")
	}
}

// The RESULT itself being a link is a different question from a link inside it,
// and the ceiling is the reason. A walk that does not follow links measures a
// link as the few bytes of its own path, so a staging path pointing at a tree of
// any size measures as nothing and passes any --max-bytes it is given.
//
// Not reachable today - FluxOS names staging with a fresh randomUUID the
// application never learns - but the safety of the ceiling should not rest on
// the caller having chosen an unguessable name, which is an invariant this
// program neither states nor can check.
func TestInspectRefusesAResultThatIsItselfALink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	write(t, filepath.Join(target, "big"), strings.Repeat("x", 5000))

	staging := filepath.Join(root, "staging")
	if err := os.Symlink(target, staging); err != nil {
		t.Fatal(err)
	}

	if _, err := inspect(staging); err == nil {
		t.Fatal("inspecting a result that is a link succeeded")
	}
}

// An entry that cannot be read is skipped rather than fatal, which is what du
// does. Whether its contents would have breached the ceiling is unknowable
// either way, and failing the measurement would refuse an operation over a
// directory the application merely happened to make unreadable.
func TestInspectSkipsWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where an unreadable directory is not unreadable")
	}

	root := t.TempDir()
	write(t, filepath.Join(root, "readable"), strings.Repeat("x", 100))
	closed := filepath.Join(root, "closed")
	write(t, filepath.Join(closed, "hidden"), strings.Repeat("x", 5000))
	if err := os.Chmod(closed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(closed, 0o755) })

	result, err := inspect(root)
	if err != nil {
		t.Fatalf("a directory that could not be read failed the whole measurement: %v", err)
	}
	// The readable file is still counted, so the walk carried on rather than
	// stopping at the entry it could not open.
	if result.bytes < 100 {
		t.Errorf("measured %d bytes, want at least the 100 it could read", result.bytes)
	}
}

// A link is not the only entry that is not ordinary data. A FIFO carries none at
// all: whatever opens it without O_NONBLOCK waits for a writer that is never
// coming, and tar both carries and recreates one - so an archive is all it takes
// to put one on the volume.
//
// Device nodes cannot be made here, because CAP_MKNOD is dropped. FIFOs and
// sockets need no capability at all.
func TestInspectFindsAnEntryThatIsNotOrdinaryData(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ordinary"), "data")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.hasIrregular {
		t.Error("a FIFO in the result was not reported, so --ordinary-only would publish it")
	}
}

// The directory holding the result is itself not a regular file, and refusing it
// would refuse every extraction there is.
func TestInspectDoesNotCallADirectoryIrregular(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "nested", "deeper", "file"), "data")

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.hasIrregular {
		t.Error("an ordinary tree of files and directories was refused")
	}
}
