package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The unit st_blocks is expressed in, fixed by POSIX and unrelated to the
// filesystem's own block size.
const blocksAreCountedIn = 512

type inspection struct {
	// What the tree OCCUPIES on the filesystem it is on, counting anything
	// hard-linked once. This is what `du` reports by default, not `du -sb`.
	//
	// Occupied rather than apparent because of what the figure is FOR: the
	// ceiling handed to this program is the volume's free space, which is a
	// count of blocks. Measuring what the files say instead compares two
	// different kinds of number, and the gap is not small - a file occupies
	// whole blocks, so twenty thousand one-byte files are 20KB by their own
	// account and 82MB on an ext4 volume. An extraction of them passed a ceiling
	// it had already exceeded four thousand times over.
	//
	// A sparse file now measures as what it occupies rather than as its length.
	// That is the right direction here: the promise is that an operation will
	// not fill the volume, and a sparse file does not. An application can write
	// into its own holes afterwards, but it can write to its own volume anyway.
	bytes int64
	// Whether the tree holds anything that is not ordinary data. Two kinds
	// qualify and for different reasons: a link REACHES something outside
	// itself, and a FIFO, socket or device node IS not data at all - whatever
	// opens a FIFO without O_NONBLOCK waits for a writer that never comes.
	//
	// Device nodes cannot be created in the executor's container, which drops
	// CAP_MKNOD. FIFOs and sockets need no capability, and tar both carries and
	// recreates a FIFO - so an archive is all it takes.
	hasIrregular bool
}

// inspect walks the result once and answers both questions the publish gates on.
//
// One pass rather than the two the shell implementation needed - `du` and then
// `find` - because on a tree large enough for the ceiling to matter, walking it
// twice is the expensive part.
//
// Nothing is followed. An app owner can write a symlink into their own volume,
// and a walk that follows one leaves the volume entirely: `escape -> /` measures
// the host and `loop -> ..` never finishes. filepath.WalkDir reads link entries
// without descending into them, which is the property this depends on.
//
// An entry that cannot be read is skipped rather than fatal, matching what du
// does. The root is not: a staging path that does not exist means the command
// produced nothing, and reporting zero bytes for it would let an empty result
// publish over the caller's data.
func inspect(root string) (inspection, error) {
	var result inspection

	info, err := os.Lstat(root)
	if err != nil {
		return result, err
	}

	// The result being a link is a different question from one inside it, and
	// the ceiling is why. Nothing is followed here, so a link measures as the few
	// bytes of its own path - and a staging path pointing at a tree of any size
	// would pass any --max-bytes it was given.
	//
	// The caller names staging with an identifier the application never learns,
	// so nothing can plant a link there today. Refused regardless: the ceiling
	// should not depend on that name having been unguessable, which is an
	// invariant this program neither states nor can check.
	if info.Mode()&fs.ModeSymlink != 0 {
		return result, fmt.Errorf("%s is a link, not a result to measure", root)
	}

	// Hard links are counted once, as du counts them, so a tree that names the
	// same data twice is not measured as twice the space it occupies.
	seen := make(map[[2]uint64]struct{})

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read contributes nothing and does not
			// fail the measurement; whether its contents would have breached
			// the ceiling is unknowable either way.
			return nil //nolint:nilerr // deliberate: skip what cannot be read
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}

		// A directory is the shape of the result itself, so it is ordinary here
		// even though it is not a regular file. Everything else that is neither
		// is refused: symlinks, FIFOs, sockets and device nodes alike.
		if !entry.Type().IsRegular() && !entry.IsDir() {
			result.hasIrregular = true
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && info.Mode().IsRegular() && uint64(stat.Nlink) > 1 {
			// A second name for the same data, and the other one may be
			// somewhere this result does not reach.
			result.hasIrregular = true

			key := [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}
			if _, counted := seen[key]; counted {
				return nil
			}
			seen[key] = struct{}{}
		}

		if ok {
			// POSIX counts st_blocks in 512-byte units whatever the filesystem's
			// own block size is, so this needs no knowledge of the volume.
			result.bytes += stat.Blocks * blocksAreCountedIn
		} else {
			// Nothing else to go on. Only reachable off unix, where this program
			// does not run - it is here so the walk cannot silently contribute
			// zero for every entry.
			result.bytes += info.Size()
		}
		return nil
	})

	return result, err
}
