// Command flux-op runs one file operation and publishes its result atomically.
//
//	flux-op --id <id> --root <dir> [--discard-staging] [--mkdir] [--max-bytes N]
//	        [--no-links] <staging> <destination> -- [command [args...]]
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
	"path/filepath"
	"strings"
)

// Exit codes the caller distinguishes. Everything else is the command's own
// status, passed through unchanged.
const (
	exitUsage    = 2
	exitTooLarge = 3
	exitHasLinks = 4
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
	noLinks        bool

	staging     string
	destination string
	command     []string
}

const usage = "flux-op: usage: flux-op --id <id> --root <dir> [--discard-staging] [--mkdir] " +
	"[--max-bytes N] [--no-links] <staging> <destination> -- [command [args...]]"

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
	// A ceiling on what the operation may leave in staging. Checked after the
	// command runs rather than from what an archive declares about itself:
	// those numbers are written by whoever made the archive, so a bomb lies.
	flags.Int64Var(&opts.maxBytes, "max-bytes", 0, "refuse a result larger than this")
	// Refuse a result containing links. An archive that carries a symlink and
	// then writes through it reaches wherever the link points; inside this
	// container that is nowhere useful, but the result is published onto a
	// volume that other code paths - and other nodes, through sync - do read.
	flags.BoolVar(&opts.noLinks, "no-links", false, "refuse a result containing links")

	if err := flags.Parse(argv); err != nil {
		return nil, errUsage
	}

	rest := flags.Args()
	if len(rest) < 3 || opts.id == "" || opts.root == "" {
		return nil, errUsage
	}

	opts.staging, opts.destination = rest[0], rest[1]
	if rest[2] != "--" {
		return nil, errors.New("flux-op: expected -- before the command")
	}
	opts.command = rest[3:]

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

	// One pass over the result answers both questions. The ceiling is checked
	// against what actually landed rather than against what the input claimed
	// about itself, because those numbers are written by whoever built the
	// archive and a bomb simply lies.
	if opts.maxBytes > 0 || opts.noLinks {
		result, err := inspect(opts.staging)
		if err != nil {
			fmt.Fprintf(os.Stderr, "flux-op: could not inspect the result: %v\n", err)
			return 1
		}
		if opts.maxBytes > 0 && result.bytes > opts.maxBytes {
			fmt.Fprintf(os.Stderr, "flux-op: result is %d bytes, over the %d limit\n", result.bytes, opts.maxBytes)
			return exitTooLarge
		}
		if opts.noLinks && result.hasLinks {
			fmt.Fprintln(os.Stderr, "flux-op: result contains links, which are not accepted here")
			return exitHasLinks
		}
	}

	reclaimStaging = false

	if err := publish(opts.staging, opts.destination, opts.root, opts.id); err != nil {
		fmt.Fprintf(os.Stderr, "flux-op: could not publish the result: %v\n", err)
		return 1
	}

	return 0
}

// markerContents records where a displaced entry belongs, relative to the volume
// root.
//
// This file sits in a directory the app owner can write to, so its contents are
// input rather than state: an absolute path in here is a path a privileged
// reader might follow off the volume, and the shape that cannot be abused is the
// one that cannot be written down. Traversal still has to be refused by whoever
// reads it - ".." is expressible in a relative path too - but the class does not
// need checking for if it cannot be represented.
func markerContents(destination, root string) string {
	return strings.TrimPrefix(destination, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
