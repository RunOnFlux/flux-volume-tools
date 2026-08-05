package main

import (
	"errors"
	"io"
	"os"
)

var errOverCeiling = errors.New("input is over the ceiling")

// receive writes the caller's stream into staging, refusing to write a byte past
// the ceiling.
//
// This is the one path where the ceiling can be exact. A command produces its
// bytes itself, so the only thing that can be checked is what it left behind -
// an extraction fills the volume and is then refused. Here this program is the
// writer, so the limit is enforced as the bytes arrive and nothing over it ever
// reaches the disk. That matters because the volume being filled is the one the
// application's own database is on.
//
// There is no child process, which removes the question a streamed upload
// otherwise raises: a command reading a truncated stream sees the same end-of-
// input as a complete one, so it exits successfully on half a file. Here the
// transfer ends when the caller closes the stream, and a caller that closes it
// only after sending everything cannot produce a short result that looks whole.
//
// O_EXCL because staging names a path this operation created for itself.
// Anything already there means the caller passed a path it does not own, and
// truncating it is the outcome this program exists to prevent.
//
// Like every other operation here this is atomic VISIBILITY, not durability:
// the destination never shows a half-written result, and a power cut seconds
// later can still lose unflushed contents, exactly as it can for cp.
func receive(src io.Reader, staging string, maxBytes int64) (int64, error) {
	file, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}

	written, copyErr := copyWithin(file, src, maxBytes)

	// Checked, never deferred and dropped: a write can fail at close rather than
	// at write - a full filesystem is the ordinary way that happens - and losing
	// that error publishes a truncated file as a complete one.
	closeErr := file.Close()

	if copyErr != nil {
		return written, copyErr
	}
	return written, closeErr
}

// copyWithin copies src into dst, stopping at maxBytes. A maxBytes of zero or
// less means no ceiling.
func copyWithin(dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return io.Copy(dst, src)
	}

	written, err := io.Copy(dst, io.LimitReader(src, maxBytes))
	if err != nil {
		return written, err
	}

	// The ceiling is a refusal, not a truncation, so the caller has to learn the
	// difference between a stream that ended and one that was cut off. Anything
	// still readable means the input was larger than it was allowed to be.
	var probe [1]byte
	switch _, probeErr := io.ReadFull(src, probe[:]); {
	case probeErr == nil:
		return written, errOverCeiling
	case errors.Is(probeErr, io.EOF):
		return written, nil
	default:
		return written, probeErr
	}
}
