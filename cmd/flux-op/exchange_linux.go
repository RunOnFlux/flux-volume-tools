//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps two directory entries in one atomic step.
//
// RENAME_EXCHANGE has no in-between state to be caught in. Either the entries
// are swapped or they are not, and both outcomes are consistent:
//
//	before  destination = the caller's data, staging = a result never published
//	after   destination = the result, staging = the caller's superseded data
//
// In both, the destination is complete and correct and the staging entry is
// disposable - so recovery is one unconditional rule: delete the staging entry.
// Nothing is recorded, and there is nothing on the volume for a sweep to read.
//
// Linux 3.15 and after, on every filesystem a Flux node puts an app volume on -
// verified on ext4, xfs, overlay and tmpfs, inside a container configured the
// way the executor configures one, so the default seccomp profile permits it.
//
// Not faked where it is missing. Falling back to two renames would mean the
// guarantee holds on some nodes and not others, decided by something no caller
// can see; a refusal says so.
func exchange(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		if err == unix.ENOSYS || err == unix.EINVAL || err == unix.ENOTSUP {
			return fmt.Errorf("this filesystem cannot exchange %s and %s atomically, so publishing here would not be crash safe: %w", a, b, err)
		}
		return fmt.Errorf("could not exchange %s and %s: %w", a, b, err)
	}
	return nil
}

// renameNoReplace moves a onto b, or refuses because b is taken.
//
// The same call as the exchange above with the opposite flag, and it is used for
// the same reason: the question and the action are one step. A caller that looked
// first and renamed afterwards would be answering for a moment that has passed -
// the app whose volume this is runs throughout, and may take the name in between.
// Here the kernel compares and moves under the parent directory's own lock, so
// "it already exists" is true of the instant nothing was written.
//
// An occupied name gives EEXIST; a directory gives ENOTEMPTY on some
// filesystems, which means the same thing to the caller.
func renameNoReplace(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_NOREPLACE); err != nil {
		if err == unix.EEXIST || err == unix.ENOTEMPTY {
			return fmt.Errorf("%w: %s", errDestinationExists, b)
		}
		if err == unix.ENOSYS || err == unix.EINVAL || err == unix.ENOTSUP {
			return fmt.Errorf("this filesystem cannot refuse an occupied name as part of the rename, so publishing %s here could destroy what is already there: %w", b, err)
		}
		return fmt.Errorf("could not move %s to %s: %w", a, b, err)
	}
	return nil
}
