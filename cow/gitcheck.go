package cow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitHead returns the commit HEAD resolves to in repo, or "" when the branch
// is unborn.
//
// Git runs with the system and global config disabled and with hooks and
// fsmonitor turned off, so a hostile config or hook inside repo cannot make
// the check run anything or report something else.
func gitHead(ctx context.Context, repo string) (string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-C", repo,
		"rev-parse", "--verify", "--quiet", "HEAD",
	)
	cmd.Env = gitEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(stdout.String()), nil

	// With --quiet, exit status 1 and no message mean HEAD does not
	// resolve, which is the state of a repository with no commits.
	case errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0:
		return "", nil
	}

	return "", fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// gitEnv is the environment for every Git invocation. It drops inherited
// GIT_* variables such as GIT_DIR and GIT_CONFIG_PARAMETERS.
func gitEnv() []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}

	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}

	return env
}

// verify checks that tree is an independent repository at the same HEAD as
// the template.
func verify(ctx context.Context, tree, wantHead string) error {
	info, err := os.Lstat(filepath.Join(tree, ".git"))
	if err != nil {
		return fmt.Errorf("verifying copy: %w", err)
	}

	if !info.IsDir() {
		return errors.New("verifying copy: .git is not a real directory")
	}

	alternates := filepath.Join(tree, ".git", "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return errors.New("verifying copy: alternates file present")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("verifying copy: %w", err)
	}

	got, err := gitHead(ctx, tree)
	if err != nil {
		return fmt.Errorf("verifying copy: %w", err)
	}

	if got != wantHead {
		return fmt.Errorf("verifying copy: HEAD is %q, template has %q", got, wantHead)
	}

	return nil
}
