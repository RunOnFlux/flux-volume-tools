//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps two directory entries in one atomic step.
//
// This is what removes the crash window rather than reasoning about it. The old
// publish was two renames - move the destination aside, then move the result in
// - and a crash between them left the caller's only copy under a name the sweep
// had to identify by comparing an inode number and a creation time. That
// comparison is not sound: filesystems reuse inode numbers immediately, and
// inode timestamps come from a coarse clock, so two objects created close
// together carry the same identity. On ext4, measured: the inode is reused every
// time and the creation time collides more than half the time. The sweep was
// therefore deciding whether to delete somebody's only copy of their data on a
// comparison that a coincidence can satisfy.
//
// RENAME_EXCHANGE has no in-between state to be caught in. Either the entries
// are swapped or they are not, and both outcomes are consistent:
//
//	before  destination = the caller's data, staging = a result never published
//	after   destination = the result, staging = the caller's superseded data
//
// In both, the destination is complete and correct and the staging entry is
// disposable - so recovery stops being a judgement and becomes one unconditional
// rule the sweep already applies: delete the staging entry. No marker, no
// identity, and no tenant-writable file for the boot sweep to read.
//
// Linux 3.15 and after, on every filesystem a Flux node puts an app volume on -
// verified on ext4, xfs, overlay and tmpfs, inside a container configured the
// way the executor configures one, so the default seccomp profile permits it.
//
// Not faked where it is missing. Falling back to the two renames would mean the
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
