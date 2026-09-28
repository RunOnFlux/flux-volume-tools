package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// inheritOwner gives everything in staging the owner of the directory its
// entries land in: the destination itself when it is an existing directory,
// which a merge fills, and otherwise the directory that will hold it.
//
// This program runs as root, so what a command or an upload writes is owned by
// root, and an application running as any other user cannot change its own
// uploaded or extracted files. The folder the result lands in says who the
// application writes as.
//
// Nothing is followed: a link is re-owned itself, never its target. A hard link
// in the result shares its inode with whatever else names it, and the only
// thing mounted is this application's volume, so what that can re-own is the
// application's own data, to the owner the application gave its own folder.
func inheritOwner(staging, destination string) error {
	folder := destination
	if info, err := os.Stat(destination); err != nil || !info.IsDir() {
		folder = filepath.Dir(destination)
	}
	info, err := os.Stat(folder)
	if err != nil {
		return fmt.Errorf("could not read the owner of %s: %w", folder, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no owner is recorded for %s on this platform", folder)
	}
	uid, gid := int(stat.Uid), int(stat.Gid)

	return filepath.WalkDir(staging, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(path, uid, gid)
	})
}
