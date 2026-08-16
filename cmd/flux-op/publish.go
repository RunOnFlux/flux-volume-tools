package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// What --no-replace refuses. Reported to the caller as its own exit code, since
// a status is the one part of a failure that does not depend on wording.
var errDestinationExists = errors.New("the destination already exists")

// A publish that was refused before anything moved. Staging is still this
// operation's own scratch when one of these comes back, so the caller reclaims
// it now instead of leaving it on the volume for the next boot sweep.
var errNothingMoved = errors.New("refused before anything moved")

// publish moves the result into place.
//
// A destination that does not exist is one atomic rename and nothing to clean
// up.
//
// Replacing one that DOES exist is one atomic EXCHANGE. rename(2) cannot do it:
// it refuses a non-empty directory as its target, and refuses to replace a file
// with a directory or the reverse at all. What this used to do instead was two
// renames - move the destination aside under .flux-old-<id>, then move the
// result in - which is a window, and everything downstream existed to survive
// it: a marker recording where the parked data belonged, an identity recorded
// so the sweep could tell whether the second rename had happened, and a boot
// sweep reading a file inside a directory the application can write to.
//
// The exchange has no in-between state. Either the entries are swapped or they
// are not, and both are consistent - the destination holds something complete
// either way, and the staging name holds something disposable either way. So
// there is nothing to record, nothing to compare, and nothing for the sweep to
// decide: it deletes the staging entry, which is what it already does with one.
//
// That comparison was worth removing rather than fixing. It was an inode number
// and a creation time, and neither is unique: filesystems reuse inode numbers
// at once, and inode timestamps come from a coarse clock, so an object the app
// owner creates can carry the identity of the one that was published. Measured
// on ext4: the inode is reused every time and the creation time collides more
// than half the time. The sweep was deciding whether to delete somebody's only
// copy on evidence a coincidence satisfies.
//
// Renaming directly, rather than through mv, means a publish that would cross a
// filesystem boundary fails instead of silently becoming a copy. Staging and
// destination are both inside the volume by construction, so that cannot happen
// without something else already being wrong - and a non-atomic publish is
// exactly what the caller was promised would not occur.
//
// noReplace is the other guarantee some callers want: publish here, or refuse
// because the name is taken. It is one syscall that answers both questions at
// once, which is the only way to answer them truthfully - looking first and
// renaming afterwards decides on a state that may have changed by the time the
// rename runs, and the caller a create-folder or a rename answers is entitled to
// "it exists" meaning it existed at the instant nothing was written.
func publish(staging, destination, root, id string, noReplace bool) error {
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
		return fmt.Errorf("%w: %s and %s contain one another, so publishing one over the other would displace it", errNothingMoved, staging, destination)
	}

	// Both operands inside the volume, checked here rather than assumed from
	// the caller. This used to fall out of writing the marker - the record was
	// the destination relative to the root, so a destination outside it could
	// not be written down and the publish stopped. Removing the marker removed
	// that, which is the kind of guard that disappears when the thing it was
	// riding on goes: it is its own check now.
	//
	// flux-op is handed paths FluxOS has already resolved, so this is not the
	// only thing standing between an app owner and the host - it is this
	// program declining to hold that invariant on trust from its caller.
	if !contains(root, staging) || !contains(root, destination) {
		return fmt.Errorf("%w: %s and %s must both be inside %s", errNothingMoved, staging, destination, root)
	}

	// A staging path that is not there fails here, before the destination is
	// touched at all.
	if _, err := os.Lstat(staging); err != nil {
		return fmt.Errorf("%w: %w", errNothingMoved, err)
	}

	// No look at the destination at all. The kernel refuses an occupied name as
	// part of the rename, so there is no instant between deciding and acting for
	// anything to change in - and a dangling symlink, an empty directory and a
	// file are all equally occupied, which is the answer the caller wants and the
	// one a stat would have had to be written carefully to give.
	if noReplace {
		return renameNoReplace(staging, destination)
	}

	// Lstat, not Stat: a dangling symlink at the destination is an entry that
	// has to be replaced, and a check that followed it would treat the
	// destination as empty and rename over the link.
	if _, err := os.Lstat(destination); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.Rename(staging, destination)
	}

	if err := exchange(staging, destination); err != nil {
		return err
	}

	// The caller's previous data is now under the staging name, and the caller
	// has what they asked for. Best effort from here: a staging entry left
	// behind is exactly what the startup sweep reclaims, and failing the
	// operation over it would report a success as a failure.
	os.RemoveAll(staging)
	return nil
}

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
