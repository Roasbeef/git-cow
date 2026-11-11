package cow

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// untrackedModes are the ways a worktree is filled. The first two clone from
// the source. The last is the plain "git worktree add" fallback, which holds
// only what the commit-ish tracks, whatever is asked for.
var untrackedModes = []struct {
	name  string
	clone bool
	opts  AddOptions
}{
	{"default", true, AddOptions{}},
	{"walk", true, AddOptions{forceWalk: true}},
	{"plain", false, AddOptions{forceNoCoW: true}},
}

// snapshot records every entry beneath dir except the Git directory: its
// type, permission bits, and content or symlink target. Two snapshots are
// equal only if the trees are byte for byte the same.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := make(map[string]string)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(dir, p)
		if rel == ".git" {
			return fs.SkipDir
		}

		info, err := os.Lstat(p)
		if err != nil {
			return err
		}

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link " + target

		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %o", info.Mode().Perm())

		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			out[rel] = fmt.Sprintf("file %o %s", info.Mode().Perm(), b)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// sameSnapshot fails the test when two snapshots differ.
func sameSnapshot(t *testing.T, what string, before, after map[string]string) {
	t.Helper()

	var diffs []string

	for k, v := range before {
		if after[k] != v {
			diffs = append(diffs, fmt.Sprintf("%q: %q -> %q", k, v, after[k]))
		}
	}

	for k := range after {
		if _, ok := before[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("%q appeared", k))
		}
	}

	sort.Strings(diffs)

	if len(diffs) > 0 {
		t.Errorf("%s changed:\n%s", what, strings.Join(diffs, "\n"))
	}
}

// exists reports whether path exists, without following a final symlink.
func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// requireStatusEmpty fails unless git shows nothing at all in dir, tracked or
// untracked.
func requireStatusEmpty(t *testing.T, dir string) {
	t.Helper()

	if got := git(t, dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status in %s = %q, want nothing", dir, got)
	}
}

// addMode adds a worktree in the given mode and fails the test on error.
func addMode(t *testing.T, src string, opts AddOptions) AddResult {
	t.Helper()

	opts.Branch = "feat"

	res, err := addTo(t, src, "wt", opts)
	if err != nil {
		t.Fatal(err)
	}

	return res
}

// TestAddLeavesUntrackedShapesBehind covers each shape of untracked path:
// a wholly untracked directory, a file inside a tracked directory, a
// directory that also holds ignored files, and awkward names.
func TestAddLeavesUntrackedShapesBehind(t *testing.T) {
	odd := []string{
		"-rf", "--dir x/inner.txt", "with space.txt", "new\nline.txt",
		"quote\"d'.txt", "back\\slash.txt", "tab\there.txt",
	}

	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)
			write(t, filepath.Join(src, "scratch", "deep", "x.txt"), "x\n")
			write(t, filepath.Join(src, "src", "new.txt"), "new\n")
			write(t, filepath.Join(src, "mix", "notes.txt"), "notes\n")
			write(t, filepath.Join(src, "mix", "build", "out.o"), "object\n")
			write(t, filepath.Join(src, "build", "out.o"), "object\n")

			for _, name := range odd {
				write(t, filepath.Join(src, "odd", name), name)
			}

			before := snapshot(t, src)
			res := addMode(t, src, mode.opts)

			for _, gone := range []string{"scratch", "src/new.txt", "mix/notes.txt", "odd"} {
				if exists(filepath.Join(res.Path, gone)) {
					t.Errorf("%s was carried", gone)
				}
			}

			if got := read(t, filepath.Join(res.Path, "src", "a.txt")); got != "alpha\n" {
				t.Errorf("tracked file holds %q", got)
			}

			if mode.clone {
				// git clean -ffd without -x keeps ignored files, and keeps
				// the directory that holds them while removing its
				// untracked file.
				for _, kept := range []string{"mix/build/out.o", "build/out.o"} {
					if got := read(t, filepath.Join(res.Path, kept)); got != "object\n" {
						t.Errorf("ignored %s holds %q", kept, got)
					}
				}
			}

			requireStatusEmpty(t, res.Path)
			sameSnapshot(t, "the source", before, snapshot(t, src))
		})
	}
}

// TestAddUntrackedFlagKeepsEverything is the mirror: on the clone path
// --untracked carries everything as before. On the plain path it is accepted
// and changes nothing.
func TestAddUntrackedFlagKeepsEverything(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)
			write(t, filepath.Join(src, "scratch", "x.txt"), "x\n")
			write(t, filepath.Join(src, "src", "new.txt"), "new\n")
			write(t, filepath.Join(src, "new\nline.txt"), "nl\n")
			write(t, filepath.Join(src, "build", "out.o"), "object\n")

			before := snapshot(t, src)

			opts := mode.opts
			opts.Untracked = true
			res := addMode(t, src, opts)

			for _, p := range []string{"scratch/x.txt", "src/new.txt", "new\nline.txt"} {
				if got := exists(filepath.Join(res.Path, p)); got != mode.clone {
					t.Errorf("%q present = %v, want %v", p, got, mode.clone)
				}
			}

			if got := exists(filepath.Join(res.Path, "build", "out.o")); got != mode.clone {
				t.Errorf("ignored file present = %v, want %v", got, mode.clone)
			}

			sameSnapshot(t, "the source", before, snapshot(t, src))
		})
	}
}

// TestAddAtOlderCommitWithFewerIgnoreRules is the ordinary case of adding a
// worktree at an older commit whose .gitignore lacks a rule the source's HEAD
// has. The build output the old rules do not hide is untracked there, so it
// is left out of the new worktree, and the add succeeds.
func TestAddAtOlderCommitWithFewerIgnoreRules(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)
			git(t, src, "tag", "v1")

			write(t, filepath.Join(src, ".gitignore"), "build/\ndist/\n.env\n")
			git(t, src, "add", ".gitignore")
			git(t, src, "commit", "-q", "-m", "ignore more")

			write(t, filepath.Join(src, "dist", "app"), "binary\n")
			write(t, filepath.Join(src, ".env"), "SECRET=1\n")
			write(t, filepath.Join(src, "build", "out.o"), "object\n")

			before := snapshot(t, src)

			opts := mode.opts
			opts.Commitish = "v1"
			res := addMode(t, src, opts)

			for _, gone := range []string{"dist", ".env"} {
				if exists(filepath.Join(res.Path, gone)) {
					t.Errorf("%s is not ignored at v1 and was carried", gone)
				}
			}

			if mode.clone {
				if got := read(t, filepath.Join(res.Path, "build", "out.o")); got != "object\n" {
					t.Errorf("build output holds %q", got)
				}
			}

			requireStatusEmpty(t, res.Path)
			sameSnapshot(t, "the source", before, snapshot(t, src))
		})
	}
}

// TestAddTrackedDecomposedName checks a tracked file whose on-disk name is
// decomposed (NFD), as Finder makes them, does not break add. Git judges
// names with the repository's own settings.
func TestAddTrackedDecomposedName(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("names are bytes elsewhere")
	}

	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "café.txt"), "tracked\n")
	write(t, filepath.Join(src, "diŕ", "n.txt"), "untracked\n")
	git(t, src, "add", "café.txt")
	git(t, src, "commit", "-q", "-m", "nfd")

	before := snapshot(t, src)
	res := addMode(t, src, AddOptions{})

	if got := read(t, filepath.Join(res.Path, "café.txt")); got != "tracked\n" {
		t.Errorf("tracked file holds %q", got)
	}

	if exists(filepath.Join(res.Path, "diŕ")) {
		t.Error("the untracked directory was carried")
	}

	requireStatusEmpty(t, res.Path)
	sameSnapshot(t, "the source", before, snapshot(t, src))
}

// TestAddUntrackedIgnoreFile pins what an untracked .gitignore does. It is
// itself untracked so it is removed, and the files it was hiding are not
// ignored in the new worktree, so they are removed too.
func TestAddUntrackedIgnoreFile(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "pkg", ".gitignore"), "*.o\n")
	write(t, filepath.Join(src, "pkg", "x.o"), "object\n")
	write(t, filepath.Join(src, "pkg", "keep.go"), "package pkg\n")

	before := snapshot(t, src)
	res := addMode(t, src, AddOptions{})

	if exists(filepath.Join(res.Path, "pkg")) {
		t.Errorf("the new worktree holds pkg: %v", names(t, filepath.Join(res.Path, "pkg")))
	}

	requireStatusEmpty(t, res.Path)
	sameSnapshot(t, "the source", before, snapshot(t, src))
}

// TestAddNestedRepositories checks an untracked nested repository's copy is
// removed from the new worktree and the original is still a working
// repository, and that an ignored one is kept.
func TestAddNestedRepositories(t *testing.T) {
	requireClone(t)

	src := newRepo(t)

	inner := filepath.Join(src, "nested")
	write(t, filepath.Join(inner, "file.txt"), "inner\n")
	git(t, inner, "init", "-q", "-b", "main")
	git(t, inner, "add", "-A")
	git(t, inner, "commit", "-q", "-m", "inner")
	write(t, filepath.Join(inner, "wip.txt"), "inner untracked\n")

	vendored := filepath.Join(src, "vendored")
	write(t, filepath.Join(vendored, "lib.txt"), "lib\n")
	git(t, vendored, "init", "-q", "-b", "main")
	write(t, filepath.Join(src, ".git", "info", "exclude"), "vendored/\n")

	before := snapshot(t, src)
	res := addMode(t, src, AddOptions{})

	if exists(filepath.Join(res.Path, "nested")) {
		t.Error("the untracked nested repository was carried")
	}

	if got := read(t, filepath.Join(res.Path, "vendored", "lib.txt")); got != "lib\n" {
		t.Errorf("the ignored nested repository holds %q", got)
	}

	requireStatusEmpty(t, res.Path)
	sameSnapshot(t, "the source", before, snapshot(t, src))

	if got := git(t, inner, "status", "--porcelain"); !strings.Contains(got, "wip.txt") {
		t.Errorf("nested repository status = %q", got)
	}

	if got := git(t, inner, "log", "--format=%s"); got != "inner" {
		t.Errorf("nested repository log = %q", got)
	}
}

// TestAddNeverFollowsUntrackedSymlinks checks untracked symlinks are removed
// from the new worktree and never followed, and the targets are untouched.
func TestAddNeverFollowsUntrackedSymlinks(t *testing.T) {
	requireClone(t)

	src := newRepo(t)

	outside := filepath.Join(filepath.Dir(src), "outside")
	write(t, filepath.Join(outside, "precious.txt"), "precious\n")

	for name, target := range map[string]string{
		"out-link": outside, "in-link": "src", "dangling": "nowhere",
	} {
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}

	before := snapshot(t, src)
	beforeOutside := snapshot(t, outside)
	res := addMode(t, src, AddOptions{})

	for _, l := range []string{"out-link", "in-link", "dangling"} {
		if exists(filepath.Join(res.Path, l)) {
			t.Errorf("symlink %s was carried", l)
		}
	}

	requireStatusEmpty(t, res.Path)
	sameSnapshot(t, "the source", before, snapshot(t, src))
	sameSnapshot(t, "the outside directory", beforeOutside, snapshot(t, outside))
}

// TestAddCommitishReplacesUntracked checks a path the commit-ish tracks wins
// over the source's untracked file, with and without the flag.
func TestAddCommitishReplacesUntracked(t *testing.T) {
	requireClone(t)

	for _, untracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("untracked=%v", untracked), func(t *testing.T) {
			src := newRepo(t)

			git(t, src, "checkout", "-q", "-b", "other")
			write(t, filepath.Join(src, "notes.txt"), "tracked\n")
			git(t, src, "add", "-A")
			git(t, src, "commit", "-q", "-m", "tracks notes")
			git(t, src, "checkout", "-q", "main")

			write(t, filepath.Join(src, "notes.txt"), "mine\n")
			write(t, filepath.Join(src, "other.txt"), "mine too\n")

			res := addMode(t, src, AddOptions{Commitish: "other", Untracked: untracked})

			if got := read(t, filepath.Join(res.Path, "notes.txt")); got != "tracked\n" {
				t.Errorf("notes.txt holds %q", got)
			}

			if got := read(t, filepath.Join(src, "notes.txt")); got != "mine\n" {
				t.Errorf("the source's file holds %q", got)
			}

			if got := exists(filepath.Join(res.Path, "other.txt")); got != untracked {
				t.Errorf("other.txt present = %v, want %v", got, untracked)
			}
		})
	}
}

// TestAddInsideSourceAndNestedWorktree checks the two skips the clone path
// already had: a destination inside the source, and a linked worktree that
// lives in the source. Nothing of the other worktree is cleaned or copied.
func TestAddInsideSourceAndNestedWorktree(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, ".git", "info", "exclude"), ".worktrees/\n")

	other, err := AddWorktree(context.Background(), AddOptions{
		Dir: src, Path: filepath.Join(src, ".worktrees", "other"), Branch: "other",
	})
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(other.Path, "scratch.txt"), "other's own\n")
	write(t, filepath.Join(src, "notes.txt"), "mine\n")

	res, err := AddWorktree(context.Background(), AddOptions{
		Dir: src, Path: filepath.Join(src, ".worktrees", "new"), Branch: "new",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(other.Path, "scratch.txt")); got != "other's own\n" {
		t.Errorf("the other worktree's file holds %q", got)
	}

	if exists(filepath.Join(res.Path, ".worktrees")) || exists(filepath.Join(res.Path, "notes.txt")) {
		t.Error("the new worktree holds the other worktree or an untracked file")
	}

	requireStatusEmpty(t, res.Path)

	if got := read(t, filepath.Join(src, "notes.txt")); got != "mine\n" {
		t.Errorf("the source's file holds %q", got)
	}
}

// TestAddThenRemoveNeedsNoForce checks a default add leaves nothing that
// makes git refuse to remove the worktree, while --untracked does.
func TestAddThenRemoveNeedsNoForce(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "notes.txt"), "mine\n")
	write(t, filepath.Join(src, "build", "out.o"), "object\n")

	res := addMode(t, src, AddOptions{})
	git(t, src, "worktree", "remove", res.Path)

	if exists(res.Path) {
		t.Error("the worktree directory remains")
	}

	res, err := addTo(t, src, "wt2", AddOptions{Branch: "feat2", Untracked: true})
	if err != nil {
		t.Fatal(err)
	}

	if out, err := gitErr(src, "worktree", "remove", res.Path); err == nil {
		t.Errorf("remove succeeded with untracked files carried: %s", out)
	}
}

// TestCleanGuard runs cleanGuard against pairs of real repositories and
// checks it refuses everything but a checkout root that is not the source.
func TestCleanGuard(t *testing.T) {
	src := newRepo(t)
	other := newRepo(t)
	write(t, filepath.Join(other, "sub", "keep.txt"), "keep\n")

	// A linked worktree is its own checkout root.
	linked := filepath.Join(filepath.Dir(src), "linked")
	git(t, src, "worktree", "add", "-q", "--detach", linked)

	cases := []struct {
		name      string
		src, dest string
		ok        bool
	}{
		{"a different checkout root", src, other, true},
		{"a linked worktree", src, linked, true},
		{"the source itself", src, src, false},
		{"the source through a symlink", src, symlinkTo(t, src), false},
		{"a subdirectory of a checkout", src, filepath.Join(other, "sub"), false},
		{"a subdirectory of the source", src, filepath.Join(src, "src"), false},
		{"not a repository", src, t.TempDir(), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := cleanGuard(context.Background(), c.src, c.dest)
			if (err == nil) != c.ok {
				t.Errorf("cleanGuard = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// symlinkTo returns a symlink to dir in a fresh directory.
func symlinkTo(t *testing.T, dir string) string {
	t.Helper()

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	return link
}

// TestAddReadOnlyUntrackedDirectory checks that an untracked read-only
// directory, which git clean cannot empty, fails the add with a message that
// names the path and offers --untracked, rolls everything back, and leaves
// the source untouched.
func TestAddReadOnlyUntrackedDirectory(t *testing.T) {
	requireClone(t)

	if os.Geteuid() == 0 {
		t.Skip("root can remove files in a read-only directory")
	}

	src := newRepo(t)
	ro := filepath.Join(src, "ro")
	write(t, filepath.Join(ro, "f"), "r\n")

	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	before := snapshot(t, src)

	_, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err == nil {
		t.Fatal("add succeeded")
	}

	if !strings.Contains(err.Error(), "ro/f") || !strings.Contains(err.Error(), "--untracked") {
		t.Errorf("err = %v, want the path and --untracked named", err)
	}

	dest := filepath.Join(filepath.Dir(src), "wt")
	if exists(dest) {
		t.Error("destination remains after the failure")
	}

	if _, err := gitErr(src, "rev-parse", "--verify", "feat"); err == nil {
		t.Error("branch remains after the failure")
	}

	if listed(t, src, dest) {
		t.Error("worktree record remains after the failure")
	}

	sameSnapshot(t, "the source", before, snapshot(t, src))

	// --untracked keeps the directory and works.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "ro"), 0o755) })

	if _, err := addTo(t, src, "wt", AddOptions{Branch: "feat", Untracked: true}); err != nil {
		t.Errorf("with --untracked: %v", err)
	}
}

// TestAddIntoExistingEmptyDirectory checks the guard accepts what Git
// accepts, an empty directory made beforehand.
func TestAddIntoExistingEmptyDirectory(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "notes.txt"), "mine\n")

	dest := filepath.Join(filepath.Dir(src), "wt")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	res := addMode(t, src, AddOptions{})
	if exists(filepath.Join(res.Path, "notes.txt")) {
		t.Error("the untracked file was carried")
	}
}
