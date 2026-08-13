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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
func volumeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "flux-op-volume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("docker", "run", "--rm", "--volume", dir+":/work", image(),
			"sh", "-c", "rm -rf /work/..?* /work/.[!.]* /work/* 2>/dev/null; true").Run()
		os.RemoveAll(dir)
	})
	return dir
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
	code, err := strconv.Atoi(strings.TrimSpace(result.output[index+len(marker):]))
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
	data, err := os.ReadFile(filepath.Join(volume, name))
	if err != nil {
		t.Fatalf("reading %s: %v\n%s", name, err, tree(volume))
	}
	return strings.TrimRight(string(data), "\n")
}

// What a sweep compares, derived independently of the code under test so the
// format is pinned rather than echoed back.
//
// Read from inside a container, because an inode number belongs to the
// filesystem that issued it and a bind mount does not always carry it across
// unchanged: Docker Desktop synthesises its own on macOS, where a node's Linux
// bind mount hands back the same number the host sees. flux-op recorded what it
// saw from in there, so the comparison is made from the same side.
func identityOf(t *testing.T, volume, name string) string {
	t.Helper()
	result := inContainer(t, volume, "", `stat -c '%i %.9Y' "$@"`, "/work/"+name)
	if result.exit != 0 {
		t.Fatalf("could not stat %s (exit %d):\n%s", name, result.exit, result.output)
	}
	fields := strings.Fields(strings.TrimSpace(result.output))
	if len(fields) != 2 {
		t.Fatalf("stat of %s returned %q", name, result.output)
	}
	// Seconds and nanoseconds arrive as one decimal number, padded to nine
	// places, so removing the point is the whole conversion.
	return fields[0] + " " + strings.Replace(fields[1], ".", "", 1)
}

func exists(volume, name string) bool {
	_, err := os.Lstat(filepath.Join(volume, name))
	return err == nil
}

// tree is what a failure prints. The suite this replaced discarded the
// container's output entirely, so a failure named the scenario and nothing else.
func tree(volume string) string {
	var out strings.Builder
	out.WriteString("volume holds:\n")
	filepath.Walk(volume, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		relative, _ := filepath.Rel(volume, path)
		if relative == "." {
			return nil
		}
		fmt.Fprintf(&out, "  %s %s\n", info.Mode(), relative)
		return nil
	})
	return out.String()
}

// artefacts are the entries an interrupted operation leaves at the volume root.
// A completed one leaves none.
func artefacts(t *testing.T, volume string) []string {
	t.Helper()
	entries, err := os.ReadDir(volume)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".flux-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func requireNoArtefacts(t *testing.T, volume string) {
	t.Helper()
	if left := artefacts(t, volume); len(left) != 0 {
		t.Errorf("left behind %v\n%s", left, tree(volume))
	}
}

func baseArgs(extra ...string) []string {
	return append([]string{"--id", operationID, "--root", "/work"}, extra...)
}
