package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// makeParents creates the folders missing between the volume and destination,
// so a result can be published into a folder that is not there yet.
//
// Each folder it makes takes the owner of the folder that receives it. This
// program runs as root, and a folder owned by root inside an application's
// volume is one an application running as any other user cannot write into -
// nor into anything later published inside it, which takes that folder's owner.
//
// Levels are made top down, one mkdir each, and a level that something else
// makes first is used as it is when it is a directory. The application is
// writing to its volume throughout, so the kernel's answer to each mkdir is the
// one acted on rather than a look taken beforehand. Anything else at a level - a
// file, a link that leads nowhere - refuses the operation, as mkdir -p does.
//
// A link along the way is followed, as mkdir -p follows it. This container has
// only the volume mounted, so wherever a link leads, a folder made through it is
// made in the application's own volume.
//
// What it makes stays when the publish after it is refused. By then another
// operation may have put something inside it.
func makeParents(destination, root string) error {
	if !contains(root, destination) || filepath.Clean(destination) == filepath.Clean(root) {
		return fmt.Errorf("%w: %s must be inside %s", errNothingMoved, destination, root)
	}

	var missing []string
	for dir := filepath.Dir(filepath.Clean(destination)); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		// The volume root is never made here: it is the mount, and one that is
		// missing means the operation was pointed at the wrong place.
		if dir == root {
			return fmt.Errorf("the volume root %s does not exist", root)
		}
		missing = append(missing, dir)
	}

	for i := len(missing) - 1; i >= 0; i-- {
		if err := makeLevel(missing[i]); err != nil {
			return err
		}
	}
	return nil
}

// makeLevel creates one folder with the owner of the folder that receives it,
// or accepts the directory something else made there first.
//
// The owner is read from the receiving folder as it is when this level is made,
// so a level made by another operation in the meantime passes its own owner on
// to what is made below it.
func makeLevel(dir string) error {
	uid, gid, err := ownerOf(filepath.Dir(dir))
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !os.IsExist(err) {
			return err
		}
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	return os.Lchown(dir, uid, gid)
}
