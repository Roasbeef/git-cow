package cow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// untrackedModes are the ways a worktree is filled. The first two clone from
// the source. The last is the plain "git worktree add" fallback, which holds
// only what the commit-ish tracks until untracked files are asked for.
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

// TestAddLeavesUntrackedShapesBehind covers each shape of untracked path:
// a wholly untracked directory, a file inside a tracked directory, an
// untracked directory that also holds ignored files, and awkward names.
func TestAddLeavesUntrackedShapesBehind(t *testing.T) {
	odd := []string{
		"-rf",
		"--dir x/inner.txt",
		"with space.txt",
		"new\nline.txt",
		"quote\"d'.txt",
		"back\\slash.txt",
		"café.txt",
		"tab\there.txt",
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

			opts := mode.opts
			opts.Branch = "feat"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			for _, gone := range []string{
				"scratch", "src/new.txt", "mix/notes.txt", "odd",
			} {
				if exists(filepath.Join(res.Path, gone)) {
					t.Errorf("%s was carried", gone)
				}
			}

			if got := read(t, filepath.Join(res.Path, "src", "a.txt")); got != "alpha\n" {
				t.Errorf("tracked file holds %q", got)
			}

			if mode.clone {
				// Ignored files stay, and so does the directory that holds
				// them, while its untracked file goes.
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

// TestAddUntrackedFlagCarriesEverything is the mirror: with Untracked the
// untracked files come along as before, awkward names included.
func TestAddUntrackedFlagCarriesEverything(t *testing.T) {
	odd := []string{"-rf", "with space.txt", "new\nline.txt", "quote\"d'.txt"}

	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)
			write(t, filepath.Join(src, "scratch", "deep", "x.txt"), "x\n")
			write(t, filepath.Join(src, "src", "new.txt"), "new\n")
			write(t, filepath.Join(src, "mix", "notes.txt"), "notes\n")
			write(t, filepath.Join(src, "build", "out.o"), "object\n")

			for _, name := range odd {
				write(t, filepath.Join(src, "odd", name), name)
			}

			before := snapshot(t, src)

			opts := mode.opts
			opts.Branch = "feat"
			opts.Untracked = true

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			want := map[string]string{
				"scratch/deep/x.txt": "x\n",
				"src/new.txt":        "new\n",
				"mix/notes.txt":      "notes\n",
			}

			for _, name := range odd {
				want["odd/"+name] = name
			}

			for p, body := range want {
				if got := read(t, filepath.Join(res.Path, p)); got != body {
					t.Errorf("%q holds %q, want %q", p, got, body)
				}
			}

			if mode.clone {
				if got := read(t, filepath.Join(res.Path, "build", "out.o")); got != "object\n" {
					t.Errorf("ignored file holds %q", got)
				}
			}

			status := git(t, res.Path, "status", "--porcelain")
			if !strings.Contains(status, "?? scratch/") || !strings.Contains(status, "?? mix/") {
				t.Errorf("status = %q, want the untracked files listed", status)
			}

			sameSnapshot(t, "the source", before, snapshot(t, src))
		})
	}
}

// TestAddNestedRepositories checks that an untracked nested repository is
// left behind whole, without looking inside it, and that an ignored one is
// carried like any other ignored file. Neither is touched in the source.
func TestAddNestedRepositories(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)

			inner := filepath.Join(src, "nested")
			write(t, filepath.Join(inner, "file.txt"), "inner\n")
			git(t, inner, "init", "-q", "-b", "main")
			git(t, inner, "add", "-A")
			git(t, inner, "commit", "-q", "-m", "inner")
			write(t, filepath.Join(inner, "wip.txt"), "inner untracked\n")
			write(t, filepath.Join(inner, ".gitignore"), "*.o\n")
			write(t, filepath.Join(inner, "inner.o"), "inner ignored\n")

			vendored := filepath.Join(src, "vendored")
			write(t, filepath.Join(vendored, "lib.txt"), "lib\n")
			git(t, vendored, "init", "-q", "-b", "main")
			write(t, filepath.Join(src, ".git", "info", "exclude"), "vendored/\n")

			before := snapshot(t, src)

			opts := mode.opts
			opts.Branch = "feat"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			if exists(filepath.Join(res.Path, "nested")) {
				t.Error("the untracked nested repository was carried")
			}

			if mode.clone {
				if got := read(t, filepath.Join(res.Path, "vendored", "lib.txt")); got != "lib\n" {
					t.Errorf("the ignored nested repository holds %q", got)
				}
			}

			requireStatusEmpty(t, res.Path)
			sameSnapshot(t, "the source", before, snapshot(t, src))

			// The nested repository is still a working repository.
			if got := git(t, inner, "status", "--porcelain"); !strings.Contains(got, "wip.txt") {
				t.Errorf("nested repository status = %q", got)
			}
		})
	}
}

// TestAddUntrackedNestedRepositoryWithFlag checks --untracked carries a
// nested repository with its own Git directory.
func TestAddUntrackedNestedRepositoryWithFlag(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)

			inner := filepath.Join(src, "nested")
			write(t, filepath.Join(inner, "file.txt"), "inner\n")
			git(t, inner, "init", "-q", "-b", "main")

			opts := mode.opts
			opts.Branch = "feat"
			opts.Untracked = true

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			if got := read(t, filepath.Join(res.Path, "nested", "file.txt")); got != "inner\n" {
				t.Errorf("nested file holds %q", got)
			}

			if !exists(filepath.Join(res.Path, "nested", ".git")) {
				t.Error("the nested repository lost its Git directory")
			}
		})
	}
}

// TestAddSubmoduleWithUntrackedFiles checks a submodule is still left as an
// empty directory and that neither the submodule's untracked files nor the
// source's are touched.
func TestAddSubmoduleWithUntrackedFiles(t *testing.T) {
	src := newRepo(t)
	sub := newRepo(t)

	git(t, src, "submodule", "add", "-q", sub, "sm")
	git(t, src, "commit", "-q", "-m", "add submodule")

	write(t, filepath.Join(src, "sm", "untracked.o"), "artifact\n")
	write(t, filepath.Join(src, "notes.txt"), "mine\n")

	before := snapshot(t, src)

	res, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	if entries := names(t, filepath.Join(res.Path, "sm")); len(entries) != 0 {
		t.Errorf("submodule directory holds %v, want empty", entries)
	}

	if exists(filepath.Join(res.Path, "notes.txt")) {
		t.Error("the untracked file was carried")
	}

	sameSnapshot(t, "the source", before, snapshot(t, src))
}

// TestAddNeverFollowsUntrackedSymlinks checks that untracked symlinks, to a
// directory outside the source or inside it, are left behind, and that the
// directory they point at is untouched.
func TestAddNeverFollowsUntrackedSymlinks(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)

			outside := filepath.Join(filepath.Dir(src), "outside")
			write(t, filepath.Join(outside, "precious.txt"), "precious\n")

			if err := os.Symlink(outside, filepath.Join(src, "out-link")); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink("src", filepath.Join(src, "in-link")); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink("nowhere", filepath.Join(src, "dangling")); err != nil {
				t.Fatal(err)
			}

			before := snapshot(t, src)
			beforeOutside := snapshot(t, outside)

			opts := mode.opts
			opts.Branch = "feat"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			for _, l := range []string{"out-link", "in-link", "dangling"} {
				if exists(filepath.Join(res.Path, l)) {
					t.Errorf("symlink %s was carried", l)
				}
			}

			requireStatusEmpty(t, res.Path)
			sameSnapshot(t, "the source", before, snapshot(t, src))
			sameSnapshot(t, "the outside directory", beforeOutside, snapshot(t, outside))
		})
	}
}

// TestAddCommitishReplacesUntracked checks that a path the commit-ish
// tracks wins over the source's untracked file, with and without the flag,
// and that the source keeps its own file.
func TestAddCommitishReplacesUntracked(t *testing.T) {
	for _, untracked := range []bool{false, true} {
		for _, mode := range untrackedModes {
			name := fmt.Sprintf("%s/untracked=%v", mode.name, untracked)

			t.Run(name, func(t *testing.T) {
				if mode.clone {
					requireClone(t)
				}

				src := newRepo(t)

				git(t, src, "checkout", "-q", "-b", "other")
				write(t, filepath.Join(src, "notes.txt"), "tracked\n")
				write(t, filepath.Join(src, "dir", "f.txt"), "tracked\n")
				git(t, src, "add", "-A")
				git(t, src, "commit", "-q", "-m", "tracks notes")
				git(t, src, "checkout", "-q", "main")

				write(t, filepath.Join(src, "notes.txt"), "mine\n")
				write(t, filepath.Join(src, "dir", "g.txt"), "mine\n")
				write(t, filepath.Join(src, "other.txt"), "mine too\n")

				opts := mode.opts
				opts.Branch = "feat"
				opts.Commitish = "other"
				opts.Untracked = untracked

				res, err := addTo(t, src, "wt", opts)
				if err != nil {
					t.Fatal(err)
				}

				for _, p := range []string{"notes.txt", "dir/f.txt"} {
					if got := read(t, filepath.Join(res.Path, p)); got != "tracked\n" {
						t.Errorf("%s holds %q, want the tracked content", p, got)
					}
				}

				if got := read(t, filepath.Join(src, "notes.txt")); got != "mine\n" {
					t.Errorf("the source's file holds %q", got)
				}

				// The tracked ones are not modified. Whatever else is
				// untracked is only there if it was asked for.
				got := git(t, res.Path, "status", "--porcelain", "--untracked-files=all")
				if untracked {
					if !strings.Contains(got, "?? other.txt") {
						t.Errorf("status = %q, want other.txt listed", got)
					}
				} else if got != "" {
					t.Errorf("status = %q, want nothing", got)
				}
			})
		}
	}
}

// TestAddRefusesWhenIgnoreFileIsLeftBehind covers the one way an untracked
// file arrives anyway: an untracked ignore file hid build output in the
// source, and without it that output would show as untracked in the new
// worktree. The add fails, is rolled back, and the flag makes it work.
func TestAddRefusesWhenIgnoreFileIsLeftBehind(t *testing.T) {
	requireClone(t)

	for _, mode := range untrackedModes[:2] {
		t.Run(mode.name, func(t *testing.T) {
			src := newRepo(t)
			write(t, filepath.Join(src, "pkg", ".gitignore"), "*.o\n")
			write(t, filepath.Join(src, "pkg", "x.o"), "object\n")

			before := snapshot(t, src)

			opts := mode.opts
			opts.Branch = "feat"

			_, err := addTo(t, src, "wt", opts)

			var r *RefusalError
			if !errors.As(err, &r) || !strings.Contains(err.Error(), "--untracked") {
				t.Fatalf("err = %v, want a refusal that names --untracked", err)
			}

			if exists(filepath.Join(filepath.Dir(src), "wt")) {
				t.Error("destination remains after the refusal")
			}

			if _, err := gitErr(src, "rev-parse", "--verify", "feat"); err == nil {
				t.Error("branch remains after the refusal")
			}

			if listed(t, src, filepath.Join(filepath.Dir(src), "wt")) {
				t.Error("worktree record remains after the refusal")
			}

			sameSnapshot(t, "the source", before, snapshot(t, src))

			opts.Untracked = true
			if _, err := addTo(t, src, "wt", opts); err != nil {
				t.Fatalf("with --untracked: %v", err)
			}
		})
	}
}

// TestAddPlainUntrackedNeverWritesThroughSymlinks checks the plain
// fallback with --untracked does not write through a symlink that the
// commit-ish tracks where the source has an untracked directory.
func TestAddPlainUntrackedNeverWritesThroughSymlinks(t *testing.T) {
	src := newRepo(t)

	outside := filepath.Join(filepath.Dir(src), "outside")
	write(t, filepath.Join(outside, "precious.txt"), "precious\n")

	git(t, src, "checkout", "-q", "-b", "other")

	if err := os.Symlink(outside, filepath.Join(src, "lnk")); err != nil {
		t.Fatal(err)
	}

	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "tracks a symlink")
	git(t, src, "checkout", "-q", "main")

	write(t, filepath.Join(src, "lnk", "evil.txt"), "evil\n")
	write(t, filepath.Join(src, "keep", "k.txt"), "kept\n")

	before := snapshot(t, src)
	beforeOutside := snapshot(t, outside)

	res, err := addTo(t, src, "wt", AddOptions{
		forceNoCoW: true, Untracked: true, Branch: "feat", Commitish: "other",
	})
	if err != nil {
		t.Fatal(err)
	}

	if exists(filepath.Join(outside, "evil.txt")) {
		t.Fatal("a file was written through a symlink")
	}

	if got := read(t, filepath.Join(res.Path, "keep", "k.txt")); got != "kept\n" {
		t.Errorf("untracked file holds %q", got)
	}

	sameSnapshot(t, "the source", before, snapshot(t, src))
	sameSnapshot(t, "the outside directory", beforeOutside, snapshot(t, outside))
}

// TestAddThenRemoveNeedsNoForce checks that a worktree made by default has
// nothing that makes git refuse to remove it, and that one made with
// Untracked does, as git defines.
func TestAddThenRemoveNeedsNoForce(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "notes.txt"), "mine\n")
	write(t, filepath.Join(src, "build", "out.o"), "object\n")

	res, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	git(t, src, "worktree", "remove", res.Path)

	if exists(res.Path) {
		t.Error("the worktree directory remains")
	}

	res, err = addTo(t, src, "wt2", AddOptions{Branch: "feat2", Untracked: true})
	if err != nil {
		t.Fatal(err)
	}

	if out, err := gitErr(src, "worktree", "remove", res.Path); err == nil {
		t.Errorf("remove succeeded with untracked files carried: %s", out)
	}
}

// TestAddCanceledLeavesSourceUntouched checks a canceled add still leaves
// the source alone.
func TestAddCanceledLeavesSourceUntouched(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	write(t, filepath.Join(src, "notes.txt"), "mine\n")
	before := snapshot(t, src)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := AddWorktree(ctx, AddOptions{Dir: src, Path: filepath.Join(filepath.Dir(src), "wt"), Branch: "feat"})
	if err == nil {
		t.Fatal("expected an error")
	}

	sameSnapshot(t, "the source", before, snapshot(t, src))
}

// TestAddLeavesDecomposedNamesBehind checks a file whose name is stored
// decomposed (NFD), as Finder makes them, is left behind. Git on macOS
// reports such a name precomposed by default, which would not match the name
// the walk reads.
func TestAddLeavesDecomposedNamesBehind(t *testing.T) {
	for _, mode := range untrackedModes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.clone {
				requireClone(t)
			}

			src := newRepo(t)
			write(t, filepath.Join(src, "café.txt"), "decomposed\n")
			write(t, filepath.Join(src, "café-2.txt"), "precomposed\n")
			write(t, filepath.Join(src, "diŕ", "n.txt"), "decomposed dir\n")

			before := snapshot(t, src)

			opts := mode.opts
			opts.Branch = "feat"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			if got := names(t, res.Path); len(got) > 0 {
				for _, n := range got {
					if strings.Contains(n, "caf") || strings.Contains(n, "dir") {
						t.Errorf("%q was carried", n)
					}
				}
			}

			requireStatusEmpty(t, res.Path)
			sameSnapshot(t, "the source", before, snapshot(t, src))
		})
	}
}
