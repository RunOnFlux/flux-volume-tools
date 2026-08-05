package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A volume with the operands the tests below share.
type volume struct {
	root        string
	staging     string
	destination string
}

func newVolume(t *testing.T) volume {
	t.Helper()
	root := t.TempDir()
	return volume{
		root:        root,
		staging:     filepath.Join(root, ".flux-op-"+testID),
		destination: filepath.Join(root, "dest"),
	}
}

func (v volume) argv(extra ...string) []string {
	return append(append([]string{"--id", testID, "--root", v.root}, extra...),
		v.staging, v.destination, "--")
}

func (v volume) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(v.root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".flux-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestUsageIsRefusedWithoutRunningAnything(t *testing.T) {
	v := newVolume(t)
	cases := []struct {
		name string
		argv []string
	}{
		{"no identifier", []string{"--root", v.root, v.staging, v.destination, "--"}},
		{"no volume root", []string{"--id", testID, v.staging, v.destination, "--"}},
		{"no operands", []string{"--id", testID, "--root", v.root}},
		{"no -- before the command", []string{"--id", testID, "--root", v.root, v.staging, v.destination, "true"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if code := run(testCase.argv); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if _, err := os.Lstat(v.destination); err == nil {
				t.Error("the destination was touched by a refused invocation")
			}
		})
	}
}

func TestAFailedCommandLeavesTheDestinationAndReclaimsStaging(t *testing.T) {
	v := newVolume(t)
	write(t, v.destination, "original")

	argv := append(v.argv("--discard-staging", "--mkdir"), "false")
	if code := run(argv); code == 0 {
		t.Fatal("a failing command reported success")
	}
	if got := read(t, v.destination); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// Staging is only ever discarded when the caller says it owns it. A move's
// operand is the user's own data, and discarding it on a failure would destroy
// the only copy.
func TestAFailureNeverDiscardsAnOperandTheCallerOwns(t *testing.T) {
	v := newVolume(t)
	source := filepath.Join(v.root, "photos")
	write(t, filepath.Join(source, "f"), "precious")

	argv := []string{"--id", testID, "--root", v.root, "--max-bytes", "1", source, v.destination, "--"}
	if code := run(argv); code != exitTooLarge {
		t.Fatalf("exit %d, want %d", code, exitTooLarge)
	}
	if got := read(t, filepath.Join(source, "f")); got != "precious" {
		t.Error("the caller's own data was discarded")
	}
}

// A move has no command: the source already IS the result, so publishing it is
// the whole operation.
func TestAMovePublishesWithNoCommandAtAll(t *testing.T) {
	v := newVolume(t)
	source := filepath.Join(v.root, "photos")
	write(t, filepath.Join(source, "f"), "hi")

	argv := []string{"--id", testID, "--root", v.root, source, v.destination, "--"}
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, filepath.Join(v.destination, "f")); got != "hi" {
		t.Errorf("destination holds %q", got)
	}
	if _, err := os.Lstat(source); err == nil {
		t.Error("the source survived a move")
	}
}

func TestAResultOverTheCeilingIsRefusedAndReclaimed(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--max-bytes", "1000"),
		"sh", "-c", "head -c 4000 /dev/zero > "+v.staging)

	if code := run(argv); code != exitTooLarge {
		t.Fatalf("exit %d, want %d", code, exitTooLarge)
	}
	if _, err := os.Lstat(v.destination); err == nil {
		t.Error("the destination was published despite the refusal")
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

func TestAResultContainingALinkIsRefused(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--mkdir", "--no-links"),
		"sh", "-c", "ln -s /etc/hosts "+filepath.Join(v.staging, "link"))

	if code := run(argv); code != exitHasLinks {
		t.Fatalf("exit %d, want %d", code, exitHasLinks)
	}
	if _, err := os.Lstat(v.destination); err == nil {
		t.Error("the destination was published despite the refusal")
	}
}
