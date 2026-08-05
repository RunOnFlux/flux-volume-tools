//go:build docker

package container

import (
	"strings"
	"testing"
)

// Each of these is the implementation the executor relies on. A change in
// Alpine's packaging fails here rather than shipping a busybox applet that is
// missing a flag the operations depend on.
func TestToolchainIsTheExpectedImplementation(t *testing.T) {
	volume := volumeDir(t)

	cases := []struct {
		name    string
		command string
		want    string
		why     string
	}{
		{
			name:    "unzip is Info-ZIP",
			command: "unzip -v",
			want:    "Info-ZIP",
			why:     "busybox's applet has no zip64 support, capping archives at 4 GB",
		},
		{
			name:    "tar supports --no-same-owner",
			command: "tar --help 2>&1",
			want:    "no-same-owner",
			why:     "without it an archive's recorded uids land verbatim on the volume",
		},
		{
			name:    "cp supports -T",
			command: "cp --help 2>&1",
			want:    "-T",
			why:     "cp -T is what stops a copy landing INSIDE the staging directory",
		},
		{
			name:    "zip is present",
			command: "command -v zip",
			want:    "zip",
			why:     "busybox has no zip applet at all, and creating archives is half of what this image is for",
		},
		{
			name:    "gzip is present",
			command: "command -v gzip",
			want:    "gzip",
			why:     "tar -czf needs it",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := inContainer(t, volume, "", testCase.command)
			if !strings.Contains(result.output, testCase.want) {
				t.Errorf("no %q in the output - %s\n%s", testCase.want, testCase.why, result.output)
			}
		})
	}
}

// -T has to actually refuse to recurse into an existing destination, not merely
// be accepted as a flag.
func TestCopyDoesNotNestIntoAnExistingDestination(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src /work/dst && echo hi > /work/src/f`)

	result := inContainer(t, volume, "", `cp -a -T /work/src /work/dst`)
	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}

	if !exists(volume, "dst/f") {
		t.Error("the copy did not land")
	}
	if exists(volume, "dst/src") {
		t.Error("the copy nested inside the destination instead of becoming it")
	}
}

// The constraints every other test here runs under are worth asserting once,
// because the value of the rest depends on them being real: flux-op needs no
// writable space of its own beyond the volume, and it cannot reach anything.
//
// Reachability is asserted as the absence of a routable address rather than by
// counting interfaces - a container with no network still gets tunl0 and
// ip6tnl0, which the kernel creates in every fresh network namespace and which
// carry no address and no route.
func TestFluxOpRunsUnderTheExecutorConfiguration(t *testing.T) {
	volume := volumeDir(t)

	result := inContainer(t, volume, "", `
		flux-op 2>&1 | head -1
		echo "writable-rootfs=$(touch /probe 2>/dev/null && echo yes || echo no)"
		echo "routable-addresses=$(ip -o addr show scope global 2>/dev/null | wc -l)"
		echo "routes=$(ip route 2>/dev/null | wc -l)"
		echo "writable-volume=$(touch /work/probe 2>/dev/null && echo yes || echo no)"
	`)

	for _, want := range []string{
		"flux-op: usage",
		"writable-rootfs=no",
		"routable-addresses=0",
		"routes=0",
		// The one thing an operation may write, and the only thing mounted.
		"writable-volume=yes",
	} {
		if !strings.Contains(result.output, want) {
			t.Errorf("expected %q under the executor's configuration:\n%s", want, result.output)
		}
	}
}
