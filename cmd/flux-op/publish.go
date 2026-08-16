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

// A publish refused because the only way to carry it out was to delete data the
// request never named - a file put where a directory is, an entry moved onto
// itself under another name. Nothing has moved, so staging is still reclaimed;
// but this is the caller's own request rather than a limit, so it reports its own
// exit status, which a dashboard turns into a specific answer rather than a
// generic failure.
var errWouldDestroy = errors.New("refused to avoid deleting data the caller did not name")

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
func publish(staging, destination, root, id string, noReplace, merge bool) error {
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
	stagingInfo, err := os.Lstat(staging)
	if err != nil {
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
	dstInfo, err := os.Lstat(destination)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Nothing at the name: one atomic rename, and no kind to reconcile.
		return os.Rename(staging, destination)
	}

	// The destination exists, so how the collision is resolved is decided from
	// what the two operands ARE:
	//
	//   the same entry under two names   refused. An app can point a symlink
	//                                    inside its own volume at another entry,
	//                                    so two different paths can name one
	//                                    file. Exchanging an entry with itself
	//                                    moves nothing, and the cleanup a normal
	//                                    exchange does next would then delete the
	//                                    caller's only copy. Compared by inode,
	//                                    the one comparison a spelling cannot fool.
	//
	//   a file over a file               replaced, in one atomic exchange.
	//
	//   a directory over a directory     merged when the caller asks for it, and
	//                                    otherwise refused. Replacing a directory
	//                                    wholesale deletes every entry the caller
	//                                    did not name but that sat beside one they
	//                                    did - the outcome this program exists to
	//                                    prevent - so it is never the default. A
	//                                    merge overlays the source onto the
	//                                    destination and keeps what neither names.
	//
	//   a file over a directory, or a    refused. A single file cannot stand in
	//   directory over a file            for a tree, and standing it there would
	//                                    delete the tree. mv -T and cp -T refuse
	//                                    this for the same reason.
	if os.SameFile(stagingInfo, dstInfo) {
		return fmt.Errorf("%w: %s and %s are the same entry", errWouldDestroy, staging, destination)
	}

	stagingIsDir := stagingInfo.IsDir()
	dstIsDir := dstInfo.IsDir()

	switch {
	case stagingIsDir && dstIsDir:
		if !merge {
			return fmt.Errorf("%w: %s is a directory, and replacing it would delete everything in it that %s does not, which a merge was not requested to allow", errWouldDestroy, destination, staging)
		}
		// Refused as a whole before anything moves where the two trees disagree
		// on a name's KIND. Overlaying regardless would delete a tree to seat a
		// file one level down, which is the same loss this refuses at the top.
		if err := checkMergeable(staging, destination); err != nil {
			return err
		}
		if err := mergeInto(staging, destination); err != nil {
			return err
		}
	case stagingIsDir != dstIsDir:
		return fmt.Errorf("%w: %s and %s are not the same kind of entry, so replacing one with the other would delete it", errWouldDestroy, staging, destination)
	default:
		if err := exchange(staging, destination); err != nil {
			return err
		}
	}

	// The caller's previous data is now under the staging name (an exchange), or
	// the staging tree has been emptied into the destination (a merge). Either
	// way the staging entry is disposable. Best effort from here: a staging entry
	// left behind is exactly what the startup sweep reclaims, and failing the
	// operation over it would report a success as a failure.
	os.RemoveAll(staging)
	return nil
}

// checkMergeable reports whether staging can be overlaid onto destination
// without a kind collision, decided before anything moves.
//
// Two entries that share a name but not a KIND - a file in one tree where the
// other keeps a directory - cannot both survive an overlay: seating the file
// deletes the directory, which is the loss a merge is meant to avoid. Found here,
// the whole publish is refused with nothing moved rather than left half done.
// Two directories are recursed into; two non-directories are a plain overwrite,
// which is what the caller asked for by merging into an occupied name.
func checkMergeable(staging, destination string) error {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(staging, entry.Name())
		dst := filepath.Join(destination, entry.Name())
		dstInfo, err := os.Lstat(dst)
		if err != nil {
			if os.IsNotExist(err) {
				continue // no entry to collide with; it moves straight in
			}
			return err
		}
		srcInfo, err := os.Lstat(src)
		if err != nil {
			return err
		}
		srcIsDir := srcInfo.IsDir()
		dstIsDir := dstInfo.IsDir()
		switch {
		case srcIsDir && dstIsDir:
			if err := checkMergeable(src, dst); err != nil {
				return err
			}
		case srcIsDir != dstIsDir:
			return fmt.Errorf("%w: %s and %s are not the same kind of entry, so merging would delete one", errWouldDestroy, src, dst)
		}
	}
	return nil
}

// mergeInto overlays staging onto destination, entry by entry.
//
// A name the destination does not hold is moved straight in. Two directories are
// merged recursively, so what the destination already keeps under a shared
// directory name stays rather than being replaced with the source's whole
// version of that directory. Two non-directories are replaced - checkMergeable
// has already established no name collides across kinds, so a rename here only
// ever replaces like with like.
//
// Not atomic: it is a sequence of renames, so an interruption can leave the
// overlay part done. That is inherent to merging - every tool that overlays one
// tree onto another has it - and it is the trade a caller accepts by merging
// into a directory that already has contents rather than replacing it.
func mergeInto(staging, destination string) error {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(staging, entry.Name())
		dst := filepath.Join(destination, entry.Name())
		dstInfo, err := os.Lstat(dst)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			continue
		}
		srcInfo, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if srcInfo.IsDir() && dstInfo.IsDir() {
			if err := mergeInto(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
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
