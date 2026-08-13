package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// publish moves the result into place.
//
// A destination that does not exist is one atomic rename and nothing to clean
// up.
//
// Replacing one that DOES exist cannot be a single rename in the general case.
// rename(2) refuses a non-empty directory as its target, and refuses to replace
// a file with a directory (or the reverse) at all - so deleting the existing
// entry first would be the only alternative, and a crash in that window loses
// the destination outright while the replacement sits under a staging name
// nobody recognises.
//
// Moving the old entry aside first avoids the window whatever the two types
// are. Both renames are atomic, so the worst a crash leaves is the old data
// under .flux-old-<id>, which the startup sweep renames back when it finds the
// destination missing. Uniform rather than branching on type: the branch is
// where the file-replaced-by-directory case was originally missed.
//
// Renaming directly, rather than through mv, means a publish that would cross a
// filesystem boundary fails instead of silently becoming a copy. Staging and
// destination are both inside the volume by construction, so that cannot happen
// without something else already being wrong - and a non-atomic publish is
// exactly what the caller was promised would not occur.
func publish(staging, destination, root, id string) error {
	// Which object is about to be published, read before anything moves. A
	// sweep that finds a destination occupied cannot otherwise tell what is
	// sitting there: the object this publish placed, or one the app owner put
	// at that path themselves while the destination stood empty. Recording the
	// answer costs one lstat and replaces a guess with a comparison.
	//
	// This also means a staging path that is not there fails before the
	// destination is displaced rather than after, which leaves nothing to sweep.
	stagedIdentity, err := identity(staging)
	if err != nil {
		return err
	}

	// Lstat, not Stat: a dangling symlink at the destination is an entry that
	// has to be moved aside, and a check that followed it would treat the
	// destination as empty and rename over the link.
	if _, err := os.Lstat(destination); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.Rename(staging, destination)
	}

	old := filepath.Join(root, swapPrefix+id)
	marker := old + markerSuffix

	// Where the data being moved aside belongs, written BEFORE it is moved. A
	// crash between the two renames below leaves the caller's previous data
	// under `old` with its own path empty, and without this the sweep has no way
	// to know where to put it back - it would delete the only copy.
	record := markerContents(destination, root) + "\n" + stagedIdentity + "\n"
	if err := os.WriteFile(marker, []byte(record), 0o644); err != nil {
		return fmt.Errorf("could not record where %s belongs: %w", destination, err)
	}

	if err := os.Rename(destination, old); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}

	// Best effort from here. The publish has happened and the caller has what
	// they asked for; a leftover swap directory is something the startup sweep
	// reclaims, and failing the operation over it would report a success as a
	// failure.
	os.RemoveAll(old)
	os.Remove(marker)
	return nil
}

const (
	// Prefix of the directory an interrupted publish leaves the previous data
	// under, and the suffix of the file recording where it belongs. FluxOS
	// matches both exactly, against a real identifier shape, because the sweep
	// DELETES what it matches in a directory the app owner can also write to.
	swapPrefix   = ".flux-old-"
	markerSuffix = ".dest"
)
