//go:build docker

package container

import (
	"strings"
	"testing"
)

// This program runs as root, so without --inherit-owner everything it creates
// is root's, and an application running as any other user cannot change its
// own uploaded or extracted files.

// stat reports a link itself, not its target.
func owner(t *testing.T, volume, name string) string {
	t.Helper()
	result := inContainer(t, volume, "", `stat -c '%u:%g' "$@"`, "/work/"+name)
	if result.exit != 0 {
		t.Fatalf("stat %s (exit %d):\n%s", name, result.exit, result.output)
	}
	return strings.TrimSpace(lastLine(result.output))
}

func TestAnUploadTakesTheOwnerOfItsFolder(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && chown 1000:1001 /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "a save",
		baseArgs("--discard-staging", "--from-stdin", "--inherit-owner", staging, "/work/app/save.db", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := owner(t, volume, "app/save.db"); got != "1000:1001" {
		t.Errorf("upload owned by %s, want the folder's 1000:1001", got)
	}
	requireNoArtefacts(t, volume)
}

func TestWithoutTheFlagAnUploadIsRoots(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && chown 1000:1001 /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "a save",
		baseArgs("--discard-staging", "--from-stdin", staging, "/work/app/save.db", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := owner(t, volume, "app/save.db"); got != "0:0" {
		t.Errorf("upload owned by %s without --inherit-owner, want 0:0", got)
	}
}

// An extraction merged over an existing folder re-owns what it brought and
// nothing that was already there.
func TestAMergedExtractionReownsOnlyWhatItBrought(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `
		mkdir -p /work/src/world/region /work/app/world &&
		echo new > /work/src/world/region/r.0 &&
		echo cfg > /work/src/world/level.dat &&
		ln -s level.dat /work/src/world/latest &&
		tar czf /work/in.tgz -C /work/src world &&
		rm -rf /work/src &&
		echo old > /work/app/world/keep.dat &&
		chown -R 1000:1000 /work/app &&
		chown 2000:2000 /work/app/world/keep.dat`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "", append(
		baseArgs("--discard-staging", "--mkdir", "--data-only", "--merge", "--inherit-owner",
			staging, "/work/app", "--"),
		"tar", "-xzf", "/work/in.tgz", "-C", staging, "--no-same-owner", "--no-same-permissions")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s%s", result.exit, result.output, tree(t, volume))
	}
	for _, name := range []string{"app/world/region", "app/world/region/r.0", "app/world/level.dat"} {
		if got := owner(t, volume, name); got != "1000:1000" {
			t.Errorf("%s owned by %s, want 1000:1000", name, got)
		}
	}
	if got := owner(t, volume, "app/world/latest"); got != "1000:1000" {
		t.Errorf("the link itself is owned by %s, want 1000:1000", got)
	}
	if got := owner(t, volume, "app/world/keep.dat"); got != "2000:2000" {
		t.Errorf("an existing file was re-owned to %s; the merge must leave it 2000:2000", got)
	}
	if got := owner(t, volume, "in.tgz"); got != "0:0" {
		t.Errorf("the archive, outside the result, was re-owned to %s", got)
	}
	requireNoArtefacts(t, volume)
}
