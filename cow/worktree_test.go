package cow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addTo runs AddWorktree from src with the given options and a destination
// named dir beside it.
func addTo(t *testing.T, src, dir string, opts AddOptions) (AddResult, error) {
	t.Helper()

	opts.Dir = src
	opts.Path = filepath.Join(filepath.Dir(src), dir)

	return AddWorktree(context.Background(), opts)
}

// listed reports whether git lists path as a worktree of repo.
func listed(t *testing.T, repo, path string) bool {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err == nil {
		path = filepath.Join(resolved, filepath.Base(path))
	}

	return strings.Contains(git(t, repo, "worktree", "list", "--porcelain"), "worktree "+path+"\n")
}

// addModes are the strategies a worktree test runs under.
var addModes = []struct {
	name string
	opts AddOptions
}{
	{"default", AddOptions{}},
	{"walk", AddOptions{forceWalk: true}},
}

// TestAddCarriesUntrackedFiles checks that ignored and untracked files come
// along, the worktree is registered with Git and is clean, and a write on
// one side stays on that side.
func TestAddCarriesUntrackedFiles(t *testing.T) {
	requireClone(t)

	for _, mode := range addModes {
		t.Run(mode.name, func(t *testing.T) {
			src := newRepo(t)
			write(t, filepath.Join(src, "build", "out.o"), "object\n")
			write(t, filepath.Join(src, "notes.txt"), "untracked\n")

			opts := mode.opts
			opts.Branch = "feat"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			if res.Method == MethodWorktree || res.Files == 0 {
				t.Fatalf("result %+v does not describe a clone", res)
			}

			if got := read(t, filepath.Join(res.Path, "build", "out.o")); got != "object\n" {
				t.Errorf("ignored artifact holds %q", got)
			}

			if got := read(t, filepath.Join(res.Path, "notes.txt")); got != "untracked\n" {
				t.Errorf("untracked file holds %q", got)
			}

			if !listed(t, src, res.Path) {
				t.Errorf("git does not list %s:\n%s", res.Path, git(t, src, "worktree", "list"))
			}

			if got := git(t, res.Path, "branch", "--show-current"); got != "feat" {
				t.Errorf("branch = %q", got)
			}

			if got := git(t, res.Path, "status", "--porcelain"); got != "?? notes.txt" {
				t.Errorf("status = %q, want only the untracked file", got)
			}

			if res.Head != head(t, src) {
				t.Errorf("head %s, want %s", res.Head, head(t, src))
			}

			// Writes stay on their own side.
			write(t, filepath.Join(res.Path, "src", "a.txt"), "in worktree\n")
			write(t, filepath.Join(res.Path, "build", "out.o"), "rebuilt\n")

			if read(t, filepath.Join(src, "src", "a.txt")) != "alpha\n" ||
				read(t, filepath.Join(src, "build", "out.o")) != "object\n" {

				t.Error("a write in the worktree reached the source")
			}

			// The Git directory is shared by design: a branch made in the
			// worktree is visible from the source.
			git(t, res.Path, "branch", "made-in-wt")
			git(t, src, "rev-parse", "--verify", "made-in-wt")
		})
	}
}

// TestAddMatchesCommitish creates a worktree at an older commit and checks the
// tracked files are those of that commit, as git worktree add gives, while the
// untracked file from the source is kept.
func TestAddMatchesCommitish(t *testing.T) {
	requireClone(t)

	for _, mode := range addModes {
		t.Run(mode.name, func(t *testing.T) {
			src := newRepo(t)
			write(t, filepath.Join(src, "notes.txt"), "untracked\n")

			opts := mode.opts
			opts.Branch = "old"
			opts.Commitish = "HEAD~2"

			res, err := addTo(t, src, "wt", opts)
			if err != nil {
				t.Fatal(err)
			}

			want := git(t, src, "rev-parse", "HEAD~2")
			if res.Head != want {
				t.Fatalf("head %s, want %s", res.Head, want)
			}

			// HEAD~2 has the first version of README, no c.txt, and no
			// gone.txt either (it was added in the second commit).
			if got := read(t, filepath.Join(res.Path, "README.md")); got != "one\n" {
				t.Errorf("README.md = %q", got)
			}

			if _, err := os.Lstat(filepath.Join(res.Path, "src", "c.txt")); err == nil {
				t.Error("c.txt exists but is not in the commit")
			}

			if got := git(t, res.Path, "status", "--porcelain"); got != "?? notes.txt" {
				t.Errorf("status = %q", got)
			}

			// A plain checkout of the same commit has the same tracked files.
			plain := filepath.Join(filepath.Dir(src), "plain")
			git(t, src, "worktree", "add", "-q", "--detach", plain, "HEAD~2")

			a := git(t, res.Path, "ls-files", "-s")
			b := git(t, plain, "ls-files", "-s")
			if a != b {
				t.Errorf("index differs from a plain checkout:\n%s\n%s", a, b)
			}
		})
	}
}

// TestAddDetachedDefaultsToHead covers --detach without a commit-ish.
func TestAddDetachedDefaultsToHead(t *testing.T) {
	src := newRepo(t)

	res, err := addTo(t, src, "wt", AddOptions{Detach: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := git(t, res.Path, "branch", "--show-current"); got != "" {
		t.Errorf("branch = %q, want detached", got)
	}

	if res.Head != head(t, src) {
		t.Errorf("head %s, want %s", res.Head, head(t, src))
	}
}

// TestAddResetBranch covers -B on an existing branch.
func TestAddResetBranch(t *testing.T) {
	src := newRepo(t)
	git(t, src, "branch", "topic", "HEAD~1")

	res, err := addTo(t, src, "wt", AddOptions{ResetBranch: "topic"})
	if err != nil {
		t.Fatal(err)
	}

	if got := git(t, src, "rev-parse", "topic"); got != head(t, src) {
		t.Errorf("topic at %s, want %s", got, head(t, src))
	}

	if got := git(t, res.Path, "status", "--porcelain"); got != "" {
		t.Errorf("status = %q", got)
	}
}

// TestAddRefusesDirtySource defines dirty as staged or unstaged changes to
// tracked files and checks both are refused with nothing left behind, while
// untracked files are fine.
func TestAddRefusesDirtySource(t *testing.T) {
	cases := map[string]func(t *testing.T, src string){
		"unstaged": func(t *testing.T, src string) {
			write(t, filepath.Join(src, "src", "a.txt"), "edited\n")
		},
		"staged": func(t *testing.T, src string) {
			write(t, filepath.Join(src, "src", "a.txt"), "edited\n")
			git(t, src, "add", "src/a.txt")
		},
		"deleted": func(t *testing.T, src string) {
			if err := os.Remove(filepath.Join(src, "src", "a.txt")); err != nil {
				t.Fatal(err)
			}
		},
		"new staged file": func(t *testing.T, src string) {
			write(t, filepath.Join(src, "added.txt"), "x\n")
			git(t, src, "add", "added.txt")
		},
	}

	for name, dirty := range cases {
		t.Run(name, func(t *testing.T) {
			src := newRepo(t)
			dirty(t, src)

			_, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})

			var r *RefusalError
			if !errors.As(err, &r) || !strings.Contains(err.Error(), "uncommitted") {
				t.Fatalf("err = %v, want a refusal about uncommitted changes", err)
			}

			if _, err := os.Lstat(filepath.Join(filepath.Dir(src), "wt")); err == nil {
				t.Error("destination was created")
			}

			if _, err := gitErr(src, "rev-parse", "--verify", "feat"); err == nil {
				t.Error("branch was created")
			}
		})
	}
}

// TestAddBranchCheckedOutElsewhere checks Git's own refusal is kept, and that
// --force passes through as Git defines it.
func TestAddBranchCheckedOutElsewhere(t *testing.T) {
	src := newRepo(t)

	_, err := addTo(t, src, "wt", AddOptions{Commitish: "main"})

	var ge *GitError
	if !errors.As(err, &ge) || ge.ExitCode == 0 {
		t.Fatalf("err = %v, want a GitError", err)
	}

	if _, err := os.Lstat(filepath.Join(filepath.Dir(src), "wt")); err == nil {
		t.Error("destination was created")
	}

	// The same refusal from real Git, for comparison.
	_, want := gitErr(src, "worktree", "add", "-q", filepath.Join(filepath.Dir(src), "plain"), "main")
	if want == nil {
		t.Fatal("real git accepted a branch checked out elsewhere")
	}

	if _, err := addTo(t, src, "forced", AddOptions{Commitish: "main", Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

// TestAddLeavesSubmoduleEmpty checks a submodule is left as an empty
// directory, as git worktree add leaves it, and its clone is not touched.
func TestAddLeavesSubmoduleEmpty(t *testing.T) {
	src := newRepo(t)
	sub := newRepo(t)

	git(t, src, "submodule", "add", "-q", sub, "sm")
	git(t, src, "commit", "-q", "-m", "add submodule")

	res, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	entries := names(t, filepath.Join(res.Path, "sm"))
	if len(entries) != 0 {
		t.Errorf("submodule directory holds %v, want it empty", entries)
	}

	if _, err := os.Lstat(filepath.Join(src, "sm", ".git")); err != nil {
		t.Errorf("the source's submodule was disturbed: %v", err)
	}

	if got := git(t, res.Path, "status", "--porcelain", "--ignore-submodules=none"); got != "D sm" &&
		got != " D sm" && got != "" {

		t.Errorf("status = %q", got)
	}

	plain := filepath.Join(filepath.Dir(src), "plain")
	git(t, src, "worktree", "add", "-q", "--detach", plain)

	if got := names(t, filepath.Join(plain, "sm")); len(got) != 0 {
		t.Errorf("real git leaves %v in the submodule directory; the assumption behind this test is wrong", got)
	}
}

// TestAddWithoutCloningSupport checks the fallback to plain git worktree add
// and the failure with RequireCOW.
func TestAddWithoutCloningSupport(t *testing.T) {
	src := newRepo(t)
	write(t, filepath.Join(src, "notes.txt"), "untracked\n")

	_, err := addTo(t, src, "required", AddOptions{forceNoCoW: true, RequireCOW: true, Branch: "a"})
	if !errors.Is(err, ErrNoCoW) {
		t.Fatalf("err = %v, want ErrNoCoW", err)
	}

	if _, err := os.Lstat(filepath.Join(filepath.Dir(src), "required")); err == nil {
		t.Error("destination was created")
	}

	if _, err := gitErr(src, "rev-parse", "--verify", "a"); err == nil {
		t.Error("branch was created")
	}

	res, err := addTo(t, src, "plain", AddOptions{forceNoCoW: true, Branch: "b"})
	if err != nil {
		t.Fatal(err)
	}

	if res.Method != MethodWorktree {
		t.Errorf("method = %q", res.Method)
	}

	if _, err := os.Lstat(filepath.Join(res.Path, "notes.txt")); err == nil {
		t.Error("the plain fallback copied an untracked file")
	}

	if !listed(t, src, res.Path) {
		t.Error("git does not list the plain worktree")
	}
}

// TestAddFailureRollsBack fails the population and checks the worktree is
// unregistered, the directory gone and the new branch deleted.
func TestAddFailureRollsBack(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	boom := errors.New("injected failure")

	n := 0
	perFileHook = func(string) error {
		if n++; n == 3 {
			return boom
		}

		return nil
	}

	t.Cleanup(func() { perFileHook = nil })

	_, err := addTo(t, src, "wt", AddOptions{forceWalk: true, Branch: "feat"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}

	if _, err := os.Lstat(filepath.Join(filepath.Dir(src), "wt")); err == nil {
		t.Error("destination remains")
	}

	if out := git(t, src, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("a worktree remains registered:\n%s", out)
	}

	if _, err := gitErr(src, "rev-parse", "--verify", "feat"); err == nil {
		t.Error("branch remains")
	}
}

// TestAddRefusesDestinationInsideSource checks the walk cannot copy into itself.
func TestAddRefusesDestinationInsideSource(t *testing.T) {
	src := newRepo(t)

	_, err := AddWorktree(context.Background(), AddOptions{Dir: src, Path: "inside"})

	var r *RefusalError
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a RefusalError", err)
	}
}

// TestAddFromLinkedWorktree uses a linked worktree as the source.
func TestAddFromLinkedWorktree(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	first := filepath.Join(filepath.Dir(src), "first")
	git(t, src, "worktree", "add", "-q", "-b", "one", first)
	write(t, filepath.Join(first, "build", "x.o"), "x\n")

	res, err := AddWorktree(context.Background(), AddOptions{
		Dir: first, Path: filepath.Join(filepath.Dir(src), "second"), Branch: "two",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(res.Path, "build", "x.o")); got != "x\n" {
		t.Errorf("artifact = %q", got)
	}

	if !listed(t, src, res.Path) {
		t.Error("the main checkout does not list the new worktree")
	}
}

// TestAddScansOnceWhenTreeCloneUnsupported checks that after the first
// whole-tree clone fails as unsupported, no further directory is scanned for
// one. On Linux every directory would otherwise be walked an extra time.
func TestAddScansOnceWhenTreeCloneUnsupported(t *testing.T) {
	requireClone(t)

	src := newRepo(t)

	calls := 0
	cloneTree = func(string, string) error {
		calls++

		return errNoCoW
	}

	t.Cleanup(func() { cloneTree = wholeTreeClone })

	if _, err := addTo(t, src, "wt", AddOptions{Branch: "feat"}); err != nil {
		t.Fatal(err)
	}

	want := 0
	if hasWholeTreeClone {
		want = 1
	}

	if calls != want {
		t.Errorf("whole-tree clone attempted %d times, want %d", calls, want)
	}
}

// ageFiles moves the modification time of every tracked file an hour into the
// past and refreshes the index. Files written moments ago are "racily clean"
// to Git, which then rehashes them and hides a stat mismatch; files of a
// settled checkout are not.
func ageFiles(t *testing.T, repo string) {
	t.Helper()

	past := time.Now().Add(-time.Hour)

	for _, f := range strings.Fields(git(t, repo, "ls-files")) {
		if err := os.Chtimes(filepath.Join(repo, f), past, past); err != nil {
			t.Fatal(err)
		}
	}

	git(t, repo, "update-index", "--refresh", "-q")
	git(t, repo, "status", "--porcelain")
}

// TestAddKeepsClonesUnrewritten checks the tracked files in the new worktree
// keep the source's modification time. A rewrite by the final reset would
// give them the time of the add, and would copy their blocks.
func TestAddKeepsClonesUnrewritten(t *testing.T) {
	requireClone(t)

	for _, mode := range addModes {
		t.Run(mode.name, func(t *testing.T) {
			src := newRepo(t)
			ageFiles(t, src)

			res, err := addTo(t, src, "wt", AddOptions{Branch: "feat", forceWalk: mode.opts.forceWalk})
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range []string{"README.md", "src/a.txt", "run.sh"} {
				a, err := os.Stat(filepath.Join(src, f))
				if err != nil {
					t.Fatal(err)
				}

				b, err := os.Stat(filepath.Join(res.Path, f))
				if err != nil {
					t.Fatal(err)
				}

				if !a.ModTime().Equal(b.ModTime()) {
					t.Errorf("%s has mtime %v in the worktree and %v in the source", f, b.ModTime(), a.ModTime())
				}
			}
		})
	}
}

// TestAddRollbackLeavesOtherWorktrees checks a failed add does not prune the
// record of an unrelated worktree whose directory is missing.
func TestAddRollbackLeavesOtherWorktrees(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	stale := filepath.Join(filepath.Dir(src), "stale")

	git(t, src, "worktree", "add", "-q", "-b", "stale", stale)

	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	n := 0
	perFileHook = func(string) error {
		if n++; n == 2 {
			return errors.New("injected failure")
		}

		return nil
	}

	t.Cleanup(func() { perFileHook = nil })

	if _, err := addTo(t, src, "wt", AddOptions{forceWalk: true, Branch: "feat"}); err == nil {
		t.Fatal("expected a failure")
	}

	if out := git(t, src, "worktree", "list", "--porcelain"); !strings.Contains(out, "stale") {
		t.Errorf("the unrelated worktree record was pruned:\n%s", out)
	}
}

// TestAddSplitIndex checks AddWorktree works when core.splitIndex is enabled
// and the source index references a sharedindex.<sha> base file.
func TestAddSplitIndex(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	git(t, src, "config", "core.splitIndex", "true")
	git(t, src, "update-index", "--split-index")

	write(t, filepath.Join(src, "split.txt"), "split\n")
	git(t, src, "add", "split.txt")
	git(t, src, "commit", "-q", "-m", "with split index")

	res, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	if got := git(t, res.Path, "status", "--porcelain"); got != "" {
		t.Errorf("status = %q, want clean", got)
	}

	if got := read(t, filepath.Join(res.Path, "split.txt")); got != "split\n" {
		t.Errorf("split.txt = %q", got)
	}
}

// TestAddDirtySubmoduleAllowed checks that modified or untracked files inside
// a submodule worktree in the source do not cause AddWorktree to refuse.
func TestAddDirtySubmoduleAllowed(t *testing.T) {
	src := newRepo(t)
	sub := newRepo(t)

	git(t, src, "submodule", "add", "-q", sub, "sm")
	git(t, src, "commit", "-q", "-m", "add submodule")

	write(t, filepath.Join(src, "sm", "README.md"), "dirty inside submodule\n")
	write(t, filepath.Join(src, "sm", "untracked.o"), "artifact\n")

	res, err := addTo(t, src, "wt", AddOptions{Branch: "feat"})
	if err != nil {
		t.Fatal(err)
	}

	if entries := names(t, filepath.Join(res.Path, "sm")); len(entries) != 0 {
		t.Errorf("submodule directory holds %v, want empty", entries)
	}
}

// TestAddRollbackCleansReadOnlyAndImplicitBranch checks that a failed add
// removes read-only directories in the half-made worktree as well as the
// branch Git created from the destination basename when -b was not given.
func TestAddRollbackCleansReadOnlyAndImplicitBranch(t *testing.T) {
	requireClone(t)

	src := newRepo(t)
	ro := filepath.Join(src, "ro_cache", "pkg")
	write(t, filepath.Join(ro, "mod.go"), "package pkg\n")

	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(ro, 0o700)
		perFileHook = nil
	})

	n := 0
	perFileHook = func(string) error {
		if n++; n == 4 {
			return errors.New("injected failure")
		}

		return nil
	}

	// Omit Branch so Git creates a branch named "implicit-wt" from the path.
	if _, err := addTo(t, src, "implicit-wt", AddOptions{forceWalk: true}); err == nil {
		t.Fatal("expected a failure")
	}

	if _, err := os.Lstat(filepath.Join(filepath.Dir(src), "implicit-wt")); err == nil {
		t.Error("destination directory remains after rollback")
	}

	if _, err := gitErr(src, "rev-parse", "--verify", "implicit-wt"); err == nil {
		t.Error("implicit branch remains after rollback")
	}

	// A pre-existing branch checked out by basename DWIM must NOT be deleted on rollback.
	git(t, src, "branch", "existing-wt")
	n = 0
	if _, err := addTo(t, src, "existing-wt", AddOptions{forceWalk: true, Lock: true}); err == nil {
		t.Fatal("expected a failure")
	}

	if _, err := gitErr(src, "rev-parse", "--verify", "existing-wt"); err != nil {
		t.Errorf("pre-existing branch was deleted on rollback: %v", err)
	}
}
