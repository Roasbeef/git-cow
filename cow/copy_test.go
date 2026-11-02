package cow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWritesAreIndependent writes to the working tree, .git/config and a
// hook on one side and checks the other side did not change, in both
// directions and for every strategy.
func TestWritesAreIndependent(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			dest := filepath.Join(filepath.Dir(tpl), "copy")

			if _, err := Copy(context.Background(), tpl, dest, mode.opts); err != nil {
				t.Fatal(err)
			}

			watched := []string{
				"README.md",
				"src/a.txt",
				".git/config",
				".git/hooks/pre-commit.sample2",
			}

			// Write in the copy, check the template.
			before := map[string]string{}
			for _, f := range watched {
				before[f] = read(t, filepath.Join(tpl, f))
				write(t, filepath.Join(dest, f), "changed in copy\n")
			}

			for _, f := range watched {
				if got := read(t, filepath.Join(tpl, f)); got != before[f] {
					t.Errorf("template %s changed to %q", f, got)
				}
			}

			// Write in the template, check the copy.
			for _, f := range watched {
				before[f] = read(t, filepath.Join(dest, f))
				write(t, filepath.Join(tpl, f), "changed in template\n")
			}

			for _, f := range watched {
				if got := read(t, filepath.Join(dest, f)); got != before[f] {
					t.Errorf("copy %s changed to %q", f, got)
				}
			}
		})
	}
}

// TestCommitsStayLocal commits in the copy and checks the template neither
// sees the commit nor loses integrity, and the reverse.
func TestCommitsStayLocal(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			dest := filepath.Join(filepath.Dir(tpl), "copy")
			tplHead := head(t, tpl)

			if _, err := Copy(context.Background(), tpl, dest, mode.opts); err != nil {
				t.Fatal(err)
			}

			write(t, filepath.Join(dest, "new.txt"), "from copy\n")
			git(t, dest, "add", "new.txt")
			git(t, dest, "commit", "-q", "-m", "in copy")
			copyHead := head(t, dest)

			if got := head(t, tpl); got != tplHead {
				t.Fatalf("template HEAD moved to %s", got)
			}

			if _, err := gitErr(tpl, "cat-file", "-e", copyHead); err == nil {
				t.Fatal("template can see the copy's commit object")
			}

			write(t, filepath.Join(tpl, "other.txt"), "from template\n")
			git(t, tpl, "add", "other.txt")
			git(t, tpl, "commit", "-q", "-m", "in template")

			if _, err := gitErr(dest, "cat-file", "-e", head(t, tpl)); err == nil {
				t.Fatal("copy can see the template's commit object")
			}

			git(t, tpl, "fsck", "--strict")
			git(t, dest, "fsck", "--strict")
		})
	}
}

// TestSymlinkToOutsideStaysALink checks that a link pointing outside the tree
// is recreated as a link and that its target is neither read nor changed.
func TestSymlinkToOutsideStaysALink(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			outside := filepath.Join(t.TempDir(), "outside.txt")
			write(t, outside, "secret\n")

			link := filepath.Join(tpl, "escape")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}

			dirLink := filepath.Join(tpl, "escape-dir")
			if err := os.Symlink(filepath.Dir(outside), dirLink); err != nil {
				t.Fatal(err)
			}

			dest := filepath.Join(filepath.Dir(tpl), "copy")
			if _, err := Copy(context.Background(), tpl, dest, mode.opts); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{"escape", "escape-dir"} {
				info, err := os.Lstat(filepath.Join(dest, name))
				if err != nil {
					t.Fatal(err)
				}

				if info.Mode()&fs.ModeSymlink == 0 {
					t.Fatalf("%s is %v, want a symlink", name, info.Mode())
				}
			}

			target, err := os.Readlink(filepath.Join(dest, "escape"))
			if err != nil || target != outside {
				t.Fatalf("link target = %q, %v; want %q", target, err, outside)
			}

			if _, err := os.Stat(filepath.Join(dest, "escape-dir", "outside.txt")); err != nil {
				t.Fatalf("directory link no longer resolves: %v", err)
			}

			if got := read(t, outside); got != "secret\n" {
				t.Fatalf("outside file changed to %q", got)
			}
		})
	}
}

// TestModesAndTimesSurvive checks permission bits and mtimes are kept, and
// that a read-only directory in the template can be copied and then removed.
func TestModesAndTimesSurvive(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			when := time.Date(2020, 1, 2, 3, 4, 5, 6000, time.UTC)

			ro := filepath.Join(tpl, "ro")
			write(t, filepath.Join(ro, "f.txt"), "x\n")

			for _, p := range []string{"run.sh", "src/a.txt", "ro/f.txt"} {
				if err := os.Chtimes(filepath.Join(tpl, p), when, when); err != nil {
					t.Fatal(err)
				}
			}

			if err := os.Chmod(ro, 0o500); err != nil {
				t.Fatal(err)
			}

			dest := filepath.Join(filepath.Dir(tpl), "copy")
			t.Cleanup(func() {
				_ = os.Chmod(ro, 0o700)
				_ = os.Chmod(filepath.Join(dest, "ro"), 0o700)
			})

			if _, err := Copy(context.Background(), tpl, dest, mode.opts); err != nil {
				t.Fatal(err)
			}

			for _, p := range []string{"run.sh", "src/a.txt", "ro/f.txt", "ro"} {
				a, err := os.Lstat(filepath.Join(tpl, p))
				if err != nil {
					t.Fatal(err)
				}

				b, err := os.Lstat(filepath.Join(dest, p))
				if err != nil {
					t.Fatal(err)
				}

				if a.Mode() != b.Mode() {
					t.Errorf("%s mode %v became %v", p, a.Mode(), b.Mode())
				}

				if p != "ro" && !a.ModTime().Equal(b.ModTime()) {
					t.Errorf("%s mtime %v became %v", p, a.ModTime(), b.ModTime())
				}
			}
		})
	}
}

// TestNoFileIsShared checks that no regular file in the copy is the same
// inode as one in the template, which is what a hard link would be.
func TestNoFileIsShared(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			dest := filepath.Join(filepath.Dir(tpl), "copy")

			if _, err := Copy(context.Background(), tpl, dest, mode.opts); err != nil {
				t.Fatal(err)
			}

			err := filepath.WalkDir(tpl, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}

				rel, _ := filepath.Rel(tpl, p)
				a, _ := os.Lstat(p)

				b, err := os.Lstat(filepath.Join(dest, rel))
				if err != nil {
					return err
				}

				if os.SameFile(a, b) {
					t.Errorf("%s is shared between template and copy", rel)
				}

				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestResultDescribesCopy checks the counts and head in a Result.
func TestResultDescribesCopy(t *testing.T) {
	tpl := newRepo(t)
	dest := filepath.Join(filepath.Dir(tpl), "copy")

	res, err := Copy(context.Background(), tpl, dest, Options{})
	if err != nil {
		t.Fatal(err)
	}

	want, err := scan(tpl)
	if err != nil {
		t.Fatal(err)
	}

	if res.Files != want.files || res.Bytes != want.bytes || res.Files == 0 {
		t.Errorf("got %d files %d bytes, want %d and %d", res.Files, res.Bytes, want.files, want.bytes)
	}

	if res.Head != head(t, tpl) {
		t.Errorf("head %q, want %q", res.Head, head(t, tpl))
	}
}

// TestUnbornHead copies a repository with no commits.
func TestUnbornHead(t *testing.T) {
	isolateGit(t)

	tpl := filepath.Join(t.TempDir(), "tpl")
	if err := os.Mkdir(tpl, 0o755); err != nil {
		t.Fatal(err)
	}

	git(t, tpl, "init", "-q", "-b", "main")

	res, err := Copy(context.Background(), tpl, filepath.Join(filepath.Dir(tpl), "copy"), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if res.Head != "" {
		t.Fatalf("head = %q, want empty", res.Head)
	}
}

// TestNativeCloning needs a filesystem that can clone and skips otherwise.
func TestNativeCloning(t *testing.T) {
	tpl := newRepo(t)
	dest := filepath.Join(filepath.Dir(tpl), "copy")

	res, err := Copy(context.Background(), tpl, dest, Options{RequireCOW: true})
	if errors.Is(err, ErrNoCoW) {
		t.Skipf("the filesystem under t.TempDir() cannot clone files (clonefile or FICLONE failed): %v", err)
	}

	if err != nil {
		t.Fatal(err)
	}

	if res.Method != MethodClonefile && res.Method != MethodReflink {
		t.Fatalf("method = %q, want a clone method", res.Method)
	}
}

// TestRequireCOWFails forces cloning to be unavailable.
func TestRequireCOWFails(t *testing.T) {
	tpl := newRepo(t)
	parent := filepath.Dir(tpl)
	dest := filepath.Join(parent, "copy")

	_, err := Copy(context.Background(), tpl, dest, Options{RequireCOW: true, forceNoCoW: true})
	if !errors.Is(err, ErrNoCoW) {
		t.Fatalf("err = %v, want ErrNoCoW", err)
	}

	assertNothingLeft(t, parent, dest, "tpl")

	res, err := Copy(context.Background(), tpl, dest, Options{forceNoCoW: true})
	if err != nil {
		t.Fatal(err)
	}

	if res.Method != MethodCopy {
		t.Fatalf("method = %q, want copy", res.Method)
	}
}

// assertNothingLeft checks dest is absent and parent holds only keep.
func assertNothingLeft(t *testing.T, parent, dest string, keep ...string) {
	t.Helper()

	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("destination exists after failure: %v", err)
	}

	got := names(t, parent)
	if fmt.Sprint(got) != fmt.Sprint(keep) {
		t.Errorf("parent holds %v, want %v", got, keep)
	}
}

// TestFailureLeavesNothing fails the copy partway and after it is built, and
// checks neither the destination nor a temporary directory remains, even when
// the template has a read-only directory.
func TestFailureLeavesNothing(t *testing.T) {
	boom := errors.New("injected failure")

	cases := []struct {
		name  string
		opts  Options
		setup func()
	}{
		{"mid-walk", Options{forceWalk: true}, func() {
			n := 0
			perFileHook = func(string) error {
				if n++; n == 5 {
					return boom
				}

				return nil
			}
		}},
		{"before-rename", Options{}, func() {
			beforeRenameHook = func(string) error { return boom }
		}},
		{"before-rename-walk", Options{forceWalk: true}, func() {
			beforeRenameHook = func(string) error { return boom }
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpl := newRepo(t)
			parent := filepath.Dir(tpl)
			dest := filepath.Join(parent, "copy")

			ro := filepath.Join(tpl, "ro")
			write(t, filepath.Join(ro, "f.txt"), "x\n")

			if err := os.Chmod(ro, 0o500); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				_ = os.Chmod(ro, 0o700)

				perFileHook, beforeRenameHook = nil, nil
			})

			tc.setup()

			if _, err := Copy(context.Background(), tpl, dest, tc.opts); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}

			assertNothingLeft(t, parent, dest, "tpl")
		})
	}
}

// TestRefusals covers each way a template or destination is refused.
func TestRefusals(t *testing.T) {
	isolateGit(t)

	refused := func(t *testing.T, err error) {
		t.Helper()

		var r *RefusalError
		if !errors.As(err, &r) {
			t.Fatalf("err = %v, want a RefusalError", err)
		}
	}

	t.Run("linked worktree", func(t *testing.T) {
		tpl := newRepo(t)
		wt := filepath.Join(filepath.Dir(tpl), "wt")
		git(t, tpl, "worktree", "add", "-q", "-b", "wtb", wt)

		_, err := Copy(context.Background(), wt, filepath.Join(filepath.Dir(tpl), "out"), Options{})
		refused(t, err)
	})

	t.Run("alternates", func(t *testing.T) {
		tpl := newRepo(t)
		write(t, filepath.Join(tpl, ".git", "objects", "info", "alternates"), "/nowhere\n")

		_, err := Copy(context.Background(), tpl, filepath.Join(filepath.Dir(tpl), "out"), Options{})
		refused(t, err)
	})

	t.Run("symlinked .git", func(t *testing.T) {
		tpl := newRepo(t)
		real := filepath.Join(filepath.Dir(tpl), "realgit")

		if err := os.Rename(filepath.Join(tpl, ".git"), real); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(real, filepath.Join(tpl, ".git")); err != nil {
			t.Fatal(err)
		}

		_, err := Copy(context.Background(), tpl, filepath.Join(filepath.Dir(tpl), "out"), Options{})
		refused(t, err)
	})

	t.Run("not a repository", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "plain")
		write(t, filepath.Join(dir, "f"), "x")

		_, err := Copy(context.Background(), dir, filepath.Join(filepath.Dir(dir), "out"), Options{})
		refused(t, err)
	})

	t.Run("missing template", func(t *testing.T) {
		root := t.TempDir()

		_, err := Copy(context.Background(), filepath.Join(root, "nope"), filepath.Join(root, "out"), Options{})
		refused(t, err)
	})

	t.Run("destination exists", func(t *testing.T) {
		tpl := newRepo(t)
		dest := filepath.Join(filepath.Dir(tpl), "out")

		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := Copy(context.Background(), tpl, dest, Options{})
		refused(t, err)
	})

	t.Run("destination inside template", func(t *testing.T) {
		tpl := newRepo(t)

		_, err := Copy(context.Background(), tpl, filepath.Join(tpl, "out"), Options{})
		refused(t, err)
	})

	t.Run("template inside destination", func(t *testing.T) {
		tpl := newRepo(t)

		_, err := Copy(context.Background(), tpl, filepath.Dir(tpl), Options{})
		refused(t, err)
	})

	t.Run("destination parent missing", func(t *testing.T) {
		tpl := newRepo(t)

		_, err := Copy(context.Background(), tpl, filepath.Join(filepath.Dir(tpl), "no", "out"), Options{})
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestConcurrentCopies makes eight copies of one template at once.
func TestConcurrentCopies(t *testing.T) {
	tpl := newRepo(t)
	parent := filepath.Dir(tpl)
	want := head(t, tpl)

	const n = 8

	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			res, err := Copy(context.Background(), tpl, filepath.Join(parent, fmt.Sprintf("c%d", i)), Options{})
			if err == nil && res.Head != want {
				err = fmt.Errorf("head %q, want %q", res.Head, want)
			}

			errs[i] = err
		}()
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
	}

	// Each copy is independent of the others as well as of the template.
	for i := range n {
		write(t, filepath.Join(parent, fmt.Sprintf("c%d", i), "src", "a.txt"), fmt.Sprintf("copy %d\n", i))
	}

	for i := range n {
		if got := read(t, filepath.Join(parent, fmt.Sprintf("c%d", i), "src", "a.txt")); got != fmt.Sprintf("copy %d\n", i) {
			t.Errorf("copy %d holds %q", i, got)
		}

		git(t, filepath.Join(parent, fmt.Sprintf("c%d", i)), "fsck", "--strict")
	}

	if got := read(t, filepath.Join(tpl, "src", "a.txt")); got != "alpha\n" {
		t.Errorf("template changed to %q", got)
	}

	if len(names(t, parent)) != n+1 {
		t.Errorf("parent holds %v, want the template and %d copies", names(t, parent), n)
	}
}

// TestDestinationRaceIsRefused makes the destination appear after the checks
// and before the rename, and checks the rename does not replace it.
func TestDestinationRaceIsRefused(t *testing.T) {
	tpl := newRepo(t)
	dest := filepath.Join(filepath.Dir(tpl), "copy")

	beforeRenameHook = func(string) error { return os.Mkdir(dest, 0o755) }
	t.Cleanup(func() { beforeRenameHook = nil })

	if _, err := Copy(context.Background(), tpl, dest, Options{}); err == nil {
		t.Fatal("expected the rename to fail")
	}

	if got := names(t, dest); len(got) != 0 {
		t.Fatalf("the competing directory was replaced, now holds %v", got)
	}
}
