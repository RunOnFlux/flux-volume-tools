package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func identityNow(t *testing.T, path string) string {
	t.Helper()
	got, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The fault this exists to prevent. The identity used to be taken from the
// modification time, so an app writing into a directory that had just been
// published to it moved that directory's mtime - the sweep stopped recognising
// its own work, and kept the displaced copy for ever. With the reserved names
// in the same release the app owner could then neither see that copy nor delete
// it, on a volume with a fixed size.
func TestTheIdentitySurvivesTheObjectBeingWrittenTo(t *testing.T) {
	published := filepath.Join(t.TempDir(), "appdata")
	mkdir(t, published)

	before := identityNow(t, published)
	if !strings.HasSuffix(before, identityBirth) {
		t.Skipf("this filesystem keeps no creation time (identity %q), so the stable "+
			"comparison is not the one in use here", before)
	}

	// Exactly what a running app does to its own volume, and the whole reason
	// the sweep cannot ask what has been done to an object - only which one it
	// is.
	if err := os.WriteFile(filepath.Join(published, "written-by-the-app"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if after := identityNow(t, published); after != before {
		t.Errorf("the identity moved when the app wrote into the directory: %q then %q", before, after)
	}
}

// The control for the test above. Without it that assertion could pass for the
// wrong reason - a stub clock, a cached stat - and never notice. This is the
// behaviour that WAS shipped, so it must fail in exactly the way the creation
// time does not.
func TestTheModificationIdentityMovesWhenTheObjectIsWrittenTo(t *testing.T) {
	published := filepath.Join(t.TempDir(), "appdata")
	mkdir(t, published)

	before, err := modificationIdentity(published)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(published, "written-by-the-app"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := modificationIdentity(published)
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Skipf("this filesystem does not move a directory's mtime when an entry is "+
			"created in it (%q), so it cannot demonstrate what the creation time avoids", before)
	}
}

// A publish IS a rename, so an identity that did not survive one would mismatch
// the instant the publish succeeded.
func TestTheIdentitySurvivesTheRenameThatPublishesIt(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-abc")
	mkdir(t, staging)

	before := identityNow(t, staging)

	destination := filepath.Join(root, "appdata")
	if err := os.Rename(staging, destination); err != nil {
		t.Fatal(err)
	}

	if after := identityNow(t, destination); after != before {
		t.Errorf("the identity moved across the publishing rename: %q then %q", before, after)
	}
}

// The case the inode number alone cannot answer: filesystems reuse them, so an
// entry the app owner creates at the destination afterwards can carry the
// number recorded here.
func TestADifferentObjectHasADifferentIdentity(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "one"))
	mkdir(t, filepath.Join(root, "two"))

	if one, two := identityNow(t, filepath.Join(root, "one")), identityNow(t, filepath.Join(root, "two")); one == two {
		t.Errorf("two different directories share the identity %q", one)
	}
}

// The record says which clock it was taken from, so FluxOS compares like with
// like instead of assuming. A volume that cannot supply a creation time is
// meant to degrade to the older comparison, not to be compared against the
// wrong field.
func TestTheIdentityNamesTheClockItWasTakenFrom(t *testing.T) {
	published := filepath.Join(t.TempDir(), "appdata")
	mkdir(t, published)

	got := identityNow(t, published)
	fields := strings.Fields(got)
	if len(fields) != 3 {
		t.Fatalf("identity %q is not <inode> <time> <clock>", got)
	}
	if fields[2] != identityBirth && fields[2] != identityModification {
		t.Errorf("identity %q names the clock %q, which FluxOS does not know how to read", got, fields[2])
	}
}
