package main

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	if before.hasNonData {
		t.Error("a tree of ordinary files reported something that is not data")
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

// A symlink is data, and it is measured as the entry it is rather than as
// whatever it points at. Following one would leave the volume entirely: a link
// to / measures the host, and a link to .. never finishes.
func TestInspectMeasuresASymlinkWithoutFollowingIt(t *testing.T) {
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
	if result.hasNonData {
		t.Error("a symlink was reported as something that is not data")
	}
	// Bounded well below the 100,000 bytes behind the link and well above the
	// handful of blocks a three-entry tree occupies. The old bound of 10,000 was
	// calibrated against apparent sizes and had no room in it once entries were
	// measured by what they occupy: on ext4 this tree is 12,288 bytes of blocks
	// and on APFS it is a fraction of that, so the same figure passed locally
	// and failed in CI.
	if result.bytes > 50000 {
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
		if result.hasNonData {
			t.Error("a linked directory was reported as something that is not data")
		}
	case <-timeoutAfterSeconds(10):
		t.Fatal("the walk did not finish - it descended through the link")
	}
}

// Hard links are counted once, as du counts them, so a tree naming the same
// data twice is not measured as twice the space it occupies - and an archive
// that holds one file under two names is ordinary content, not a refusal.
func TestInspectCountsHardLinkedDataOnce(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	write(t, original, strings.Repeat("x", 5000))

	before, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.hasNonData {
		t.Fatal("a tree with one ordinary file reported something that is not data")
	}

	if err := os.Link(original, filepath.Join(root, "second-name")); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}

	after, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.hasNonData {
		t.Error("a hard link was reported as something that is not data")
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
	// What it OCCUPIES, which is its length rounded up to whole blocks - not the
	// 1234 it reports for itself. The ceiling this feeds is the volume's free
	// space, so the two have to be the same kind of number.
	if result.bytes < 1234 {
		t.Errorf("measured %d bytes, want at least the 1234 the file holds", result.bytes)
	}
	if result.hasNonData {
		t.Error("a plain file reported something that is not data")
	}
}

// Twenty thousand one-byte files are 20KB by their own account and 82MB on the
// disk they sit on. The ceiling exists to stop an extraction filling the volume,
// and it is handed the volume's free space - so measuring what the files SAY
// leaves it comparing two different kinds of number, and passing an extraction
// that has already consumed thousands of times its measured size.
func TestInspectMeasuresWhatTheVolumeLosesNotWhatTheFilesSay(t *testing.T) {
	root := t.TempDir()

	const files = 200
	for i := 0; i < files; i++ {
		write(t, filepath.Join(root, "f"+strconv.Itoa(i)), "x")
	}

	// A filesystem that keeps a small file inside its inode reports no blocks
	// for it, and there is no difference here to measure. Proven rather than
	// assumed, so this cannot pass by measuring nothing.
	info, err := os.Lstat(filepath.Join(root, "f0"))
	if err != nil {
		t.Fatal(err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Blocks == 0 {
		t.Skip("this filesystem stores a one-byte file without allocating a block")
	}

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	// st_blocks is counted in 512-byte units whatever the filesystem's own block
	// size is, and a file holding a byte has to occupy at least one. So the floor
	// is files*512 - two orders of magnitude above the `files` bytes they report
	// between them, which is what makes this fail on the apparent figure rather
	// than merely reading differently.
	apparent := int64(0)
	filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			apparent += info.Size()
		}
		return nil
	})

	if result.bytes < files*512 {
		t.Errorf("measured %d bytes for %d one-byte files (apparent size of the tree is %d); "+
			"that is what the files say rather than what they occupy",
			result.bytes, files, apparent)
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

// A FIFO carries no data at all: whatever opens it without O_NONBLOCK waits for
// a writer that is never coming, and tar both carries and recreates one - so an
// archive is all it takes to put one on the volume.
//
// Device nodes cannot be made here, because CAP_MKNOD is dropped. FIFOs and
// sockets need no capability at all.
func TestInspectFindsAnEntryThatIsNotData(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ordinary"), "data")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.hasNonData {
		t.Error("a FIFO in the result was not reported, so --data-only would publish it")
	}
	// Named, because an owner told their archive was refused has no other way to
	// find out which entry did it.
	if result.nonData != "pipe" {
		t.Errorf("the refusal names %q, want pipe", result.nonData)
	}
}

// A socket is the other one an unprivileged process can create, and it is not
// data for the same reason.
func TestInspectFindsASocket(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(root, "sock"))
	if err != nil {
		t.Skipf("this platform will not bind a unix socket here: %v", err)
	}
	defer listener.Close()

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.hasNonData {
		t.Error("a socket in the result was not reported")
	}
	if result.nonData != "sock" {
		t.Errorf("the refusal names %q, want sock", result.nonData)
	}
}

// The directory holding the result is itself not a regular file, and refusing it
// would refuse every extraction there is.
func TestInspectDoesNotCallADirectoryNonData(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "nested", "deeper", "file"), "data")

	result, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.hasNonData {
		t.Error("an ordinary tree of files and directories was refused")
	}
}
