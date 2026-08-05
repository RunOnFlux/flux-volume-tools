package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// runChild runs the caller's command and reports how it ended.
//
// The command's standard input is this program's own, stated explicitly. That
// is not incidental: an upload streamed into the container arrives on it, and
// the shell implementation this replaced gave the command /dev/null instead,
// because POSIX assigns that to an asynchronous list when job control is off.
// Nothing the caller sent could be read, and nothing reported an error.
//
// A container stop delivers only to PID 1, so the signal has to be forwarded or
// the command keeps writing into a staging directory nobody will publish. The
// signal is forwarded and then waited out rather than acted on immediately: the
// command decides how long it needs to stop, and the staging directory it was
// writing into is only safe to reclaim once it has.
//
// A SIGKILL bypasses all of this. Nothing can be done from in here about that,
// and the startup sweep remains the backstop for it.
//
// The streams are passed in rather than read from the process, so that the
// descriptor the command receives is something a test can assert on. They are
// *os.File rather than io.Reader/io.Writer deliberately: exec hands a *os.File
// to the child as-is, where any other reader or writer becomes a pipe with a
// copying goroutine behind it. The whole point here is which descriptor the
// command actually gets.
//
// Returns the status to exit with, whether a signal interrupted the run, and an
// error only if the command could not be started at all.
func runChild(command []string, stdin, stdout, stderr *os.File) (status int, canceled bool, err error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Installed before the command starts, so a signal arriving in the gap is
	// delivered here rather than ending this process with the command orphaned.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		return 0, false, err
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	for {
		select {
		case received := <-signals:
			canceled = true
			// Forwarded rather than translated: a command that distinguishes
			// INT from TERM gets to keep doing so. Failure means it has already
			// gone, which the wait below is about to tell us anyway.
			_ = cmd.Process.Signal(received)
		case waitErr := <-waited:
			return exitStatus(cmd, waitErr), canceled, nil
		}
	}
}

// exitStatus is what a shell would have reported for this command: its own exit
// code, or 128 plus the signal that killed it.
func exitStatus(cmd *exec.Cmd, waitErr error) int {
	if waitErr == nil {
		return 0
	}

	state := cmd.ProcessState
	if state == nil {
		return 1
	}
	if wait, ok := state.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		return 128 + int(wait.Signal())
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
