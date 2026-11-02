package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roasbeef/git-cow/cow"
)

// gitRun runs real git in dir and returns its output, failing on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := gitTry(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// gitTry runs real git in dir and returns its trimmed combined output.
func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// newRepo makes a repository named "main" in a fresh directory, with an
// ignored build directory, and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()

	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		t.Setenv(k, v)
	}

	dir := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		".gitignore": "build/\n", "a.txt": "a\n", "build/o": "o\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", ".gitignore", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	return dir
}

// cli runs git-cow in dir and returns its status and streams.
func cli(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()

	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"-C", dir}, args...), &out, &errb)

	return code, out.String(), errb.String()
}

// TestCopyJSONAndExitCodes checks the one-line JSON and the exit statuses.
func TestCopyJSONAndExitCodes(t *testing.T) {
	src := newRepo(t)
	parent := filepath.Dir(src)

	code, out, errOut := cli(t, parent, "copy", "--json", "main", "copy1")
	if code != 0 {
		t.Fatalf("status %d: %s", code, errOut)
	}

	if strings.Count(out, "\n") != 1 {
		t.Fatalf("output is not one line: %q", out)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"method", "files", "bytes", "elapsed_ms", "head"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in %s", key, out)
		}
	}

	if got["head"] != gitRun(t, src, "rev-parse", "HEAD") {
		t.Errorf("head = %v", got["head"])
	}

	if code, out, _ := cli(t, parent, "copy", "main", "copy2"); code != 0 || !strings.HasPrefix(out, "copied ") {
		t.Errorf("human output: status %d, %q", code, out)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"destination exists", []string{"copy", "main", "copy1"}, 2},
		{"missing template", []string{"copy", "nope", "copy3"}, 2},
		{"missing destination parent", []string{"copy", "main", "no/copy4"}, 1},
		{"wrong argument count", []string{"copy", "main"}, 1},
		{"bad flag", []string{"copy", "--nope", "main", "x"}, 1},
		{"unknown command", []string{"frobnicate"}, 1},
	}

	for _, tc := range cases {
		if code, _, _ := cli(t, parent, tc.args...); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
}

// TestNoCoWExitStatus checks ErrNoCoW maps to 3 and a refusal to 2.
func TestNoCoWExitStatus(t *testing.T) {
	var stderr bytes.Buffer

	if got := fail(&stderr, fmt.Errorf("wrapped: %w", cow.ErrNoCoW)); got != exitNoCoW {
		t.Errorf("ErrNoCoW maps to %d", got)
	}

	if got := fail(&stderr, errors.New("other")); got != exitError {
		t.Errorf("other error maps to %d", got)
	}
}

// TestAddInteroperatesWithGit makes worktrees with git-cow and with git and
// manages each with the other.
func TestAddInteroperatesWithGit(t *testing.T) {
	src := newRepo(t)
	parent := filepath.Dir(src)

	code, out, errOut := cli(t, src, "add", "--json", "-b", "feat", "../cow-made")
	if code != 0 {
		t.Fatalf("add: status %d: %s", code, errOut)
	}

	var added struct {
		Path   string `json:"path"`
		Method string `json:"method"`
	}

	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatal(err)
	}

	t.Logf("add used method %q", added.Method)

	if got := added.Method; got == cow.MethodWorktree {
		t.Skip("the filesystem under t.TempDir() cannot clone files; add fell back to plain git")
	}

	if body, err := os.ReadFile(filepath.Join(added.Path, "build", "o")); err != nil || string(body) != "o\n" {
		t.Errorf("ignored artifact: %q, %v", body, err)
	}

	// git sees what git-cow made, and git-cow list matches git worktree list.
	gitList := gitRun(t, src, "worktree", "list", "--porcelain")
	if !strings.Contains(gitList, "worktree "+added.Path+"\n") {
		t.Fatalf("git does not list the worktree:\n%s", gitList)
	}

	if code, out, _ := cli(t, src, "list", "--porcelain"); code != 0 || strings.TrimSpace(out) != gitList {
		t.Errorf("list differs from git worktree list:\n%s\nvs\n%s", out, gitList)
	}

	// git-cow sees what git made.
	gitRun(t, src, "worktree", "add", "-q", "-b", "gitmade", filepath.Join(parent, "git-made"))

	if _, out, _ := cli(t, src, "list"); !strings.Contains(out, "git-made") || !strings.Contains(out, "cow-made") {
		t.Errorf("list misses a worktree:\n%s", out)
	}

	// git removes the git-cow worktree, which holds no untracked files.
	gitRun(t, src, "worktree", "remove", added.Path)

	if _, err := os.Lstat(added.Path); err == nil {
		t.Error("git worktree remove left the directory")
	}

	// git-cow removes the git worktree.
	if code, _, errOut := cli(t, src, "remove", "../git-made"); code != 0 {
		t.Fatalf("remove: %d %s", code, errOut)
	}

	if out := gitRun(t, src, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktrees remain:\n%s", out)
	}
}

// TestRemoveRefusesUntrackedUnlessForced checks git-cow remove has git's
// rule: a worktree holding an untracked file needs --force.
func TestRemoveRefusesUntrackedUnlessForced(t *testing.T) {
	src := newRepo(t)

	if code, _, errOut := cli(t, src, "add", "-b", "feat", "../wt"); code != 0 {
		t.Fatal(errOut)
	}

	if err := os.WriteFile(filepath.Join(filepath.Dir(src), "wt", "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := cli(t, src, "remove", "../wt"); code == 0 {
		t.Fatal("remove succeeded with an untracked file")
	}

	if code, _, errOut := cli(t, src, "remove", "--force", "../wt"); code != 0 {
		t.Fatalf("remove --force: %d %s", code, errOut)
	}
}

// TestPruneInteroperates deletes worktree directories by hand and prunes
// them with the other tool.
func TestPruneInteroperates(t *testing.T) {
	src := newRepo(t)
	parent := filepath.Dir(src)

	if code, _, errOut := cli(t, src, "add", "-b", "feat", "../cow-made"); code != 0 {
		t.Fatal(errOut)
	}

	gitRun(t, src, "worktree", "add", "-q", "-b", "gitmade", filepath.Join(parent, "git-made"))

	for _, d := range []string{"cow-made", "git-made"} {
		if err := os.RemoveAll(filepath.Join(parent, d)); err != nil {
			t.Fatal(err)
		}
	}

	if out := gitRun(t, src, "worktree", "list", "--porcelain"); !strings.Contains(out, "prunable") {
		t.Fatalf("git does not mark them prunable:\n%s", out)
	}

	if code, _, errOut := cli(t, src, "prune"); code != 0 {
		t.Fatalf("prune: %d %s", code, errOut)
	}

	if out := gitRun(t, src, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktrees remain after prune:\n%s", out)
	}

	// And the reverse: git prunes a git-cow worktree.
	if code, _, errOut := cli(t, src, "add", "-b", "again", "../again"); code != 0 {
		t.Fatal(errOut)
	}

	if err := os.RemoveAll(filepath.Join(parent, "again")); err != nil {
		t.Fatal(err)
	}

	gitRun(t, src, "worktree", "prune")

	if out := gitRun(t, src, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktrees remain after git prune:\n%s", out)
	}
}

// TestAddExitStatuses checks refusals and Git failures.
func TestAddExitStatuses(t *testing.T) {
	src := newRepo(t)

	// A branch checked out elsewhere is refused by Git with its own status.
	_, _, _ = cli(t, src, "add", "-b", "feat", "../ok")

	code, _, _ := cli(t, src, "add", "../again", "feat")
	if code == 0 || code == exitRefused || code == exitNoCoW {
		t.Errorf("checked-out branch: status %d, want git's own failure status", code)
	}

	if code, _, _ := cli(t, src, "add", "-b", "other", "--nonsense", "../x"); code != exitError {
		t.Errorf("unknown option: status %d", code)
	}

	// A dirty source is refused with status 2.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := cli(t, src, "add", "-b", "dirty", "../dirty"); code != exitRefused {
		t.Errorf("dirty source: status %d (%s)", code, errOut)
	}
}
