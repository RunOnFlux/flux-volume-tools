//go:build !linux

package main

// flux-op runs in a Linux container and nowhere else; this exists so the tool
// still builds and its tests still run on a developer's machine. The creation
// time is read through statx, which is Linux-only, so everywhere else records
// the weaker identity and says that it did.
func identity(path string) (string, error) {
	return modificationIdentity(path)
}
