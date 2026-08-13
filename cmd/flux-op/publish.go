package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	// Neither operand may contain the other, decided before anything moves.
	//
	// Displacing the destination takes everything under it, so a destination
	// that contains staging carries staging away and the second rename finds
	// nothing - stopping in the interrupted state for an operation that was
	// never going to work. The caller's whole folder is then parked under a name
	// the reserved-name rules hide from them, and only the next boot sweep puts
	// it back. The mirror case cannot be renamed at all: rename(2) refuses to
	// move a directory into its own subtree.
	//
	// Refused rather than made to work. Completing it would mean deleting
	// everything ELSE in the destination - entries the caller never named, which
	// merely happened to sit beside the one they did - and that is the outcome
	// this program exists to prevent. A caller that means "move this up a level"
	// asks for <parent>/<name>, which is an ordinary publish.
	if contains(destination, staging) || contains(staging, destination) {
		return fmt.Errorf("%s and %s contain one another, so publishing one over the other would displace it", staging, destination)
	}

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
	if err := writeMarker(marker, record); err != nil {
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

// contains reports whether ancestor holds path, or names the same entry.
//
// Lexical, because that is the question a rename answers: both paths were
// resolved by the caller before they were handed over, and rename(2) acts on
// the path it is given rather than on wherever a link along it might lead.
func contains(ancestor, path string) bool {
	cleanAncestor := filepath.Clean(ancestor)
	cleanPath := filepath.Clean(path)
	if cleanAncestor == cleanPath {
		return true
	}
	return strings.HasPrefix(cleanPath, cleanAncestor+string(filepath.Separator))
}

// writeMarker writes the record, refusing to follow or replace anything already
// at that path.
//
// O_NOFOLLOW because this sits in a directory the application can write to as
// well: a link planted here would otherwise be followed, and the record of where
// the caller's data belongs would land wherever it pointed - leaving the
// displaced copy with no marker beside it, which the sweep reads as a duplicate
// and deletes.
//
// O_EXCL because the name derives from an identifier that is fresh for every
// operation. Anything already there is not a marker this operation wrote, and
// writing over it would destroy whatever it is.
//
// Neither is reachable while the caller names operations with an identifier the
// application never learns. Both are here so that this does not depend on it.
func writeMarker(path, record string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}

	// Checked rather than deferred and dropped: a write can fail at close, and a
	// marker that is short is a marker the sweep cannot read - which is the one
	// case that costs the caller their data.
	if _, err := file.WriteString(record); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
