//go:build docker

package container

import "testing"

// --parents makes the folders missing above the destination, each owned by the
// folder that receives it, so an upload into a folder that is not there yet
// arrives owned by the application rather than by root.

func TestAnUploadIntoMissingFoldersIsTheApplicationsThroughout(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && chown 1000:1001 /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "a config",
		baseArgs("--discard-staging", "--from-stdin", "--parents", "--inherit-owner",
			staging, "/work/app/config/neoforge/deep/fml.toml", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s%s", result.exit, result.output, tree(t, volume))
	}
	for _, name := range []string{"app/config", "app/config/neoforge", "app/config/neoforge/deep", "app/config/neoforge/deep/fml.toml"} {
		if got := owner(t, volume, name); got != "1000:1001" {
			t.Errorf("%s owned by %s, want the folder's 1000:1001", name, got)
		}
	}
	if got := contents(t, volume, "app/config/neoforge/deep/fml.toml"); got != "a config" {
		t.Errorf("the upload holds %q", got)
	}
	requireNoArtefacts(t, volume)
}

// Each level takes the owner of the folder it is made in, not of the volume or
// of the first folder found.
func TestEachFolderTakesTheOwnerOfTheFolderThatReceivesIt(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/app/shared && chown 1000:1000 /work/app && chown 2000:2002 /work/app/shared`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "x",
		baseArgs("--discard-staging", "--from-stdin", "--parents", "--inherit-owner",
			staging, "/work/app/shared/b/c/file", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s%s", result.exit, result.output, tree(t, volume))
	}
	for _, name := range []string{"app/shared/b", "app/shared/b/c", "app/shared/b/c/file"} {
		if got := owner(t, volume, name); got != "2000:2002" {
			t.Errorf("%s owned by %s, want the receiving folder's 2000:2002", name, got)
		}
	}
	if got := owner(t, volume, "app"); got != "1000:1000" {
		t.Errorf("an existing folder was re-owned to %s", got)
	}
}

// The folders are made for the application whatever the result's own owner is:
// a copy keeps its files' owners, and still lands in folders the application
// can write into.
func TestFoldersAreTheApplicationsWithoutInheritOwner(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && chown 1000:1001 /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "x",
		baseArgs("--discard-staging", "--from-stdin", "--parents", staging, "/work/app/a/b/file", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s%s", result.exit, result.output, tree(t, volume))
	}
	for _, name := range []string{"app/a", "app/a/b"} {
		if got := owner(t, volume, name); got != "1000:1001" {
			t.Errorf("%s owned by %s, want 1000:1001", name, got)
		}
	}
	if got := owner(t, volume, "app/a/b/file"); got != "0:0" {
		t.Errorf("the result is owned by %s without --inherit-owner, want 0:0", got)
	}
}

func TestCreatingAFolderWithItsParentsUnderTheExecutor(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && chown 1000:1001 /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "",
		baseArgs("--discard-staging", "--mkdir", "--no-replace", "--parents", "--inherit-owner",
			staging, "/work/app/x/y", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s%s", result.exit, result.output, tree(t, volume))
	}
	for _, name := range []string{"app/x", "app/x/y"} {
		if got := owner(t, volume, name); got != "1000:1001" {
			t.Errorf("%s owned by %s, want 1000:1001", name, got)
		}
	}
	requireNoArtefacts(t, volume)
}

func TestWithoutParentsAMissingFolderIsRefusedUnderTheExecutor(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "x",
		baseArgs("--discard-staging", "--from-stdin", "--inherit-owner", staging, "/work/app/a/file", "--")...)

	if result.exit != 1 {
		t.Fatalf("exit %d, want 1:\n%s", result.exit, result.output)
	}
	if exists(t, volume, "app/a") {
		t.Errorf("a folder was made without --parents\n%s", tree(t, volume))
	}
	requireNoArtefacts(t, volume)
}

func TestAFileWhereAFolderIsNeededIsRefusedUnderTheExecutor(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && echo precious > /work/app/a`)
	staging := "/work/.flux-op-" + operationID

	result := fluxOp(t, volume, "x",
		baseArgs("--discard-staging", "--from-stdin", "--parents", "--inherit-owner",
			staging, "/work/app/a/b/file", "--")...)

	if result.exit != 1 {
		t.Fatalf("exit %d, want 1:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "app/a"); got != "precious" {
		t.Errorf("the file holds %q", got)
	}
	requireNoArtefacts(t, volume)
}

// A link the application planted is followed, and the only place it can lead
// is the application's own volume: everything else in the container is
// read-only.
func TestALinkOffTheVolumeLeadsNowhereWritable(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir /work/app && ln -s /etc /work/app/out && ln -s /tmp /work/app/tmp`)
	staging := "/work/.flux-op-" + operationID

	for _, link := range []string{"out", "tmp"} {
		result := fluxOp(t, volume, "x",
			baseArgs("--discard-staging", "--from-stdin", "--parents", "--inherit-owner",
				staging, "/work/app/"+link+"/new/file", "--")...)
		if result.exit == 0 {
			t.Errorf("published through a link to %s:\n%s", link, result.output)
		}
	}
	requireNoArtefacts(t, volume)
}
