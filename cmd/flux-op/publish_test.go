package main

import (
	"errors"
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

	if err := publish(staging, destination, root, testID, false); err != nil {
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

			if err := publish(staging, destination, root, testID, false); err != nil {
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

			// A completed publish leaves the staging name empty. The old
			// data is under it between the exchange and the cleanup, and
			// the cleanup is the last thing publish does.
			if _, err := os.Lstat(staging); err == nil {
				t.Error("the staging entry was left behind")
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

	if err := publish(staging, destination, root, testID, false); err != nil {
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

// Nothing is displaced over an operation that cannot be carried out, so there is
// nothing for a sweep to put back afterwards.
func TestAMissingStagingPathFailsBeforeAnythingMoves(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, destination, "precious")

	if err := publish(staging, destination, root, testID, false); err == nil {
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

		if err := publish(staging, destination, root, testID, false); err == nil {
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
		if _, err := os.Lstat(destination); err != nil {
			t.Error("something was displaced by an operation that was refused")
		}
	})

	// The mirror. rename(2) cannot move a directory into its own subtree at
	// all, so this one could not have worked either.
	t.Run("staging contains destination", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "photos")
		destination := filepath.Join(staging, "2024")
		write(t, destination, "precious")

		if err := publish(staging, destination, root, testID, false); err == nil {
			t.Fatal("publishing a directory into its own subtree succeeded")
		}
		if _, err := os.Lstat(destination); err != nil {
			t.Error("something was displaced by an operation that was refused")
		}
	})

	// Equal paths are the degenerate case of both, and displacing the
	// destination would leave staging naming nothing.
	t.Run("they are the same entry", func(t *testing.T) {
		root := t.TempDir()
		same := filepath.Join(root, "photos")
		write(t, same, "precious")

		if err := publish(same, same, root, testID, false); err == nil {
			t.Fatal("publishing an entry over itself succeeded")
		}
		if got := read(t, same); got != "precious" {
			t.Errorf("the entry holds %q", got)
		}
	})
}

// The property the whole recovery scheme used to exist to survive, now stated
// directly: at NO point is the destination absent.
//
// The old publish was two renames, and between them the caller's data sat under
// a name their file browser hides while their own path was empty. Everything
// downstream - the marker, the recorded identity, the boot sweep reading a file
// the application can write to - was there to get out of that state. An
// exchange has no such state to get out of.
func TestPublishNeverLeavesTheDestinationAbsent(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, filepath.Join(staging, "new.txt"), "the result")
	write(t, filepath.Join(destination, "mine.txt"), "the caller's")

	// Watched from another goroutine while the publish runs. A window of the
	// kind the two renames opened would be seen as an absent destination.
	stop := make(chan struct{})
	absent := make(chan bool, 1)
	go func() {
		sawAbsent := false
		for {
			select {
			case <-stop:
				absent <- sawAbsent
				return
			default:
				if _, err := os.Lstat(destination); os.IsNotExist(err) {
					sawAbsent = true
				}
			}
		}
	}()

	if err := publish(staging, destination, root, testID, false); err != nil {
		t.Fatal(err)
	}
	close(stop)

	if <-absent {
		t.Error("the destination was absent at some point during the publish")
	}
	if got := read(t, filepath.Join(destination, "new.txt")); got != "the result" {
		t.Errorf("destination holds %q, want the result", got)
	}
}

// What a crash between the exchange and the cleanup leaves, and why the sweep
// needs no marker to decide about it.
//
// Both reachable states put something complete at the destination and something
// disposable under the staging name, so recovery is not a judgement: delete the
// staging entry, which is what the sweep already does with one.
func TestTheOldDataIsLeftUnderTheStagingNameForTheSweep(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, filepath.Join(staging, "new.txt"), "the result")
	write(t, filepath.Join(destination, "mine.txt"), "the caller's")

	// The exchange alone, which is the state a crash before the cleanup leaves.
	if err := exchange(staging, destination); err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(destination, "new.txt")); got != "the result" {
		t.Errorf("destination holds %q, want the result", got)
	}
	if got := read(t, filepath.Join(staging, "mine.txt")); got != "the caller's" {
		t.Errorf("staging holds %q, want the caller's superseded data", got)
	}
}

// Nothing is written beside the operands. The marker was a file in a directory
// the application can write to, and reading it on the boot sweep was the root
// of two separate findings; a publish that records nothing cannot be read.
func TestPublishWritesNoMarkerBesideTheOperands(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, filepath.Join(staging, "new.txt"), "the result")
	write(t, filepath.Join(destination, "mine.txt"), "the caller's")

	if err := publish(staging, destination, root, testID, false); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "dest" {
			t.Errorf("publish left %q behind", entry.Name())
		}
	}
}

// The refusal --no-replace exists for: the caller is told the name is taken, and
// what is under that name is exactly as it was.
func TestPublishRefusesAnOccupiedDestinationWhenAskedNot(t *testing.T) {
	// Each is a different kind of occupied, and a check written with stat rather
	// than with the rename would have had to remember all three.
	cases := map[string]func(t *testing.T, at string){
		"a file": func(t *testing.T, at string) { write(t, at, "precious") },
		"an empty directory": func(t *testing.T, at string) {
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"a dangling symlink": func(t *testing.T, at string) {
			if err := os.Symlink(filepath.Join(at, "..", "gone"), at); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, occupy := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			staging := filepath.Join(root, ".flux-op-"+testID)
			destination := filepath.Join(root, "dest")
			write(t, staging, "the result")
			occupy(t, destination)

			err := publish(staging, destination, root, testID, true)
			if !errors.Is(err, errDestinationExists) {
				t.Fatalf("publishing onto %s gave %v, want a refusal the caller can tell apart", name, err)
			}

			// Both sides untouched: the caller's entry is theirs, and the result is
			// still in staging for the caller to reclaim.
			if got := read(t, staging); got != "the result" {
				t.Errorf("staging holds %q", got)
			}
			if _, err := os.Lstat(destination); err != nil {
				t.Errorf("the destination was disturbed by a publish that was refused: %v", err)
			}
		})
	}
}

// And it is a refusal only when there is something to refuse.
func TestPublishTakesAFreeNameWhenAskedNotToReplace(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, staging, "the result")

	if err := publish(staging, destination, root, testID, true); err != nil {
		t.Fatal(err)
	}

	if got := read(t, destination); got != "the result" {
		t.Errorf("destination holds %q, want the result", got)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Error("staging is still there, so the publish copied rather than renamed")
	}
}

// Without the flag a publish still REPLACES, which is what an upload and an
// overwriting move are. The two behaviours share every other check, so the one
// that destroys data has to be the one a caller asks for explicitly.
func TestPublishStillReplacesWithoutTheFlag(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".flux-op-"+testID)
	destination := filepath.Join(root, "dest")
	write(t, staging, "the result")
	write(t, destination, "superseded")

	if err := publish(staging, destination, root, testID, false); err != nil {
		t.Fatal(err)
	}

	if got := read(t, destination); got != "the result" {
		t.Errorf("destination holds %q, want the result", got)
	}
}
