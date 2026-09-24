package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	return append(append([]string{"--root", v.root}, extra...),
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
		{"no volume root", []string{v.staging, v.destination, "--"}},
		{"no operands", []string{"--root", v.root}},
		{"no -- before the command", []string{"--root", v.root, v.staging, v.destination, "true"}},
		// After the --, which is where a command goes. Written as an extra
		// operand it was refused for missing its separator instead, so the check
		// this names was never reached.
		{"input and a command together", append(v.argv("--from-stdin"), "true")},
		{"input into a staging directory", v.argv("--from-stdin", "--mkdir")},
		// The operands are checked against the root, and a relative one resolves
		// against whatever directory this happens to be run from.
		{"a volume root that is not absolute", []string{
			"--root", "work", v.staging, v.destination, "--"}},
		// Cleaning this would change where it points, so it is refused rather
		// than quietly accepted as the directory it actually names.
		{"a volume root that does not lead where it says", []string{
			"--root", v.root + "/../etc", v.staging, v.destination, "--"}},
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

	argv := []string{"--root", v.root, "--max-bytes", "1", source, v.destination, "--"}
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

	argv := []string{"--root", v.root, source, v.destination, "--"}
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

// The ceiling holds while the command writes, not only once it has finished:
// the volume it is the free space of never has more than the ceiling taken out
// of it. Staging is kept here so the partial file can be measured.
func TestACommandIsStoppedAtTheCeilingAsItWrites(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--max-bytes", "1000"),
		"sh", "-c", "head -c 4000 /dev/zero > "+v.staging)

	if code := run(argv); code != exitTooLarge {
		t.Fatalf("exit %d, want %d", code, exitTooLarge)
	}
	info, err := os.Lstat(v.staging)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1000 {
		t.Errorf("the command wrote %d bytes past a ceiling of 1000", info.Size())
	}
}

// zip writes into a temporary file beside its output and renames it into place
// at the end, so when the cap stops it the result itself holds nothing. The
// signal it was killed by is what says why.
func TestACommandKilledByTheCapBesideItsResultIsTooLarge(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--max-bytes", "1000"),
		"sh", "-c", "head -c 4000 /dev/zero > "+filepath.Join(v.root, "scratch"))

	if code := run(argv); code != exitTooLarge {
		t.Fatalf("exit %d, want %d", code, exitTooLarge)
	}
}

// A command that fails for a reason of its own, well inside the ceiling, is
// reported as itself rather than as too large.
func TestAFailureUnderTheCeilingKeepsItsOwnStatus(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--max-bytes", "1000"),
		"sh", "-c", "head -c 10 /dev/zero > "+v.staging+"; exit 7")

	if code := run(argv); code != 7 {
		t.Fatalf("exit %d, want 7", code)
	}
}

// Without a ceiling nothing is capped, and the limit this process runs under
// afterwards is the one it started with - it is lowered for the command only.
func TestTheFileSizeLimitIsOnlyLoweredForACommandWithACeiling(t *testing.T) {
	var before syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
		t.Fatal(err)
	}

	v := newVolume(t)
	argv := append(v.argv("--discard-staging"),
		"sh", "-c", "head -c 4000 /dev/zero > "+v.staging)
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := len(read(t, v.destination)); got != 4000 {
		t.Errorf("published %d bytes, want 4000", got)
	}

	capped := newVolume(t)
	run(append(capped.argv("--discard-staging", "--max-bytes", "1000"),
		"sh", "-c", "head -c 4000 /dev/zero > "+capped.staging))

	var after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the file size limit is %+v after a run, was %+v", after, before)
	}
}

// A link in the result is published. Nothing here reads through one, and the
// readers at the other end do not follow one either - so refusing it would only
// take content away from the owner it belongs to.
func TestAResultContainingALinkIsPublished(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--mkdir", "--data-only"),
		"sh", "-c", "ln -s /etc/hosts "+filepath.Join(v.staging, "link"))

	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}

	info, err := os.Lstat(filepath.Join(v.destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was published as something other than a link")
	}
}

// A FIFO is not published. Whatever opens one without O_NONBLOCK waits for a
// writer that is never coming, so leaving one on the volume leaves a reader that
// hangs.
func TestAResultContainingAFifoIsRefused(t *testing.T) {
	v := newVolume(t)
	argv := append(v.argv("--discard-staging", "--mkdir", "--data-only"),
		"sh", "-c", "mkfifo "+filepath.Join(v.staging, "pipe"))

	if code := run(argv); code != exitNotData {
		t.Fatalf("exit %d, want %d", code, exitNotData)
	}
	if _, err := os.Lstat(v.destination); err == nil {
		t.Error("the destination was published despite the refusal")
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("a refused result left %v behind", left)
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

	argv := []string{"--root", v.root, inner, v.destination, "--"}
	if code := run(argv); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got := read(t, filepath.Join(v.destination, "wedding.jpg")); got != "irreplaceable" {
		t.Errorf("a file the caller never named holds %q", got)
	}
}

// The destination is inside the volume by construction, so one that is not
// means something upstream is wrong and the operation stops rather than acting
// on it. This used to fall out of writing the marker, whose contents were the
// destination relative to the root; it is its own check now, because a guard
// riding on something else disappears when that something else does.
func TestAPublishRefusesAnOperandOutsideTheVolume(t *testing.T) {
	v := newVolume(t)
	outside := filepath.Join(t.TempDir(), "dest")
	write(t, v.staging, "the object being published")
	write(t, outside, "displaced")

	if err := publish(v.staging, outside, v.root, false, false); err == nil {
		t.Fatal("published to a destination outside the volume root")
	}
	if got := read(t, outside); got != "displaced" {
		t.Errorf("the destination holds %q", got)
	}
}

// The whole reason --no-replace has a status of its own: the caller turns it
// into "that name is in use" for an app owner, and it cannot get that from a
// message without matching on the wording of whichever tool ran.
func TestAnOccupiedDestinationHasItsOwnStatus(t *testing.T) {
	v := newVolume(t)
	write(t, v.staging, "the result")
	write(t, v.destination, "the caller's")

	if code := run(v.argv("--discard-staging", "--no-replace")); code != exitDestinationExists {
		t.Fatalf("exit %d, want %d for a destination that is already there", code, exitDestinationExists)
	}

	if got := read(t, v.destination); got != "the caller's" {
		t.Errorf("destination holds %q, so a refused publish wrote something", got)
	}
	// Nothing was exchanged, so staging is still this operation's own scratch and
	// goes back now rather than waiting for the next boot sweep to notice it.
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("a refused publish left %v behind", left)
	}
}

// Creating a folder is this shape: no command at all, staging made here, and a
// name that must be free. It is the same publish as everything else, which is
// what keeps the create out of the caller's hands.
func TestCreatingADirectoryOnAFreeName(t *testing.T) {
	v := newVolume(t)

	if code := run(v.argv("--discard-staging", "--mkdir", "--no-replace")); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}

	info, err := os.Lstat(v.destination)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("the destination is not a directory")
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("creating a directory left %v behind", left)
	}
}

// And the same shape onto a name that is taken, which is what an owner clicking
// the button twice does.
func TestCreatingADirectoryOnAnOccupiedName(t *testing.T) {
	v := newVolume(t)
	write(t, filepath.Join(v.destination, "theirs.txt"), "precious")

	if code := run(v.argv("--discard-staging", "--mkdir", "--no-replace")); code != exitDestinationExists {
		t.Fatalf("exit %d, want %d", code, exitDestinationExists)
	}

	if got := read(t, filepath.Join(v.destination, "theirs.txt")); got != "precious" {
		t.Errorf("a folder that was already there holds %q", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("a refused create left %v behind", left)
	}
}

// Without the flag the publish replaces, which is what an upload is. Asserted
// here as well as in the publish tests because the flag is plumbed through
// run(), and a flag that is parsed and never passed on reads as working.
func TestWithoutTheFlagAPublishStillReplaces(t *testing.T) {
	v := newVolume(t)
	write(t, v.staging, "the result")
	write(t, v.destination, "superseded")

	if code := run(v.argv("--discard-staging")); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, v.destination); got != "the result" {
		t.Errorf("destination holds %q, want the result", got)
	}
}
