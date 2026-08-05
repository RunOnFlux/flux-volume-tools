package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

type inspection struct {
	// Apparent size of every entry in the tree - files, directories and the
	// targets symlinks name - counting anything hard-linked once. This is what
	// `du -sb` reports, and the ceiling has always been expressed against it.
	bytes int64
	// Whether the tree holds a symlink, or a regular file with more than one
	// name. Either lets a published result reach something outside itself.
	hasLinks bool
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

	if _, err := os.Lstat(root); err != nil {
		return result, err
	}

	// Hard links are counted once, as du counts them, so a tree that names the
	// same data twice is not measured as twice the space it occupies.
	seen := make(map[[2]uint64]struct{})

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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

		if entry.Type()&fs.ModeSymlink != 0 {
			result.hasLinks = true
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && info.Mode().IsRegular() && uint64(stat.Nlink) > 1 {
			result.hasLinks = true

			key := [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}
			if _, counted := seen[key]; counted {
				return nil
			}
			seen[key] = struct{}{}
		}

		result.bytes += info.Size()
		return nil
	})

	return result, err
}
