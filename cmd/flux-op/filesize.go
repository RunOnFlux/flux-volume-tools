package main

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// limitFileSize caps how large a file this process, and every process it starts
// until restore is called, may write. A write that would pass the cap fails in
// the kernel - the writer is sent SIGXFSZ, or given EFBIG if it ignores that -
// so the file stops at exactly maxBytes, whatever the program writing it does.
//
// This is what makes --max-bytes hold while a command runs rather than only
// once it has finished. The ceiling is the volume's free space, and an archiver
// that is only checked afterwards fills the volume first, leaving the running
// application with nothing to write into until the refusal gives the space
// back. An archiver writes one file, so capping each file caps what it can
// take.
//
// Only the soft limit is lowered. The hard limit is left where it was so that
// restore can raise the soft limit back: lowering the hard limit is permanent
// for a process without CAP_SYS_RESOURCE, which the executor's container drops.
// A command could raise its own soft limit back to the hard one; the commands
// run here are the archivers and coreutils the caller names, and none does.
func limitFileSize(maxBytes int64) (restore func(), err error) {
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		return nil, err
	}

	limited := previous
	if ceiling := uint64(maxBytes); ceiling < limited.Cur {
		limited.Cur = ceiling
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		return nil, err
	}

	return func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous) }, nil
}

// stoppedByTheCap reports whether a command that exited with status was
// stopped by limitFileSize rather than failing for a reason of its own. The
// archivers report the cap in their own words and statuses, none of which says
// "too large", so it is recognised in the two shapes they give it:
//
//   - the command was killed by SIGXFSZ, the signal the cap sends. zip is, and
//     it writes into a temporary file beside its output rather than into the
//     output, so the result is not where the cap was reached.
//   - a file in the result reached maxBytes. tar exits 2 when the compressor it
//     runs is killed, and the compressor writes the result itself.
//
// Files are measured by LENGTH, which is what the cap limits, and not by what
// they occupy: a file of ten bytes occupies a whole block, which would read as
// a small ceiling reached by a command that wrote almost nothing. A file at
// exactly maxBytes is one the cap stopped, since the cap refuses the write that
// would have gone past it. Nothing is followed, as in inspect.
func stoppedByTheCap(status int, staging string, maxBytes int64) bool {
	if status == 128+int(syscall.SIGXFSZ) {
		return true
	}
	reached := false
	_ = filepath.WalkDir(staging, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Size() >= maxBytes {
			reached = true
			return filepath.SkipAll
		}
		return nil
	})
	return reached
}
