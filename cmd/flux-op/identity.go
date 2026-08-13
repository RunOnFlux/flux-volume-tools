package main

import (
	"fmt"
	"os"
	"syscall"
)

// Which clock an identity was taken from. The record says so rather than
// leaving the reader to assume, because the two are not interchangeable: a
// creation time answers the question being asked and a modification time only
// approximates it. FluxOS compares whichever one is named here, so a volume
// that cannot supply the better answer degrades to the older behaviour instead
// of silently comparing one against the other.
const (
	identityBirth        = "btime"
	identityModification = "mtime"
)

// modificationIdentity is the weaker record: the inode number and the
// modification time.
//
// It is weaker because mtime moves. An app writing into a directory that was
// just published to it changes that directory's mtime, so the sweep stops
// recognising its own work and keeps the displaced copy for ever. That is the
// reason the creation time is preferred wherever the kernel offers one.
//
// Kept as the fallback because it is still better than the inode alone, which
// filesystems reuse.
func modificationIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot read the identity of %s on this platform", path)
	}
	return fmt.Sprintf("%d %d %s", stat.Ino, info.ModTime().UnixNano(), identityModification), nil
}
