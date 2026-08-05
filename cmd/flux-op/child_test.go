package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fileHolding returns a *os.File positioned at the start of contents.
func fileHolding(t *testing.T, contents string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func fileForWriting(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

// The command must receive the caller's standard input.
//
// The shell implementation this replaced ran the command as a background job,
// and POSIX assigns /dev/null as the standard input of an asynchronous list when
// job control is off. So a command that read its input got nothing at all,
// silently, and an upload streamed into the container arrived nowhere. Nothing
// failed - the command simply saw an empty stream and exited successfully.
func TestChildReceivesStandardInput(t *testing.T) {
	stdin := fileHolding(t, "the caller's bytes")
	stdout := fileForWriting(t, "out")

	status, canceled, err := runChild([]string{"cat"}, stdin, stdout, os.Stderr)
	if err != nil {
		t.Fatalf("could not run: %v", err)
	}
	if status != 0 || canceled {
		t.Fatalf("status %d canceled %v, want 0 false", status, canceled)
	}

	got, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the caller's bytes" {
		t.Errorf("the command read %q, want the caller's bytes - it was handed the wrong descriptor", got)
	}
}

func TestChildExitStatusPassesThrough(t *testing.T) {
	for _, want := range []int{0, 1, 3, 42} {
		status, canceled, err := runChild(
			[]string{"sh", "-c", "exit " + itoa(want)},
			fileHolding(t, ""), os.Stdout, os.Stderr,
		)
		if err != nil {
			t.Fatalf("could not run: %v", err)
		}
		if canceled {
			t.Errorf("exit %d reported as canceled", want)
		}
		if status != want {
			t.Errorf("status %d, want %d", status, want)
		}
	}
}

// A stop reaches this program, not the command, so it has to be forwarded or the
// command keeps writing into a staging directory nobody will publish.
func TestChildIsSignalledAndReportsCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "trapped")

	// Writes the marker when it is signalled, so the test asserts the command
	// actually received it rather than that this program noticed one.
	script := "trap 'echo caught > " + marker + "; exit 7' TERM; while :; do sleep 0.05; done"

	go func() {
		time.Sleep(300 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()

	status, canceled, err := runChild(
		[]string{"sh", "-c", script},
		fileHolding(t, ""), os.Stdout, os.Stderr,
	)
	if err != nil {
		t.Fatalf("could not run: %v", err)
	}
	if !canceled {
		t.Error("the run was not reported as cancelled")
	}
	if status != 7 {
		t.Errorf("status %d, want 7 - the command's own answer to the signal", status)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the command never received the signal; it was not forwarded")
	}
}

// A command killed by a signal reports what a shell would report for it.
func TestChildKilledBySignalReportsShellStatus(t *testing.T) {
	status, _, err := runChild(
		[]string{"sh", "-c", "kill -9 $$"},
		fileHolding(t, ""), os.Stdout, os.Stderr,
	)
	if err != nil {
		t.Fatalf("could not run: %v", err)
	}
	if status != 128+int(syscall.SIGKILL) {
		t.Errorf("status %d, want %d", status, 128+int(syscall.SIGKILL))
	}
}

func TestChildThatCannotStartIsAnError(t *testing.T) {
	_, _, err := runChild(
		[]string{"there-is-no-such-command-here"},
		fileHolding(t, ""), os.Stdout, os.Stderr,
	)
	if err == nil {
		t.Error("a command that does not exist started successfully")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
