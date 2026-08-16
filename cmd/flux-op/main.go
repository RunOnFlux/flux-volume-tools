// Command flux-op runs one file operation and publishes its result atomically.
//
//	flux-op --id <id> --root <dir> [--discard-staging] [--mkdir] [--max-bytes N]
//	        [--data-only] [--from-stdin] [--no-replace] <staging> <destination>
//	        -- [command [args...]]
//
// --id and --root together decide where the artefacts of an interrupted publish
// land and what they are called: <root>/.flux-old-<id> and its .dest marker.
// Neither is derived from <staging>, because <staging> is not always a directory
// this program created - a move publishes the caller's source where it stands,
// so its "staging" is an arbitrary path at an arbitrary depth. A name derived
// from it collides with what a user might call a folder, and a location derived
// from it lands outside the one directory the startup sweep reads.
//
// The command writes into <staging>, never into <destination>. Only if it
// succeeds is the result moved into place. A command that fails, a container
// that is stopped, and a node that loses power all leave <destination> exactly
// as it was, with the incomplete work parked under a name the sweep recognises.
//
// This lives in the image rather than in the caller so that one container does
// the work AND the publish: the "did it succeed" and "put it in place" decisions
// cannot drift apart, and a second container spawn is not needed per operation.
//
// Operands reach the command as argv and are never interpolated into a command
// string, so there is no shell parsing of anything the caller supplied.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The shape the startup sweep matches when it decides which entries at the
// volume root are this program's artefacts. Checked here as well as there
// because the two must not drift: a name accepted here and refused there is a
// copy of the caller's data left on their volume permanently, at a name the
// browser hides from them.
var operationIdentifier = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Exit codes the caller distinguishes. Everything else is the command's own
// status, passed through unchanged.
const (
	exitUsage             = 2
	exitTooLarge          = 3
	exitNotData           = 4
	exitDestinationExists = 5
	// 128 + SIGTERM, which is what a shell reports for a signalled process and
	// what FluxOS matches on to tell a cancelled operation from a failed one.
	exitCanceled = 143
)

type options struct {
	id             string
	root           string
	discardStaging bool
	makeStaging    bool
	maxBytes       int64
	dataOnly       bool
	fromStdin      bool
	noReplace      bool

	staging     string
	destination string
	command     []string
}

const usage = "flux-op: usage: flux-op --id <id> --root <dir> [--discard-staging] [--mkdir] " +
	"[--max-bytes N] [--data-only] [--from-stdin] [--no-replace] <staging> <destination> " +
	"-- [command [args...]]"

func main() {
	// os.Exit skips deferred functions, so everything that has to run on the way
	// out - reclaiming staging above all - lives under run().
	os.Exit(run(os.Args[1:]))
}

func parse(argv []string) (*options, error) {
	opts := &options{}

	flags := flag.NewFlagSet("flux-op", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage) }

	// Identifies this operation's artefacts. Supplied by the caller so that
	// every name the sweep has to recognise has one exact shape.
	flags.StringVar(&opts.id, "id", "", "identifier for this operation's artefacts")
	// The volume root as this container sees it. Everything an interrupted
	// publish leaves behind goes here, which is the one directory the sweep
	// reads.
	flags.StringVar(&opts.root, "root", "", "the volume root inside the container")
	// Staging is scratch this operation created, so a failure may throw it away.
	// WITHOUT this, staging is never deleted - a move publishes the caller's
	// source where it stands, and there "staging" is the only copy of their
	// data. Opt-in rather than opt-out, because the safe behaviour has to be the
	// one a forgetful caller gets.
	flags.BoolVar(&opts.discardStaging, "discard-staging", false, "staging is scratch and may be discarded")
	flags.BoolVar(&opts.makeStaging, "mkdir", false, "create the staging directory first")
	// A ceiling on what the operation may leave in staging. For a command it is
	// checked afterwards, because the bytes are written by another program and
	// an archive's declared size is written by whoever built it. For --from-stdin
	// it is enforced as the bytes arrive, because this program is the writer.
	flags.Int64Var(&opts.maxBytes, "max-bytes", 0, "refuse a result larger than this")
	// Refuse a result holding a FIFO, a socket or a device node. None of them is
	// data: whatever opens a FIFO without O_NONBLOCK waits for a writer that
	// never comes, so one sitting in an application's volume is a reader that
	// hangs, and nothing an application archives has a reason to be one.
	//
	// Links are NOT refused. What stops a hostile archive reaching anything is
	// this container - one volume mounted, a read-only rootfs, no network - and
	// a link left in the result is answered by the readers at the other end,
	// which open with O_NOFOLLOW and list with lstat.
	flags.BoolVar(&opts.dataOnly, "data-only", false, "refuse a result holding a FIFO, socket or device node")
	// The caller streams the content in rather than naming a command to produce
	// it. There is no child process at all, so nothing can exit successfully on
	// a short read: the transfer ends when the caller closes the stream, and the
	// caller closes it only when it has sent everything.
	flags.BoolVar(&opts.fromStdin, "from-stdin", false, "write this program's standard input into staging")
	// Publish only onto a free name. Without it a publish REPLACES whatever is at
	// the destination, which is what an upload or an overwriting move means; with
	// it the operation is one a caller asked to be told about instead - creating a
	// folder, renaming beside an existing entry. The refusal is the rename's own,
	// so nothing is decided about a state that may since have changed.
	flags.BoolVar(&opts.noReplace, "no-replace", false, "refuse rather than replace an occupied destination")

	if err := flags.Parse(argv); err != nil {
		return nil, errUsage
	}

	rest := flags.Args()
	if len(rest) < 3 || opts.id == "" || opts.root == "" {
		return nil, errUsage
	}

	// Both are joined into paths below, so their shape is not a formality. An
	// identifier carrying a separator puts the artefacts in a subdirectory the
	// sweep never reads, and one carrying traversal puts them outside the volume
	// altogether - in both cases leaving a copy of the caller's data that nothing
	// will ever reclaim.
	if !operationIdentifier.MatchString(opts.id) {
		return nil, fmt.Errorf("flux-op: --id must be an operation identifier, not %q", opts.id)
	}

	root, ok := volumeRoot(opts.root)
	if !ok {
		return nil, fmt.Errorf("flux-op: --root must be an absolute path that leads where it says, not %q", opts.root)
	}
	opts.root = root

	opts.staging, opts.destination = rest[0], rest[1]
	if rest[2] != "--" {
		return nil, errors.New("flux-op: expected -- before the command")
	}
	opts.command = rest[3:]

	if opts.fromStdin && len(opts.command) > 0 {
		return nil, errors.New("flux-op: --from-stdin takes no command")
	}
	if opts.fromStdin && opts.makeStaging {
		return nil, errors.New("flux-op: --from-stdin writes a file, so it cannot also create staging as a directory")
	}

	return opts, nil
}

var errUsage = errors.New(usage)

func run(argv []string) int {
	opts, err := parse(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}

	// Anything that fails from here until the publish begins leaves staging
	// behind, and nobody is coming back for it - the operation is over.
	// Reclaiming it here rather than leaving it to the startup sweep means a
	// refused extraction gives the volume its space back immediately, which
	// matters because the reason it was refused may well have been size.
	//
	// Disarmed the moment the swap starts: past that point the same path names
	// the caller's PREVIOUS data, and deleting that is the one outcome this
	// program exists to prevent.
	reclaimStaging := true
	defer func() {
		if reclaimStaging && opts.discardStaging {
			os.RemoveAll(opts.staging)
		}
	}()

	// Asked for across the whole run, not only around a child.
	//
	// The executor runs this as PID 1, and the kernel delivers no signal to PID 1
	// that the process has not asked for. So an unhandled TERM does not end the
	// program the way it would anywhere else - it is discarded, the Go runtime
	// gives up and exits 2, and NOTHING unwinds: the reclaim below never runs and
	// a partial upload is left at a name the app owner cannot see.
	//
	// Registering here suppresses that for every phase, including the publish,
	// where being ended between two renames is the one outcome this program
	// exists to prevent.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	// Only the commands that need an existing directory to write into ask for
	// this. A file copy must NOT have it: cp -T refuses to overwrite a directory
	// with a non-directory.
	if opts.makeStaging {
		if err := os.MkdirAll(opts.staging, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "flux-op: could not create staging: %v\n", err)
			return 1
		}
	}

	switch {
	case opts.fromStdin:
		// There is no child here to forward the signal to, and the read cannot be
		// interrupted: it wakes when bytes arrive or the sender closes, and a
		// stalled sender does neither. Closing the descriptor does not help - the
		// runtime does not poll standard input, so a read already blocked on it
		// stays blocked, and the container is SIGKILLed at the end of its grace
		// period with nothing unwound.
		//
		// So the transfer is waited ON rather than waited FOR: it runs in its own
		// goroutine and this one takes whichever arrives first. A cancel then
		// returns through the ordinary path, which is what runs the reclaim above.
		// The abandoned goroutine writes into a descriptor whose file is being
		// unlinked and the process is gone moments later; it creates nothing, so
		// there is nothing for it to leave behind.
		transfer := make(chan error, 1)
		go func() {
			_, err := receive(os.Stdin, opts.staging, opts.maxBytes)
			transfer <- err
		}()

		select {
		case <-signals:
			fmt.Fprintln(os.Stderr, "flux-op: cancelled")
			return exitCanceled
		case err := <-transfer:
			if err != nil {
				if errors.Is(err, errOverCeiling) {
					fmt.Fprintf(os.Stderr, "flux-op: input is over the %d byte limit\n", opts.maxBytes)
					return exitTooLarge
				}
				fmt.Fprintf(os.Stderr, "flux-op: could not write the incoming data: %v\n", err)
				return 1
			}
		}

	case len(opts.command) > 0:
		status, canceled, err := runChild(opts.command, os.Stdin, os.Stdout, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "flux-op: could not run the command: %v\n", err)
			return 1
		}
		if canceled {
			fmt.Fprintln(os.Stderr, "flux-op: cancelled")
			return exitCanceled
		}
		if status != 0 {
			return status
		}

		// An EMPTY command is legitimate and is how a move is expressed: its
		// source is already the result, so there is nothing to run and
		// publishing it is the whole operation.
	}

	// A signal that arrived during a phase with nothing to interrupt still means
	// the caller asked for this to stop, and stopping before the publish is what
	// makes honouring it free.
	select {
	case <-signals:
		fmt.Fprintln(os.Stderr, "flux-op: cancelled")
		return exitCanceled
	default:
	}

	// One pass over the result answers both questions. The ceiling is checked
	// against what actually landed rather than against what the input claimed
	// about itself, because those numbers are written by whoever built the
	// archive and a bomb simply lies.
	if opts.maxBytes > 0 || opts.dataOnly {
		// --from-stdin has already enforced its own ceiling as it wrote, and
		// cannot produce a link.
		if !opts.fromStdin {
			result, err := inspect(opts.staging)
			if err != nil {
				fmt.Fprintf(os.Stderr, "flux-op: could not inspect the result: %v\n", err)
				return 1
			}
			if opts.maxBytes > 0 && result.bytes > opts.maxBytes {
				fmt.Fprintf(os.Stderr, "flux-op: result is %d bytes, over the %d limit\n", result.bytes, opts.maxBytes)
				return exitTooLarge
			}
			if opts.dataOnly && result.hasNonData {
				fmt.Fprintf(os.Stderr, "flux-op: result holds %s, which is not data and is not accepted here\n", result.nonData)
				return exitNotData
			}
		}
	}

	reclaimStaging = false

	if err := publish(opts.staging, opts.destination, opts.root, opts.id, opts.noReplace); err != nil {
		// A publish that refused before moving anything leaves staging as this
		// operation's own scratch rather than as the caller's data under another
		// name, so it goes back now instead of waiting for the next boot sweep.
		if errors.Is(err, errDestinationExists) || errors.Is(err, errNothingMoved) {
			reclaimStaging = true
		}
		if errors.Is(err, errDestinationExists) {
			fmt.Fprintf(os.Stderr, "flux-op: %v\n", err)
			return exitDestinationExists
		}
		fmt.Fprintf(os.Stderr, "flux-op: could not publish the result: %v\n", err)
		return 1
	}

	return 0
}

// volumeRoot normalises the volume root, and reports whether it is one.
//
// Absolute, because everything here is built by joining onto it and a relative
// root resolves against whatever directory this happened to be started in.
// Rejected rather than cleaned when cleaning would CHANGE where it points: a
// root of /work/../etc is a path that does not lead where it says, and silently
// accepting it as /etc would put this program to work somewhere nobody named.
func volumeRoot(root string) (string, bool) {
	cleaned := filepath.Clean(root)
	if !filepath.IsAbs(cleaned) {
		return "", false
	}
	if cleaned != root && cleaned != strings.TrimRight(root, string(filepath.Separator)) {
		return "", false
	}
	return cleaned, true
}
