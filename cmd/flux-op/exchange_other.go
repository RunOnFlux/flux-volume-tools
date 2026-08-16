//go:build !linux && !darwin

package main

import "fmt"

// exchange where the platform offers no atomic swap. flux-op runs on Linux and
// is developed on macOS, both of which have one; this is here so the package
// still builds anywhere, and refuses rather than quietly doing something that
// is not crash safe.
func exchange(a, b string) error {
	return fmt.Errorf("atomically exchanging %s and %s needs linux", a, b)
}

// renameNoReplace where the platform cannot refuse an occupied name as part of
// the rename. Refused for the same reason as above: doing it in two steps would
// be a rename that can destroy what is at the destination, on some platforms and
// not others, decided by something no caller can see.
func renameNoReplace(a, b string) error {
	return fmt.Errorf("moving %s to %s without replacing what is there needs linux", a, b)
}
