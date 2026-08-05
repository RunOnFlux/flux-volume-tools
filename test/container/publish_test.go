//go:build docker

package container

import (
	"testing"
)

// The contract: the destination changes only on success, and it changes
// atomically whatever the two entry types are.
//
// rename(2) cannot replace a file with a directory (or the reverse) at all, and
// refuses a non-empty directory target - which is why publishing goes through a
// swap rather than a delete-then-rename, and why every combination is covered
// rather than just the common one.
func TestPublishReplacesEveryCombinationOfTypes(t *testing.T) {
	cases := []struct {
		name        string
		seed        string
		wantAtDest  string
		wantMissing string
	}{
		{
			name:       "a directory replaces an existing file",
			seed:       `mkdir -p /work/src && echo new > /work/src/f && echo original > /work/dest`,
			wantAtDest: "dest/f",
		},
		{
			name:        "a directory replaces an existing directory, without merging into it",
			seed:        `mkdir -p /work/src /work/dest && echo new > /work/src/f && echo old > /work/dest/keep`,
			wantAtDest:  "dest/f",
			wantMissing: "dest/keep",
		},
		{
			name:       "a file replaces an existing directory",
			seed:       `mkdir -p /work/dest && echo new > /work/src && echo old > /work/dest/keep`,
			wantAtDest: "dest",
		},
		{
			name:       "a new destination is one rename",
			seed:       `mkdir -p /work/src && echo new > /work/src/f`,
			wantAtDest: "dest/f",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			volume := volumeDir(t)
			seed(t, volume, testCase.seed)

			staging := "/work/.flux-op-" + operationID
			result := fluxOp(t, volume, "",
				append(baseArgs("--discard-staging", staging, "/work/dest", "--"),
					"cp", "-a", "-T", "/work/src", staging)...)

			if result.exit != 0 {
				t.Fatalf("exit %d:\n%s", result.exit, result.output)
			}
			if !exists(volume, testCase.wantAtDest) {
				t.Errorf("%s is not there\n%s", testCase.wantAtDest, tree(volume))
			}
			if testCase.wantMissing != "" && exists(volume, testCase.wantMissing) {
				t.Errorf("%s survived - the destination was merged into, not replaced\n%s",
					testCase.wantMissing, tree(volume))
			}
			requireNoArtefacts(t, volume)
		})
	}
}

func TestAFailedCommandLeavesTheDestinationAndReclaimsStaging(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `echo original > /work/dest`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "", append(baseArgs("--discard-staging", staging, "/work/dest", "--"), "false")...)

	if result.exit == 0 {
		t.Fatalf("a failing command reported success:\n%s", result.output)
	}
	if got := contents(t, volume, "dest"); got != "original" {
		t.Errorf("destination holds %q, want original", got)
	}
	requireNoArtefacts(t, volume)
}

// Checked against what actually landed rather than against what an archive
// claims about itself: those numbers are written by whoever built it, so a bomb
// simply lies.
func TestAResultOverTheCeilingIsRefusedAndReclaimed(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && head -c 2000 /dev/zero > /work/src/big`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--max-bytes", "1000", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 3 {
		t.Fatalf("exit %d, want 3:\n%s", result.exit, result.output)
	}
	if exists(volume, "dest") {
		t.Error("the destination was published despite the refusal")
	}
	requireNoArtefacts(t, volume)
}

// An archive that carries a symlink and then writes through it reaches wherever
// the link points. Inside this container that is nowhere useful, but the result
// is published onto a volume that other code paths - and other nodes, through
// sync - do read.
func TestAResultContainingALinkIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && ln -s /etc/shadow /work/src/link`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--no-links", staging, "/work/dest", "--"),
			"cp", "-a", "-T", "/work/src", staging)...)

	if result.exit != 4 {
		t.Fatalf("exit %d, want 4:\n%s", result.exit, result.output)
	}
	if exists(volume, "dest") {
		t.Error("the destination was published despite the refusal")
	}
	requireNoArtefacts(t, volume)
}

// A move and a rename have NO command: the caller's source already IS the
// result, so publishing it is the whole operation. A usage check that demanded a
// command rejected every move, and nothing noticed for a whole branch.
func TestAMovePublishesWithNoCommandAtAll(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo hi > /work/photos/f`)

	result := fluxOp(t, volume, "", baseArgs("/work/photos", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if !exists(volume, "out/f") {
		t.Errorf("the move did not publish\n%s", tree(volume))
	}
	if exists(volume, "photos") {
		t.Error("the source survived the move")
	}
	requireNoArtefacts(t, volume)
}

func TestAMoveOverAnExistingDestinationSwapsItAsideAndCleansUp(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo new > /work/photos/f && echo old > /work/out`)

	result := fluxOp(t, volume, "", baseArgs("/work/photos", "/work/out", "--")...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out/f"); got != "new" {
		t.Errorf("destination holds %q, want new", got)
	}
	requireNoArtefacts(t, volume)
}

// Staging is only ever discarded when the caller says it owns it. A move's
// operand is the user's own data, and discarding it on a failure would destroy
// the only copy.
func TestAFailureNeverDiscardsAnOperandTheCallerOwns(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/photos && echo precious > /work/photos/f`)

	result := fluxOp(t, volume, "",
		baseArgs("--max-bytes", "1", "/work/photos", "/work/dest", "--")...)

	if result.exit != 3 {
		t.Fatalf("exit %d, want 3:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "photos/f"); got != "precious" {
		t.Errorf("the caller's own data holds %q, want precious", got)
	}
}

// The state the marker exists for: the caller's previous data has been moved
// aside and the replacement never arrived. Reproduced by publishing a staging
// path that does not exist, so the second rename fails exactly where a crash
// would land.
func TestAnInterruptedPublishLeavesTheDataAndAMarkerThatPlacesIt(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/a/b /work/x/y && echo precious > /work/x/y/out`)

	result := fluxOp(t, volume, "", baseArgs("/work/a/b/photos", "/work/x/y/out", "--")...)

	if result.exit == 0 {
		t.Fatalf("publishing a staging path that does not exist succeeded:\n%s", result.output)
	}

	displaced := ".flux-old-" + operationID
	if got := contents(t, volume, displaced); got != "precious" {
		t.Errorf("displaced data holds %q, want precious\n%s", got, tree(volume))
	}

	// Relative, so nothing that reads it can be sent off the volume by following
	// an absolute path. At the volume root, which is the one directory the sweep
	// reads - not beside the destination, wherever the caller kept it.
	if got := contents(t, volume, displaced+".dest"); got != "x/y/out" {
		t.Errorf("marker holds %q, want x/y/out", got)
	}
	if exists(volume, "x/y/"+displaced) {
		t.Error("the artefacts were left beside the destination rather than at the volume root")
	}
	if exists(volume, "x/y/out") {
		t.Error("the destination is still there, so this is not the interrupted state")
	}
}

// tar -C and unzip -d both need the directory to exist already. A file copy must
// NOT ask for it: cp -T refuses to overwrite a directory with a non-directory.
func TestStagingIsCreatedForCommandsThatNeedIt(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && echo x > /work/src/f && tar -cf /work/a.tar -C /work src`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", staging, "/work/out", "--"),
			"tar", "-xf", "/work/a.tar", "-C", staging)...)

	if result.exit != 0 {
		t.Fatalf("exit %d:\n%s", result.exit, result.output)
	}
	if got := contents(t, volume, "out/src/f"); got != "x" {
		t.Errorf("extracted content is %q", got)
	}
}

func TestTheIdentifierAndVolumeRootAreRequired(t *testing.T) {
	volume := volumeDir(t)

	cases := [][]string{
		{"/work/.flux-op-1", "/work/dest", "--", "true"},
		{"--id", operationID, "/work/.flux-op-1", "/work/dest", "--", "true"},
		{"--root", "/work", "/work/.flux-op-1", "/work/dest", "--", "true"},
	}

	for _, argv := range cases {
		result := fluxOp(t, volume, "", argv...)
		if result.exit != 2 {
			t.Errorf("exit %d for %v, want 2:\n%s", result.exit, argv, result.output)
		}
		if exists(volume, "dest") {
			t.Error("a refused invocation touched the destination")
		}
	}
}
