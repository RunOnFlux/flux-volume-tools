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
func TestAnInterruptedPublishLeavesTheDataAndAMarkerThatPlacesIt(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "nested", "deeper", "dest")
	write(t, destination, "precious")

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("publishing a staging path that does not exist succeeded")
	}

	displaced := filepath.Join(root, swapPrefix+testID)
	if got := read(t, displaced); got != "precious" {
		t.Errorf("displaced data holds %q, want precious", got)
	}

	// Relative, so nothing that reads it can be sent off the volume by
	// following an absolute path. At the volume root, which is the one
	// directory the sweep reads - not beside the destination, wherever the
	// caller happened to keep it.
	if got := read(t, displaced+markerSuffix); got != "nested/deeper/dest\n" {
		t.Errorf("marker holds %q, want nested/deeper/dest", got)
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
