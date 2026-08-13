//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// identity is what a sweep compares to decide whether a publish finished: the
// inode number of the object being placed, and the time that object was
// created.
//
// Both survive rename, which is what makes them usable at all - the publish
// itself is a rename, so anything that did not survive one would mismatch the
// moment it succeeded. ctime does not survive it.
//
// The inode number alone is not enough, because filesystems reuse them: an
// entry the app owner creates at the destination after a publish can carry the
// number recorded here and be taken for the published object.
//
// The second field is the CREATION time and not the modification time, because
// the sweep is asking which object this is, not what has been done to it. An
// app writing into a directory that was just published to it moves that
// directory's mtime - so an mtime comparison stops recognising the publish and
// parks a full second copy of the volume's data for ever. A creation time does
// not move, and cannot be set by the app owner at all: there is no syscall for
// it, whereas mtime can be chosen freely.
//
// Verified on a Flux node (chud, kernel 6.17) that the creation time survives
// both rename and modification, and that a later object receives a different
// one:
//
//	filesystem            btime survives rename + write
//	ext4 (FLUXFSVOL)      yes
//	xfs (/dat)            yes
//	overlayfs             yes
//	tmpfs                 yes
//
// The mask is checked rather than assumed. A kernel that cannot supply a
// creation time still returns success from statx with the bit clear, and Node's
// lstat on the FluxOS side reports ctime as birthtime when it has nothing
// better - which reads as a valid answer at creation and then drifts on the
// first write, reintroducing exactly the fault this replaces. Only the mask
// distinguishes the two, and only this side can see it.
func identity(path string) (string, error) {
	var stx unix.Statx_t

	// AT_SYMLINK_NOFOLLOW: a publish moves a symlink AS a symlink, so the link
	// itself is the object being placed and what it points at is irrelevant.
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_BTIME, &stx)

	switch {
	case err == nil && stx.Mask&unix.STATX_BTIME != 0:
		born := stx.Btime.Sec*1_000_000_000 + int64(stx.Btime.Nsec)
		return fmt.Sprintf("%d %d %s", stx.Ino, born, identityBirth), nil

	// statx answered but this filesystem keeps no creation time, or the kernel
	// predates statx entirely. Neither is a reason to fail an operation the
	// caller asked for.
	case err == nil, err == unix.ENOSYS:
		return modificationIdentity(path)

	// Anything else - a missing staging path above all - is a real failure, and
	// failing HERE means it happens before the destination is displaced.
	default:
		return "", err
	}
}
