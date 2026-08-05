package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiveWritesTheWholeStream(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	contents := strings.Repeat("payload", 10000)

	written, err := receive(strings.NewReader(contents), staging, 0)
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(contents)) {
		t.Errorf("wrote %d bytes, want %d", written, len(contents))
	}
	if got := read(t, staging); got != contents {
		t.Error("what landed is not what was sent")
	}
}

// The ceiling is exact here, which it cannot be for a command: a command
// produces its own bytes, so the only thing that can be checked is what it left
// behind. Nothing over the limit reaches the disk, which matters because the
// volume being filled is the one the application's database is on.
func TestReceiveWritesNothingPastTheCeiling(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")

	written, err := receive(strings.NewReader(strings.Repeat("x", 5000)), staging, 1000)
	if !errors.Is(err, errOverCeiling) {
		t.Fatalf("error %v, want the ceiling refusal", err)
	}
	if written > 1000 {
		t.Errorf("reported %d bytes written, over the 1000 ceiling", written)
	}

	info, statErr := os.Stat(staging)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() > 1000 {
		t.Errorf("%d bytes reached the disk, over the 1000 ceiling", info.Size())
	}
}

func TestReceiveAcceptsAStreamExactlyAtTheCeiling(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")

	written, err := receive(strings.NewReader(strings.Repeat("x", 1000)), staging, 1000)
	if err != nil {
		t.Fatalf("a stream exactly at the ceiling was refused: %v", err)
	}
	if written != 1000 {
		t.Errorf("wrote %d bytes, want 1000", written)
	}
}

func TestReceiveWritesAnEmptyStreamAsAnEmptyFile(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")

	written, err := receive(strings.NewReader(""), staging, 0)
	if err != nil {
		t.Fatal(err)
	}
	if written != 0 {
		t.Errorf("wrote %d bytes for an empty stream", written)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Error("an empty stream produced no file at all")
	}
}

// Staging names a path this operation created for itself. Anything already
// there means the caller passed a path it does not own, and truncating it is
// the outcome this program exists to prevent.
func TestReceiveRefusesToWriteOverAnExistingPath(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	write(t, staging, "somebody else's data")

	if _, err := receive(strings.NewReader("mine"), staging, 0); err == nil {
		t.Fatal("wrote over an existing path")
	}
	if got := read(t, staging); got != "somebody else's data" {
		t.Error("the existing data was modified")
	}
}
