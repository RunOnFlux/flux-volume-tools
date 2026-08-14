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
