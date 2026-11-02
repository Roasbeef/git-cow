package cow

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGit gives the test a Git that reads no user or system config and
// has a fixed identity, so results do not depend on the developer's setup.
func isolateGit(t testing.TB) {
	t.Helper()

	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",

		// Lets the submodule test clone from a local path.
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "protocol.file.allow",
		"GIT_CONFIG_VALUE_0": "always",
	} {
		t.Setenv(k, v)
	}
}

// git runs git in dir and returns its trimmed output, failing the test on error.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()

	out, err := gitErr(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// gitErr runs git in dir and returns its combined, trimmed output.
func gitErr(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir

	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()

	return strings.TrimSpace(buf.String()), err
}

// write creates a file with the given content, making parent directories.
func write(t testing.TB, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// read returns a file's content.
func read(t testing.TB, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// newRepo creates a non-bare repository named "tpl" under a fresh temporary
// directory and returns its path. It has three commits, an executable file,
// nested directories and a .git/hooks file of its own.
func newRepo(t testing.TB) string {
	t.Helper()
	isolateGit(t)

	dir := filepath.Join(t.TempDir(), "tpl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	git(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "README.md"), "one\n")
	write(t, filepath.Join(dir, ".gitignore"), "build/\n")
	write(t, filepath.Join(dir, "src", "a.txt"), "alpha\n")
	write(t, filepath.Join(dir, "src", "deep", "b.txt"), "beta\n")
	write(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\necho hi\n")

	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")

	write(t, filepath.Join(dir, "README.md"), "two\n")
	write(t, filepath.Join(dir, "gone.txt"), "to be removed\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "second")

	git(t, dir, "rm", "-q", "gone.txt")
	write(t, filepath.Join(dir, "src", "c.txt"), "gamma\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "third")

	write(t, filepath.Join(dir, ".git", "hooks", "pre-commit.sample2"), "#!/bin/sh\nexit 0\n")

	return dir
}

// copyModes are the strategies every copy test runs under.
var copyModes = []struct {
	name string
	opts Options
}{
	{"default", Options{}},
	{"walk", Options{forceWalk: true}},
	{"plain", Options{forceNoCoW: true}},
}

// head returns HEAD of the repository at dir.
func head(t testing.TB, dir string) string {
	t.Helper()

	return git(t, dir, "rev-parse", "HEAD")
}

// names lists the entries of dir.
func names(t testing.TB, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}

	return out
}

// requireClone skips the test when the filesystem under t.TempDir() cannot
// clone files, because the test checks behaviour of the cloning path. Without
// cloning, add falls back to plain git worktree add, which other tests cover.
func requireClone(t testing.TB) {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")

	if err := os.WriteFile(src, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cloneFile(src, filepath.Join(dir, "dst")); err != nil {
		if isUnsupported(err) {
			t.Skipf("the filesystem under t.TempDir() cannot clone files: %v", err)
		}

		t.Fatal(err)
	}
}
