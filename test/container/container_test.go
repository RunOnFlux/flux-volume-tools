//go:build docker

// What the image does, exercised through a container configured exactly as the
// FluxOS volume executor configures it.
//
// The configuration is not incidental to these tests - it is most of what makes
// the design safe, and until now nothing verified flux-op works under it. A
// read-only rootfs, no network, all capabilities dropped but three, and a
// volume that is the only thing mounted: an operation that quietly depends on
// anything else would pass a plain `docker run` and fail on a node.
//
// Run with:
//
//	go test -tags docker -count=1 ./test/container/ -v
//	FLUX_VOLUME_TOOLS_IMAGE=... go test -tags docker -count=1 ./test/container/
//
// -count=1 is not optional. The image under test is an input the test cache
// cannot see, so rebuilding it - for another architecture, say - and running
// again reports the previous result without starting a container.
//
// The FluxOS side of the contract - the attach, the hijacked socket, racing the
// stream against the container's exit - is not testable from here, because it
// is about how a caller drives the docker API rather than about the image. It
// lives in the FluxOS unit tests and the integration harness.
package container

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The identifier FluxOS passes. Its exact shape is what lets the boot sweep tell
// this image's artefacts from a folder a user named.
const operationID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func image() string {
	if named := os.Getenv("FLUX_VOLUME_TOOLS_IMAGE"); named != "" {
		return named
	}
	return "flux-volume-tools:test"
}

// The container configuration volumeExecutor applies, expressed as the CLI
// equivalents of its HostConfig. Containment comes from the container having
// nowhere to escape TO rather than from a sequence of path checks being right,
// so these belong in front of every scenario rather than in one test of their
// own.
func executorConfig(volume string) []string {
	return []string{
		"--rm", "--interactive",
		"--volume", volume + ":/work",
		"--workdir", "/work",
		"--read-only",
		"--network", "none",
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "FOWNER",
		"--cap-add", "DAC_OVERRIDE",
		"--security-opt", "no-new-privileges",
		"--memory", "512m",
		"--pids-limit", "256",
	}
}

type outcome struct {
	exit   int
	output string
}

// volumeDir gives the test a directory bound in as the app volume, and removes
// it through a container afterwards - what the container writes is owned by
// root, and the test process cannot delete a root-owned tree.
// volumeDir is a docker NAMED VOLUME, not a host directory bind-mounted in.
//
// This suite exists to say what the image does on the filesystem a node puts an
// app volume on, and a bind mount from a macOS host is not one: Docker Desktop
// serves it through a shim which reports its type as "fakeowner", drops setuid
// bits, and - the reason this changed - refuses to exchange two entries of
// different types atomically, answering with the error a plain rename gives. A
// named volume is real Linux ext4 wherever docker runs.
//
// CI would never have caught the difference. A bind mount on a Linux runner IS
// the runner's ext4, so the suite passed there and failed only on the machine
// the code is written on - which is the worst way round, because that is where
// it is run before pushing.
//
// The cost is that nothing here can read the volume from the host, so the
// helpers below ask the image instead. That is the more honest question anyway:
// they now see what the running application sees.
func volumeDir(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("flux-op-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	if out, err := exec.Command("docker", "volume", "create", name).CombinedOutput(); err != nil {
		t.Fatalf("could not create the test volume: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "volume", "rm", "-f", name).Run()
	})
	return name
}

// inContainer runs a shell script in the image, with args available to it as
// "$@" so nothing has to be quoted into the script itself.
func inContainer(t *testing.T, volume, stdin, script string, args ...string) outcome {
	t.Helper()

	argv := append(executorConfig(volume), image(), "sh", "-c", script, "flux-op")
	argv = append(argv, args...)

	cmd := exec.Command("docker", append([]string{"run"}, argv...)...)
	cmd.Stdin = strings.NewReader(stdin)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	result := outcome{output: combined.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("could not run the container: %v\n%s", err, result.output)
		}
		result.exit = exitErr.ExitCode()
	}
	return result
}

// fluxOp runs one flux-op invocation and reports its own exit code, separately
// from whatever the assertions afterwards decide.
func fluxOp(t *testing.T, volume, stdin string, args ...string) outcome {
	t.Helper()
	const script = `flux-op "$@"; printf 'FLUXOP_EXIT=%s\n' "$?"`
	result := inContainer(t, volume, stdin, script, args...)

	marker := "FLUXOP_EXIT="
	index := strings.LastIndex(result.output, marker)
	if index < 0 {
		t.Fatalf("flux-op never reported an exit code:\n%s", result.output)
	}
	// The first line after the marker, not everything after it. stdout and
	// stderr arrive on one stream here, and which lands last is a matter of
	// buffering rather than of order - so a run that writes to stderr can put a
	// diagnostic AFTER the marker, and reading to the end then parses the
	// diagnostic as part of the number. That failed on one architecture and
	// passed on the other in the same run.
	tail := result.output[index+len(marker):]
	if newline := strings.IndexByte(tail, '\n'); newline >= 0 {
		tail = tail[:newline]
	}
	code, err := strconv.Atoi(strings.TrimSpace(tail))
	if err != nil {
		t.Fatalf("could not read the exit code:\n%s", result.output)
	}
	result.exit = code
	return result
}

// seed prepares the volume through a container, so what it creates is owned the
// way the running application's own files are.
func seed(t *testing.T, volume, script string) {
	t.Helper()
	if result := inContainer(t, volume, "", script); result.exit != 0 {
		t.Fatalf("could not prepare the volume (exit %d):\n%s", result.exit, result.output)
	}
}

func contents(t *testing.T, volume, name string) string {
	t.Helper()
	result := inContainer(t, volume, "", `cat "$@"`, "/work/"+name)
	if result.exit != 0 {
		t.Fatalf("reading %s (exit %d):\n%s%s", name, result.exit, result.output, tree(t, volume))
	}
	return strings.TrimRight(lastLine(result.output), "\n")
}

// The last line of a container's output. Running a foreign architecture under
// emulation puts a platform warning ahead of it, which is the same noise the
// exit code is read past.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return lines[len(lines)-1]
}

// Seconds and nanoseconds arrive from stat as one decimal number, padded to nine
// places, so removing the point is the whole conversion.
func nanoseconds(stamp string) string {
	return strings.Replace(stamp, ".", "", 1)
}

func exists(t *testing.T, volume, name string) bool {
	t.Helper()
	result := inContainer(t, volume, "", `test -e "$@"; echo $?`, "/work/"+name)
	return strings.TrimSpace(lastLine(result.output)) == "0"
}

// tree is what a failure prints. The suite this replaced discarded the
// container's output entirely, so a failure named the scenario and nothing else.
func tree(t *testing.T, volume string) string {
	t.Helper()
	result := inContainer(t, volume, "", `find /work -mindepth 1 -printf '  %M %P\n' 2>/dev/null | sort`)
	return "volume holds:\n" + result.output
}

// artefacts are the entries an interrupted operation leaves at the volume root.
// A completed one leaves none.
func artefacts(t *testing.T, volume string) []string {
	t.Helper()
	result := inContainer(t, volume, "", `ls -A /work | grep '^\.flux-' || true`)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(result.output), "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, ".flux-") {
			names = append(names, name)
		}
	}
	return names
}

func requireNoArtefacts(t *testing.T, volume string) {
	t.Helper()
	if left := artefacts(t, volume); len(left) != 0 {
		t.Errorf("left behind %v\n%s", left, tree(t, volume))
	}
}

func baseArgs(extra ...string) []string {
	return append([]string{"--id", operationID, "--root", "/work"}, extra...)
}
