//go:build docker

package container

import (
	"strconv"
	"strings"
	"testing"
)

// What a hostile archive can reach.
//
// An extraction's result is content this node did not write and cannot vouch
// for: an app owner uploads an archive and asks for it to be unpacked. What
// bounds it is this container - one application's volume at /work, a read-only
// rootfs, no network, every capability dropped but the three a copy needs -
// rather than an inspection of the archive's members deciding which are safe.
//
// Two layers stand between an archive and the node, and they are tested apart
// because only one of them is ours. GNU tar refuses a member whose name climbs
// out and refuses to write through a symlink it restored; unzip does neither.
// The container is what holds when the extractor does nothing, so it is asserted
// directly rather than inferred from a hostile archive happening to be stopped.
//
// A node's app volume IS a host directory, bind-mounted here. So "did it escape"
// is one precise question - is anything host-backed writable other than /work -
// and TestOnlyTheAppVolumeIsWritable answers it over the whole mount table
// rather than over a list of paths somebody thought of.

// Filesystems that live and die with the container. A write to one of these
// reaches nothing: it is kernel state or memory, discarded when the container
// is, and none of them is backed by the node's disk.
var ephemeralFilesystems = map[string]bool{
	"tmpfs": true, "proc": true, "sysfs": true, "devpts": true,
	"mqueue": true, "cgroup": true, "cgroup2": true, "devtmpfs": true,
}

// The claim the rest of this file rests on, read off the mount table rather than
// sampled. Every filesystem the node's disk is behind is read-only here except
// the one volume this operation is for.
func TestOnlyTheAppVolumeIsWritable(t *testing.T) {
	volume := volumeDir(t)

	// Bracketed, because docker writes to this stream too: on a runner whose
	// architecture differs from the image's it prints a platform warning, and a
	// sentence has enough words in it to be read as a mount line by anything
	// splitting on whitespace. It was, and the test failed naming "requested" as
	// a filesystem.
	mounts := inContainer(t, volume, "", `echo MOUNTS-BEGIN; cat /proc/mounts; echo MOUNTS-END`)
	if mounts.exit != 0 {
		t.Fatalf("could not read the mount table:\n%s", mounts.output)
	}
	_, after, found := strings.Cut(mounts.output, "MOUNTS-BEGIN\n")
	table, _, closed := strings.Cut(after, "MOUNTS-END")
	if !found || !closed {
		t.Fatalf("the mount table did not arrive whole:\n%s", mounts.output)
	}

	writable := []string{}
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		// Six fields exactly, and a mount point that is an absolute path. A line
		// of prose satisfies neither.
		if len(fields) != 6 || !strings.HasPrefix(fields[1], "/") {
			continue
		}
		point, fstype, options := fields[1], fields[2], fields[3]
		if ephemeralFilesystems[fstype] {
			continue
		}
		readOnly := options == "ro" || strings.HasPrefix(options, "ro,")
		if !readOnly {
			writable = append(writable, point+" ("+fstype+", "+options+")")
		}
	}

	if len(writable) != 1 || !strings.HasPrefix(writable[0], "/work ") {
		t.Errorf("writable host-backed mounts are %v, want /work alone\n%s", writable, mounts.output)
	}

	// And it really is writable, or the line above passes on a container where
	// nothing works at all.
	if result := inContainer(t, volume, "", `echo ok > /work/probe && cat /work/probe`); !strings.Contains(result.output, "ok") {
		t.Fatalf("the app volume is not writable, so this suite is testing nothing:\n%s", result.output)
	}
}

// The case with no extractor protection in the way, which is why it is the one
// that matters. unzip restores a symlink pointing at /etc without complaint, and
// what stops the write that follows is the filesystem it points at.
func TestAWriteThroughARestoredLinkIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build && cd /work/build && ln -s /etc escape && zip -q -y /work/hostile.zip escape`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"sh", "-c", "unzip -q /work/hostile.zip -d "+staging+" && echo pwned > "+staging+"/escape/pwned")...)

	// The link is restored - that part is not defended anywhere - and the write
	// through it is refused by the filesystem it lands on.
	if !strings.Contains(result.output, "Read-only file system") {
		t.Errorf("the write through the link was not refused by the filesystem:\n%s", result.output)
	}
	if result.exit == 0 {
		t.Errorf("the operation reported success although its write could not have happened\n%s", result.output)
	}
	if exists(t, volume, "out") {
		t.Error("a destination was published by an operation that failed")
	}
	requireNoArtefacts(t, volume)
}

// A member whose name climbs out of the extraction directory - the oldest attack
// there is. GNU tar refuses this one itself; the assertion is that nothing lands
// anywhere but where it was asked to.
func TestATarMemberThatClimbsOutOfTheTree(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build/a/b && echo pwned > /work/build/a/b/payload && cd /work/build &&
		tar -cf /work/hostile.tar --transform 's|^a/b/payload|../../../../work/pwned|' a/b/payload`)

	// The archive really does carry the climbing name. Without this the case
	// passes having attacked nothing.
	if listed := inContainer(t, volume, "", `tar -tf /work/hostile.tar`); !strings.Contains(listed.output, "../../../../work/pwned") {
		t.Fatalf("the hostile archive does not contain a climbing member:\n%s", listed.output)
	}

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"tar", "-xf", "/work/hostile.tar", "-C", staging)...)

	// Aimed at the volume rather than at the rootfs on purpose: the volume is the
	// one writable filesystem, so this is the version of the attack that could
	// have worked. Nothing may appear outside the destination it was given.
	if exists(t, volume, "pwned") {
		t.Errorf("a tar member landed at the volume root\n%s", result.output)
	}
	requireNoArtefacts(t, volume)
}

// The archive plants a link and writes a member through it.
func TestAnArchiveThatWritesThroughItsOwnSymlink(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build && cd /work/build && ln -s /work escape && echo pwned > payload &&
		tar -cf /work/hostile.tar escape &&
		tar -rf /work/hostile.tar --transform 's|^payload|escape/pwned|' payload`)

	if listed := inContainer(t, volume, "", `tar -tf /work/hostile.tar`); !strings.Contains(listed.output, "escape/pwned") {
		t.Fatalf("the hostile archive does not write through its link:\n%s", listed.output)
	}

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"tar", "-xf", "/work/hostile.tar", "-C", staging)...)

	if exists(t, volume, "pwned") {
		t.Errorf("an archive wrote through its own link and landed at the volume root\n%s", result.output)
	}
	requireNoArtefacts(t, volume)
}

// A device node would be a way to reach the disk under the filesystem. The
// container drops CAP_MKNOD, so an archive cannot recreate one however it is
// packed.
func TestAnArchiveCarryingADeviceNodeCannotRecreateIt(t *testing.T) {
	volume := volumeDir(t)

	if packed := inContainer(t, volume, "", `cd / && tar -cf /work/hostile.tar dev/null 2>&1; echo PACKED=$?`); !strings.Contains(packed.output, "PACKED=0") {
		t.Skipf("could not pack a device node to attack with:\n%s", packed.output)
	}

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"tar", "-xf", "/work/hostile.tar", "-C", staging)...)

	if strings.Contains(inContainer(t, volume, "", `test -c /work/out/dev/null && echo NODE || echo none`).output, "NODE") {
		t.Errorf("a device node was published onto the volume\n%s", result.output)
	}
	requireNoArtefacts(t, volume)
}

// The operands are the other way in: name a destination outside the volume and
// ask the publish to write there. flux-op holds that invariant itself rather
// than on trust from the caller that resolved the paths.
func TestPublishingOutsideTheVolumeRootIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/src && echo new > /work/src/f`)

	staging := "/work/.flux-op-" + operationID
	for _, destination := range []string{"/etc/pwned", "/work/../pwned", "/"} {
		result := fluxOp(t, volume, "",
			append(baseArgs("--discard-staging", "--mkdir", staging, destination, "--"),
				"cp", "-a", "-T", "/work/src", staging)...)

		if result.exit == 0 {
			t.Errorf("publishing to %s succeeded\n%s", destination, result.output)
		}
	}
	requireNoArtefacts(t, volume)
}

// A FIFO is refused for a reason that has nothing to do with escaping: whatever
// opens one without O_NONBLOCK waits for a writer that never comes, so one left
// on a volume is a reader that hangs.
func TestAnArchiveCarryingAFifoIsRefused(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build && cd /work/build && mkfifo pipe && echo data > ordinary &&
		tar -cf /work/hostile.tar pipe ordinary`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"tar", "-xf", "/work/hostile.tar", "-C", staging)...)

	if result.exit != 4 {
		t.Fatalf("exit %d, want 4 - the status that says the result is not data\n%s", result.exit, result.output)
	}
	// Named, so an owner told their archive was refused has somewhere to look.
	if !strings.Contains(result.output, "pipe") {
		t.Errorf("the refusal does not name the entry:\n%s", result.output)
	}
	if exists(t, volume, "out") {
		t.Error("the destination was published despite the refusal")
	}
	requireNoArtefacts(t, volume)
}

// An archive of an application's own data commonly holds both, and both are
// content rather than a threat: the link is published as a link, and two names
// for one file stay two names for one file.
func TestAnArchiveOfLinksAndDuplicatesIsPublished(t *testing.T) {
	volume := volumeDir(t)
	seed(t, volume, `mkdir -p /work/build/dir && cd /work/build &&
		echo data > dir/original &&
		ln dir/original dir/second-name &&
		ln -s original dir/relative &&
		ln -s /etc/hosts dir/absolute &&
		tar -cf /work/archive.tar dir`)

	staging := "/work/.flux-op-" + operationID
	result := fluxOp(t, volume, "",
		append(baseArgs("--discard-staging", "--mkdir", "--data-only", staging, "/work/out", "--"),
			"tar", "-xf", "/work/archive.tar", "-C", staging)...)

	if result.exit != 0 {
		t.Fatalf("exit %d, want 0 - links and duplicates are ordinary content\n%s", result.exit, result.output)
	}
	for _, name := range []string{"out/dir/original", "out/dir/second-name", "out/dir/relative", "out/dir/absolute"} {
		if !exists(t, volume, name) {
			t.Errorf("%s is not there\n%s", name, tree(t, volume))
		}
	}
	// As a link, not as a copy of what it points at.
	if target := inContainer(t, volume, "", `readlink /work/out/dir/absolute`); !strings.Contains(target.output, "/etc/hosts") {
		t.Errorf("the link was published as something else: %s", target.output)
	}
	// And one file, not two: the second name still shares the inode.
	links := inContainer(t, volume, "", `stat -c %h /work/out/dir/original`)
	if count, err := strconv.Atoi(strings.TrimSpace(lastLine(links.output))); err != nil || count != 2 {
		t.Errorf("the hard link was published as a separate copy (link count %q)", links.output)
	}
	requireNoArtefacts(t, volume)
}
