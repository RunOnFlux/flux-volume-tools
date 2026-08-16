//go:build darwin

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange on macOS, which has the same primitive under another name.
//
// flux-op only ever runs on Linux - it lives in the executor image - so this
// exists for the tests. It is not a convenience: publish is filesystem
// behaviour, and testing filesystem behaviour against a mocked filesystem
// proves nothing, so its tests act on a real one. Without this they could not
// run on the machine the code is written on, and a suite that is only honest in
// CI is a suite nobody runs before pushing.
//
// APFS and HFS+ both implement RENAME_SWAP with the same guarantee: the two
// entries are exchanged or they are not.
func exchange(a, b string) error {
	if err := unix.RenamexNp(a, b, unix.RENAME_SWAP); err != nil {
		if err == unix.ENOTSUP || err == unix.EINVAL {
			return fmt.Errorf("this filesystem cannot exchange %s and %s atomically, so publishing here would not be crash safe: %w", a, b, err)
		}
		return fmt.Errorf("could not exchange %s and %s: %w", a, b, err)
	}
	return nil
}

// renameNoReplace on macOS, which spells RENAME_NOREPLACE as RENAME_EXCL. Same
// guarantee: the kernel refuses the occupied name as part of the rename, so
// nothing is decided about a moment that has already passed.
func renameNoReplace(a, b string) error {
	if err := unix.RenamexNp(a, b, unix.RENAME_EXCL); err != nil {
		if err == unix.EEXIST || err == unix.ENOTEMPTY {
			return fmt.Errorf("%w: %s", errDestinationExists, b)
		}
		if err == unix.ENOTSUP || err == unix.EINVAL {
			return fmt.Errorf("this filesystem cannot refuse an occupied name as part of the rename, so publishing %s here could destroy what is already there: %w", b, err)
		}
		return fmt.Errorf("could not move %s to %s: %w", a, b, err)
	}
	return nil
}
