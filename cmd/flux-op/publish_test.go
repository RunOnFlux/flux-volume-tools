package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(contents)
}

func TestPublishToANewDestination(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, staging, "new")

	if err := publish(staging, destination, root, testID); err != nil {
		t.Fatal(err)
	}

	if got := read(t, destination); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
	// Nothing to clean up when there was nothing to displace.
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("volume holds %d entries, want only the destination", len(entries))
	}
}

// rename(2) refuses a non-empty directory target and cannot replace a file with
// a directory at all, so every combination goes through the same swap rather
// than branching on type - the branch is where the file-replaced-by-directory
// case was originally missed.
func TestPublishReplacesEveryCombinationOfTypes(t *testing.T) {
	cases := []struct {
		name          string
		makeStaging   func(t *testing.T, path string)
		makeExisting  func(t *testing.T, path string)
		expectAtDest  string
		expectMissing string
	}{
		{
			name:         "a file replaces a file",
			makeStaging:  func(t *testing.T, p string) { write(t, p, "new") },
			makeExisting: func(t *testing.T, p string) { write(t, p, "old") },
			expectAtDest: "new",
		},
		{
			name:         "a directory replaces a file",
			makeStaging:  func(t *testing.T, p string) { write(t, filepath.Join(p, "f"), "new") },
			makeExisting: func(t *testing.T, p string) { write(t, p, "old") },
			expectAtDest: "f",
		},
		{
			name:         "a file replaces a directory",
			makeStaging:  func(t *testing.T, p string) { write(t, p, "new") },
			makeExisting: func(t *testing.T, p string) { write(t, filepath.Join(p, "keep"), "old") },
			expectAtDest: "new",
		},
		{
			name:          "a directory replaces a directory without merging into it",
			makeStaging:   func(t *testing.T, p string) { write(t, filepath.Join(p, "f"), "new") },
			makeExisting:  func(t *testing.T, p string) { write(t, filepath.Join(p, "keep"), "old") },
			expectAtDest:  "f",
			expectMissing: "keep",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			staging := filepath.Join(root, ".flux-op-"+testID)
			destination := filepath.Join(root, "dest")
			testCase.makeStaging(t, staging)
			testCase.makeExisting(t, destination)

			if err := publish(staging, destination, root, testID); err != nil {
				t.Fatal(err)
			}

			info, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if info.IsDir() {
				if _, err := os.Stat(filepath.Join(destination, testCase.expectAtDest)); err != nil {
					t.Errorf("expected %s inside the destination", testCase.expectAtDest)
				}
				if testCase.expectMissing != "" {
					if _, err := os.Stat(filepath.Join(destination, testCase.expectMissing)); err == nil {
						t.Errorf("%s survived - the destination was merged into, not replaced", testCase.expectMissing)
					}
				}
			} else if got := read(t, destination); got != testCase.expectAtDest {
				t.Errorf("destination holds %q, want %q", got, testCase.expectAtDest)
			}

			// The swap directory and its marker are transient; a completed
			// publish leaves neither.
			for _, leftover := range []string{swapPrefix + testID, swapPrefix + testID + markerSuffix} {
				if _, err := os.Lstat(filepath.Join(root, leftover)); err == nil {
					t.Errorf("%s was left behind", leftover)
				}
			}
		})
	}
}

// A dangling symlink is an entry, and has to be moved aside like any other. A
// check that followed it would find nothing and rename straight over the link.
func TestPublishTreatsADanglingSymlinkAsAnExistingEntry(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, staging, "new")
	if err := os.Symlink(filepath.Join(root, "nothing-here"), destination); err != nil {
		t.Fatal(err)
	}

	if err := publish(staging, destination, root, testID); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("the destination is still a symlink; the publish went through it")
	}
	if got := read(t, destination); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
}

// The state the marker exists for: the caller's previous data has been moved
// aside and the replacement never arrived. Reproduced by publishing a staging
// path that does not exist, so the second rename fails exactly where a crash
// would land.
// What a sweep compares. identity() is a unit in its own right, with its own
// tests for the property that matters - that it does not move when the object
// is written to - so what these tests pin is that the marker holds the identity
// of the PUBLISHED object rather than of the displaced one.
func identityOf(t *testing.T, path string) string {
	t.Helper()
	got, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAnInterruptedPublishLeavesTheDataAndAMarkerThatPlacesIt(t *testing.T) {
	root := t.TempDir()

	// A directory published over its own parent, which is a move a user can
	// ask for. The first rename carries the staging path away inside the
	// destination, so the second finds nothing at it and the publish stops
	// between the two - the state a crash in that window leaves, reached
	// without one.
	destination := filepath.Join(root, "photos")
	staging := filepath.Join(destination, "2024")
	write(t, staging, "precious")

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("publishing a directory over its own parent succeeded")
	}

	displaced := filepath.Join(root, swapPrefix+testID)
	if got := read(t, filepath.Join(displaced, "2024")); got != "precious" {
		t.Errorf("displaced data holds %q, want precious", got)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Error("the destination is still there, so this is not the interrupted state")
	}

	// Relative, so nothing that reads it can be sent off the volume by
	// following an absolute path. At the volume root, which is the one
	// directory the sweep reads - not beside the destination, wherever the
	// caller happened to keep it.
	//
	// The identity is read from the displaced copy: rename preserved it, which
	// is the property the whole record depends on.
	want := "photos\n" + identityOf(t, filepath.Join(displaced, "2024")) + "\n"
	if got := read(t, displaced+markerSuffix); got != want {
		t.Errorf("marker holds %q, want %q", got, want)
	}
}

// The record has to name the object being placed, not the one being displaced -
// the sweep compares it against whatever occupies the destination afterwards.
func TestTheMarkerRecordsTheIdentityOfWhatIsBeingPublished(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "photos")
	staging := filepath.Join(destination, "2024")
	write(t, staging, "precious")
	displacedIdentity := identityOf(t, destination)

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("publishing a directory over its own parent succeeded")
	}

	marker := read(t, filepath.Join(root, swapPrefix+testID)+markerSuffix)
	if got := identityOf(t, filepath.Join(root, swapPrefix+testID, "2024")); marker != "photos\n"+got+"\n" {
		t.Errorf("marker holds %q, want the identity of the published object %q", marker, got)
	}
	if marker == "photos\n"+displacedIdentity+"\n" {
		t.Error("marker records the displaced entry rather than what is being published")
	}
}

// Nothing is displaced over an operation that cannot be carried out, so there is
// nothing for a sweep to put back afterwards.
func TestAMissingStagingPathFailsBeforeAnythingMoves(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, destination, "precious")

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("publishing a staging path that does not exist succeeded")
	}

	if got := read(t, destination); got != "precious" {
		t.Errorf("destination holds %q, want precious", got)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("volume holds %d entries, want only the untouched destination: %v", len(entries), entries)
	}
}

func TestMarkerContentsAreRelativeToTheVolumeRoot(t *testing.T) {
	cases := []struct{ root, destination, want string }{
		{"/work", "/work/photos", "photos"},
		{"/work", "/work/a/b/c", "a/b/c"},
		{"/work/", "/work/photos", "photos"},
	}
	for _, testCase := range cases {
		if got := markerContents(testCase.destination, testCase.root); got != testCase.want {
			t.Errorf("markerContents(%q, %q) = %q, want %q",
				testCase.destination, testCase.root, got, testCase.want)
		}
	}
}
