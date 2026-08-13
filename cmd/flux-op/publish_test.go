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

// interruptedPublish builds the state a crash between the two renames leaves:
// the destination displaced, and the second rename then failing.
//
// Reached by making staging's directory unwritable, which is the only lever that
// needs neither a second filesystem nor a mount the executor does not use. It
// deliberately does NOT use an operand that contains the other - that is refused
// now, and reaching this state through an operation that could never work is
// what hid the refusal being missing.
//
// Skipped as root, where permissions do not apply, rather than passing without
// having tested anything.
func interruptedPublish(t *testing.T, displaced string) (root, staging, destination string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where an unwritable directory is not unwritable")
	}

	root = t.TempDir()
	locked := filepath.Join(root, "locked")
	staging = filepath.Join(locked, "staged")
	destination = filepath.Join(root, "dest")
	write(t, staging, "the object being published")
	write(t, destination, displaced)

	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	// Restored, or the temp tree cannot be removed afterwards.
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	return root, staging, destination
}

func TestAnInterruptedPublishLeavesTheDataAndAMarkerThatPlacesIt(t *testing.T) {
	root, staging, destination := interruptedPublish(t, "THE ONLY COPY")

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("a publish that could not complete reported success")
	}

	displaced := filepath.Join(root, swapPrefix+testID)
	if got := read(t, displaced); got != "THE ONLY COPY" {
		t.Errorf("displaced data holds %q, want THE ONLY COPY", got)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Error("the destination is still there, so this is not the interrupted state")
	}

	// Relative, so nothing that reads it can be sent off the volume by
	// following an absolute path. At the volume root, which is the one
	// directory the sweep reads - not beside the destination, wherever the
	// caller happened to keep it.
	//
	// The identity is of what was being PUBLISHED, which is still at the staging
	// path here: the publish stopped before it could be moved.
	want := "dest\n" + identityOf(t, staging) + "\n"
	if got := read(t, displaced+markerSuffix); got != want {
		t.Errorf("marker holds %q, want %q", got, want)
	}
}

// The record has to name the object being placed, not the one being displaced -
// the sweep compares it against whatever occupies the destination afterwards.
func TestTheMarkerRecordsTheIdentityOfWhatIsBeingPublished(t *testing.T) {
	root, staging, destination := interruptedPublish(t, "displaced")
	displacedIdentity := identityOf(t, destination)

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("a publish that could not complete reported success")
	}

	marker := read(t, filepath.Join(root, swapPrefix+testID)+markerSuffix)
	if want := "dest\n" + identityOf(t, staging) + "\n"; marker != want {
		t.Errorf("marker holds %q, want the identity of the published object %q", marker, want)
	}
	if marker == "dest\n"+displacedIdentity+"\n" {
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
		got, err := markerContents(testCase.destination, testCase.root)
		if err != nil {
			t.Errorf("markerContents(%q, %q): %v", testCase.destination, testCase.root, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("markerContents(%q, %q) = %q, want %q",
				testCase.destination, testCase.root, got, testCase.want)
		}
	}
}

// The shape that cannot be abused is the one that cannot be written down, and
// that only holds if a destination outside the root is REFUSED. Trimming a
// prefix that is not there leaves the absolute path, which is what a reader
// would then be handed - and which the whole relative form exists to prevent.
func TestMarkerContentsRefuseADestinationOutsideTheRoot(t *testing.T) {
	cases := []struct{ root, destination string }{
		{"/work", "/etc/cron.d/payload"},
		{"/work", "/workshop/photos"},
		{"/work", "photos"},
	}
	for _, testCase := range cases {
		if got, err := markerContents(testCase.destination, testCase.root); err == nil {
			t.Errorf("markerContents(%q, %q) = %q, want a refusal",
				testCase.destination, testCase.root, got)
		}
	}
}

// Neither operand may contain the other, and the refusal has to come BEFORE
// anything is displaced - which is the whole difference between an operation
// that did not happen and one that took the caller's folder away.
func TestPublishRefusesOperandsThatContainOneAnother(t *testing.T) {
	// The destination containing staging is a move a client can express: replace
	// photos with photos/2024. Displacing photos carries 2024 away with it, so
	// the publish cannot finish - and everything ELSE in photos goes with it.
	t.Run("destination contains staging", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "photos")
		staging := filepath.Join(destination, "2024")
		write(t, staging, "precious")
		write(t, filepath.Join(destination, "wedding.jpg"), "irreplaceable")

		if err := publish(staging, destination, root, testID); err == nil {
			t.Fatal("publishing a directory over its own parent succeeded")
		}

		// The point of refusing early: the caller's folder is still theirs, and
		// there is nothing for a sweep to have to put back.
		if got := read(t, filepath.Join(destination, "wedding.jpg")); got != "irreplaceable" {
			t.Errorf("a file the caller never named holds %q", got)
		}
		if got := read(t, staging); got != "precious" {
			t.Errorf("staging holds %q", got)
		}
		if _, err := os.Lstat(filepath.Join(root, swapPrefix+testID)); !os.IsNotExist(err) {
			t.Error("something was displaced by an operation that was refused")
		}
	})

	// The mirror. rename(2) cannot move a directory into its own subtree at all,
	// so this one also stops after displacing the destination.
	t.Run("staging contains destination", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "photos")
		destination := filepath.Join(staging, "2024")
		write(t, destination, "precious")

		if err := publish(staging, destination, root, testID); err == nil {
			t.Fatal("publishing a directory into its own subtree succeeded")
		}
		if _, err := os.Lstat(filepath.Join(root, swapPrefix+testID)); !os.IsNotExist(err) {
			t.Error("something was displaced by an operation that was refused")
		}
	})

	// Equal paths are the degenerate case of both, and displacing the
	// destination would leave staging naming nothing.
	t.Run("they are the same entry", func(t *testing.T) {
		root := t.TempDir()
		same := filepath.Join(root, "photos")
		write(t, same, "precious")

		if err := publish(same, same, root, testID); err == nil {
			t.Fatal("publishing an entry over itself succeeded")
		}
		if got := read(t, same); got != "precious" {
			t.Errorf("the entry holds %q", got)
		}
	})
}

// The marker names a path derived from --id, and it is written into a directory
// the application can also write to. A link planted there would be followed, so
// the record of where the caller's data belongs would be written wherever the
// link pointed - and the sweep would then find no marker beside the displaced
// copy and delete it as a duplicate.
//
// Not reachable today, because --id is a fresh randomUUID the application never
// learns. Refused anyway: this program should not depend on its caller having
// picked an unguessable name for a file it writes as root.
func TestTheMarkerIsNotWrittenThroughAPlantedLink(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staged")
	destination := filepath.Join(root, "dest")
	write(t, staging, "the object being published")
	write(t, destination, "displaced")

	elsewhere := filepath.Join(root, "elsewhere")
	write(t, elsewhere, "SOMETHING THE CALLER OWNS")
	if err := os.Symlink(elsewhere, filepath.Join(root, swapPrefix+testID)+markerSuffix); err != nil {
		t.Fatal(err)
	}

	if err := publish(staging, destination, root, testID); err == nil {
		t.Fatal("publishing through a planted marker link succeeded")
	}
	if got := read(t, elsewhere); got != "SOMETHING THE CALLER OWNS" {
		t.Errorf("the link target was written through, and now holds %q", got)
	}
	// Refused before the swap, so the caller still has what they had.
	if got := read(t, destination); got != "displaced" {
		t.Errorf("the destination holds %q", got)
	}
}
