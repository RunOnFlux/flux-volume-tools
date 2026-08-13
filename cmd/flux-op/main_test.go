package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdin points this process's standard input at a file holding contents,
// for the duration of one test.
func withStdin(t *testing.T, contents string) {
	t.Helper()
	original := os.Stdin
	os.Stdin = fileHolding(t, contents)
	t.Cleanup(func() { os.Stdin = original })
}

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
		// After the --, which is where a command goes. Written as an extra
		// operand it was refused for missing its separator instead, so the check
		// this names was never reached.
		{"input and a command together", append(v.argv("--from-stdin"), "true")},
		{"input into a staging directory", v.argv("--from-stdin", "--mkdir")},

		// The identifier names what an interrupted publish leaves behind, and
		// the sweep that reclaims those artefacts matches one exact shape. It is
		// checked here as well so the two cannot drift: a name this accepts and
		// the sweep does not is a copy of the caller's data left on their volume
		// permanently, invisible to them.
		{"an identifier that is not the shape the sweep matches", []string{
			"--id", "not-a-uuid", "--root", v.root, v.staging, v.destination, "--"}},
		// Joined into a path, so traversal in it leaves the volume entirely.
		{"an identifier that traverses", []string{
			"--id", "../../escape", "--root", v.root, v.staging, v.destination, "--"}},
		// A separator puts the artefacts in a subdirectory. The sweep reads the
		// volume root and nowhere else, so nothing would ever reclaim them.
		{"an identifier holding a separator", []string{
			"--id", "3f2504e0-4f89-41d3-9a0c-0305e82c3301/sub", "--root", v.root, v.staging, v.destination, "--"}},
		// Everything here is built by joining onto the root, and a relative one
		// resolves against whatever directory this happens to be run from.
		{"a volume root that is not absolute", []string{
			"--id", testID, "--root", "work", v.staging, v.destination, "--"}},
		// Cleaning this would change where it points, so it is refused rather
		// than quietly accepted as the directory it actually names.
		{"a volume root that does not lead where it says", []string{
			"--id", testID, "--root", v.root + "/../etc", v.staging, v.destination, "--"}},
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
	argv := append(v.argv("--discard-staging", "--mkdir", "--ordinary-only"),
		"sh", "-c", "ln -s /etc/hosts "+filepath.Join(v.staging, "link"))

	if code := run(argv); code != exitNotOrdinary {
		t.Fatalf("exit %d, want %d", code, exitNotOrdinary)
	}
	if _, err := os.Lstat(v.destination); err == nil {
		t.Error("the destination was published despite the refusal")
	}
}

// The upload path: no child process, so nothing can exit successfully having
// read half a stream.
func TestInputIsWrittenIntoStagingAndPublished(t *testing.T) {
	v := newVolume(t)
	withStdin(t, "uploaded content")

	if code := run(v.argv("--discard-staging", "--from-stdin")); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, v.destination); got != "uploaded content" {
		t.Errorf("destination holds %q", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

func TestInputOverTheCeilingIsRefusedAndNothingIsPublished(t *testing.T) {
	v := newVolume(t)
	write(t, v.destination, "original")
	withStdin(t, strings.Repeat("x", 5000))

	if code := run(v.argv("--discard-staging", "--from-stdin", "--max-bytes", "1000")); code != exitTooLarge {
		t.Fatalf("exit %d, want %d", code, exitTooLarge)
	}
	if got := read(t, v.destination); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

func TestInputReplacesAnExistingDestination(t *testing.T) {
	v := newVolume(t)
	write(t, v.destination, "old")
	withStdin(t, "new")

	if code := run(v.argv("--discard-staging", "--from-stdin")); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, v.destination); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// A result that cannot be measured is not a result that may be published. The
// ceiling is the reason inspect runs at all, so failing to answer it has to stop
// the operation rather than wave it through.
func TestAResultThatCannotBeMeasuredIsNotPublished(t *testing.T) {
	v := newVolume(t)
	write(t, v.destination, "THE ONLY COPY")

	// A link where the result should be. Nothing here follows one, so measuring
	// it would report the few bytes of its own path whatever sits behind it.
	elsewhere := filepath.Join(v.root, "elsewhere")
	write(t, filepath.Join(elsewhere, "big"), strings.Repeat("x", 5000))
	if err := os.Symlink(elsewhere, v.staging); err != nil {
		t.Fatal(err)
	}

	if code := run(v.argv("--max-bytes", "1000")); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got := read(t, v.destination); got != "THE ONLY COPY" {
		t.Errorf("the destination holds %q", got)
	}
}

// A publish that refuses reports a failure rather than a success. The refusals
// themselves are covered in publish_test; this is that they reach the caller.
func TestAPublishThatIsRefusedFailsTheOperation(t *testing.T) {
	v := newVolume(t)
	inner := filepath.Join(v.destination, "2024")
	write(t, inner, "precious")
	write(t, filepath.Join(v.destination, "wedding.jpg"), "irreplaceable")

	argv := []string{"--id", testID, "--root", v.root, inner, v.destination, "--"}
	if code := run(argv); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got := read(t, filepath.Join(v.destination, "wedding.jpg")); got != "irreplaceable" {
		t.Errorf("a file the caller never named holds %q", got)
	}
}

// The destination is inside the volume by construction, so a marker that cannot
// be written relative to the root means something upstream is wrong - and the
// operation stops rather than publishing with no record of what it displaced.
func TestAPublishStopsWhenWhereItBelongsCannotBeRecorded(t *testing.T) {
	v := newVolume(t)
	outside := filepath.Join(t.TempDir(), "dest")
	write(t, v.staging, "the object being published")
	write(t, outside, "displaced")

	if err := publish(v.staging, outside, v.root, testID); err == nil {
		t.Fatal("published to a destination outside the volume root")
	}
	if got := read(t, outside); got != "displaced" {
		t.Errorf("the destination holds %q", got)
	}
}
