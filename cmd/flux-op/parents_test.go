package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// Who owns what is made is decided in the container tests, which run as root:
// here every folder belongs to the user running the tests whoever made it.

func requireDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s was created", path)
	}
}

func TestAnUploadBringsItsMissingFoldersWithIt(t *testing.T) {
	v := newVolume(t)
	destination := filepath.Join(v.root, "config", "neoforge", "deep", "fml.toml")
	withStdin(t, "the upload")

	argv := []string{"--root", v.root, "--discard-staging", "--from-stdin", "--parents", "--inherit-owner",
		v.staging, destination, "--"}
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, level := range []string{"config", "config/neoforge", "config/neoforge/deep"} {
		requireDirectory(t, filepath.Join(v.root, level))
	}
	if got := read(t, destination); got != "the upload" {
		t.Errorf("destination holds %q", got)
	}
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// The default is unchanged: a destination whose folder is missing is refused,
// and nothing is made on the way.
func TestWithoutTheFlagAMissingFolderIsRefused(t *testing.T) {
	v := newVolume(t)
	destination := filepath.Join(v.root, "config", "fml.toml")
	withStdin(t, "the upload")

	argv := []string{"--root", v.root, "--discard-staging", "--from-stdin", "--inherit-owner",
		v.staging, destination, "--"}
	if code := run(argv); code == 0 {
		t.Fatal("published into a folder that does not exist")
	}
	requireAbsent(t, filepath.Join(v.root, "config"))
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// Creating a folder whose parents are missing is the same shape as creating
// one whose parents are there.
func TestCreatingAFolderWithItsParents(t *testing.T) {
	v := newVolume(t)
	destination := filepath.Join(v.root, "a", "b", "c")

	argv := []string{"--root", v.root, "--discard-staging", "--mkdir", "--no-replace", "--parents", "--inherit-owner",
		v.staging, destination, "--"}
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	requireDirectory(t, destination)
	if left := v.leftovers(t); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// What is already there is used as it is, and what is in it is left alone.
func TestAnExistingFolderIsUsedAsItIs(t *testing.T) {
	v := newVolume(t)
	write(t, filepath.Join(v.root, "a", "keep.txt"), "precious")
	destination := filepath.Join(v.root, "a", "b", "new.txt")
	withStdin(t, "the upload")

	argv := []string{"--root", v.root, "--discard-staging", "--from-stdin", "--parents",
		v.staging, destination, "--"}
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, filepath.Join(v.root, "a", "keep.txt")); got != "precious" {
		t.Errorf("an existing file holds %q", got)
	}
	if got := read(t, destination); got != "the upload" {
		t.Errorf("destination holds %q", got)
	}
}

// Folders are made only for a result that is going to be published.
func TestAFailedOperationMakesNoFolders(t *testing.T) {
	cases := []struct {
		name  string
		input string
		flags []string
		cmd   []string
	}{
		{"a command that fails", "", []string{"--mkdir"}, []string{"false"}},
		{"an upload over the ceiling", "too large", []string{"--from-stdin", "--max-bytes", "1"}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			v := newVolume(t)
			withStdin(t, testCase.input)
			destination := filepath.Join(v.root, "a", "b", "result")

			argv := append([]string{"--root", v.root, "--discard-staging", "--parents"}, testCase.flags...)
			argv = append(argv, v.staging, destination, "--")
			argv = append(argv, testCase.cmd...)
			if code := run(argv); code == 0 {
				t.Fatal("reported success")
			}
			requireAbsent(t, filepath.Join(v.root, "a"))
			if left := v.leftovers(t); len(left) != 0 {
				t.Errorf("left behind %v", left)
			}
		})
	}
}

// Anything but a directory at a level refuses the operation and is left as it
// was, as mkdir -p refuses it.
func TestSomethingOtherThanAFolderAtALevelIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string)
		check   func(t *testing.T, root string)
	}{
		{
			"a file at the top",
			func(t *testing.T, root string) { write(t, filepath.Join(root, "a"), "a file") },
			func(t *testing.T, root string) {
				if got := read(t, filepath.Join(root, "a")); got != "a file" {
					t.Errorf("the file holds %q", got)
				}
			},
		},
		{
			"a file where the destination's own folder goes",
			func(t *testing.T, root string) { write(t, filepath.Join(root, "a", "b", "c"), "a file") },
			func(t *testing.T, root string) {
				if got := read(t, filepath.Join(root, "a", "b", "c")); got != "a file" {
					t.Errorf("the file holds %q", got)
				}
			},
		},
		{
			"a file further down",
			func(t *testing.T, root string) { write(t, filepath.Join(root, "a", "b"), "a file") },
			func(t *testing.T, root string) {
				if got := read(t, filepath.Join(root, "a", "b")); got != "a file" {
					t.Errorf("the file holds %q", got)
				}
			},
		},
		{
			"a link that leads nowhere",
			func(t *testing.T, root string) {
				if err := os.Symlink("nowhere", filepath.Join(root, "a")); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, root string) {
				if target, err := os.Readlink(filepath.Join(root, "a")); err != nil || target != "nowhere" {
					t.Errorf("the link reads %q, %v", target, err)
				}
				requireAbsent(t, filepath.Join(root, "nowhere"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			v := newVolume(t)
			testCase.prepare(t, v.root)
			withStdin(t, "the upload")
			destination := filepath.Join(v.root, "a", "b", "c", "result")

			argv := []string{"--root", v.root, "--discard-staging", "--from-stdin", "--parents",
				v.staging, destination, "--"}
			if code := run(argv); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			testCase.check(t, v.root)
			if left := v.leftovers(t); len(left) != 0 {
				t.Errorf("left behind %v", left)
			}
		})
	}
}

// A link to a folder is followed, as mkdir -p follows it.
func TestALinkToAFolderIsFollowed(t *testing.T) {
	v := newVolume(t)
	if err := os.Mkdir(filepath.Join(v.root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(v.root, "link")); err != nil {
		t.Fatal(err)
	}
	withStdin(t, "the upload")

	argv := []string{"--root", v.root, "--discard-staging", "--from-stdin", "--parents",
		v.staging, filepath.Join(v.root, "link", "new", "result"), "--"}
	if code := run(argv); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := read(t, filepath.Join(v.root, "real", "new", "result")); got != "the upload" {
		t.Errorf("result holds %q", got)
	}
}

func TestParentsAreOnlyMadeInsideTheVolume(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	cases := map[string]string{
		"outside the volume":     filepath.Join(outside, "a", "result"),
		"beside the volume":      root + "-sibling/a/result",
		"climbing out of it":     root + "/../" + filepath.Base(outside) + "/a/result",
		"the volume root itself": root,
	}
	for name, destination := range cases {
		t.Run(name, func(t *testing.T) {
			if err := makeParents(destination, root); err == nil {
				t.Fatal("made the parents of a destination outside the volume")
			}
			requireAbsent(t, filepath.Join(outside, "a"))
			requireAbsent(t, root+"-sibling")
		})
	}
}

// The reason given is the kernel's own, so the caller's log says what was in
// the way rather than that something was.
func TestARefusalReportsWhatWasInTheWay(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "b"), "a file")

	err := makeParents(filepath.Join(root, "a", "b", "c", "d", "result"), root)
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("got %v, want the kernel's ENOTDIR", err)
	}
}

// The volume root is the mount. Missing, it means the operation was pointed at
// the wrong place, and making it would put a volume where none was mounted.
func TestAMissingVolumeRootIsNeverMade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "volume")
	if err := makeParents(filepath.Join(root, "a", "result"), root); err == nil {
		t.Fatal("made the parents of a destination in a volume that is not there")
	}
	requireAbsent(t, root)
}

// Another operation making the same folders at the same moment is what a
// folder dropped into the file browser does: one request per file.
func TestOperationsMakingTheSameFoldersAtOnceAllSucceed(t *testing.T) {
	root := t.TempDir()
	const writers = 32

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = makeParents(filepath.Join(root, "a", "b", "c", "d", fmt.Sprintf("file-%d", i)), root)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	requireDirectory(t, filepath.Join(root, "a", "b", "c", "d"))
}

func TestALevelSomethingElseMadeFirst(t *testing.T) {
	root := t.TempDir()

	t.Run("a directory is used", func(t *testing.T) {
		dir := filepath.Join(root, "made-first")
		write(t, filepath.Join(dir, "theirs"), "precious")
		if err := makeLevel(dir); err != nil {
			t.Fatal(err)
		}
		if got := read(t, filepath.Join(dir, "theirs")); got != "precious" {
			t.Errorf("its contents hold %q", got)
		}
	})

	t.Run("anything else is refused", func(t *testing.T) {
		file := filepath.Join(root, "a-file")
		write(t, file, "precious")
		if err := makeLevel(file); err == nil {
			t.Fatal("accepted a file as a folder")
		}
		if got := read(t, file); got != "precious" {
			t.Errorf("the file holds %q", got)
		}
	})
}
