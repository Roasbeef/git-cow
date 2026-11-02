package cow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MethodWorktree is the method reported when the filesystem could not clone
// and the worktree was made by plain "git worktree add".
const MethodWorktree = "worktree"

// GitError is a failed Git invocation. Its exit status is Git's own.
type GitError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

// Error implements the error interface.
func (e *GitError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}

	return fmt.Sprintf("git %s failed with status %d", strings.Join(e.Args, " "), e.ExitCode)
}

// AddOptions are the arguments of AddWorktree. They mirror "git worktree add".
type AddOptions struct {
	// Dir is the directory the command runs in. Its checkout is the source
	// whose files are cloned. Empty means the current directory.
	Dir string

	// Path is where the new worktree goes, relative to Dir.
	Path string

	// Commitish is the commit, branch or tag to check out. Empty means HEAD.
	Commitish string

	// Branch is the name for "-b", a new branch.
	Branch string

	// ResetBranch is the name for "-B", a new or reset branch.
	ResetBranch string

	// Detach is "--detach".
	Detach bool

	// Force is "--force", which Git uses to allow a path that is a missing
	// registered worktree or a branch checked out elsewhere.
	Force bool

	// RequireCOW makes AddWorktree fail with ErrNoCoW instead of falling
	// back to plain "git worktree add".
	RequireCOW bool

	// The fields below let the package tests force paths that the host
	// filesystem would not otherwise take.
	forceWalk  bool
	forceNoCoW bool
}

// AddResult describes a created worktree.
type AddResult struct {
	// Method is MethodClonefile, MethodReflink, MethodCopy or
	// MethodWorktree.
	Method string

	// Path is the absolute path of the new worktree.
	Path string

	// Files and Bytes count what was cloned from the source checkout.
	Files int
	Bytes int64

	// Elapsed is the wall time of the whole operation.
	Elapsed time.Duration

	// Head is the commit the new worktree is at.
	Head string
}

// AddWorktree creates a linked worktree at opts.Path whose files are
// copy-on-write clones of the checkout containing opts.Dir.
//
// Git's own metadata is made by "git worktree add --no-checkout", so Git's
// refusals (a branch checked out elsewhere, an existing path) and its
// "list", "remove" and "prune" all apply unchanged. The working tree is then
// filled by cloning the source checkout, so untracked and ignored files such
// as build output come along, and made to match the requested commit the way
// a checkout would.
//
// The source must have no staged or unstaged changes to tracked files. That
// is the one state in which its files are exactly its HEAD commit, so the
// result can be made to match any commit-ish with a hard reset that rewrites
// only the files which differ. Untracked files are copied and are not
// protected from that reset: a commit-ish that tracks a path where the source
// has an untracked file replaces it.
//
// A submodule is left as an empty directory, as "git worktree add" leaves it.
func AddWorktree(ctx context.Context, opts AddOptions) (AddResult, error) {
	start := time.Now()

	src, err := gitOut(ctx, opts.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return AddResult{}, refusef("not inside a Git checkout: %v", err)
	}

	dest := opts.Path
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(absDir(opts.Dir), dest)
	}

	dest = filepath.Clean(dest)

	if within(src, dest) {
		return AddResult{}, refusef("destination %q is inside the source checkout", opts.Path)
	}

	if err := requireClean(ctx, src); err != nil {
		return AddResult{}, err
	}

	srcHead, err := gitOut(ctx, src, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, refusef("the source checkout has no commits")
	}

	supported, err := probeCoW(ctx, src, dest, opts.forceNoCoW)
	if err != nil {
		return AddResult{}, err
	}

	if !supported {
		if opts.RequireCOW {
			return AddResult{}, fmt.Errorf("%w: the filesystem cannot clone files", ErrNoCoW)
		}

		return addPlain(ctx, src, dest, opts, start)
	}

	if err := runGit(ctx, src, addArgs(opts, dest, true)...); err != nil {
		return AddResult{}, err
	}

	res, err := populateWorktree(ctx, src, srcHead, dest, opts)
	if err != nil {
		rollbackAdd(src, dest, opts)

		return AddResult{}, err
	}

	res.Elapsed = time.Since(start)

	return res, nil
}

// populateWorktree fills the empty worktree dest from src and checks the
// result against its HEAD.
func populateWorktree(ctx context.Context, src, srcHead, dest string,
	opts AddOptions) (AddResult, error) {

	gitlinks, err := gitlinkPaths(ctx, src)
	if err != nil {
		return AddResult{}, err
	}

	w := &walker{
		opts:        Options{RequireCOW: opts.RequireCOW, forceNoCoW: opts.forceNoCoW},
		skip:        map[string]struct{}{".git": {}},
		skipSpecial: true,
		treeClone:   !opts.forceWalk && !opts.forceNoCoW,
	}

	for _, p := range gitlinks {
		w.skip[p] = struct{}{}
	}

	if err := w.populate(ctx, src, dest, ""); err != nil {
		return AddResult{}, err
	}

	// The source index describes exactly these files, so copying it gives
	// the new worktree stat data to compare against and spares the first
	// "git status" from hashing every tracked file. The clones share the
	// source's mtime and size but not its inode or ctime, so the refresh
	// compares only the first two and then stores the clones' own values.
	if err := copyIndex(ctx, src, dest); err != nil {
		return AddResult{}, err
	}

	_ = runGit(ctx, dest, "-c", "core.checkStat=minimal", "-c", "core.trustctime=false",
		"update-index", "--refresh", "-q")

	// With the index at the source's HEAD, a hard reset to the worktree's
	// own HEAD rewrites exactly the files that differ between the two
	// commits, and removes the ones the target commit does not have.
	if err := runGit(ctx, dest, "reset", "--hard", "-q"); err != nil {
		return AddResult{}, err
	}

	status, err := gitOut(ctx, dest, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return AddResult{}, err
	}

	if status != "" {
		return AddResult{}, fmt.Errorf("new worktree does not match its commit:\n%s", status)
	}

	head, err := gitOut(ctx, dest, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, err
	}

	method := MethodReflink
	switch {
	case w.copied > 0:
		method = MethodCopy

	case w.wholeTrees > 0:
		method = MethodClonefile
	}

	return AddResult{Method: method, Path: dest, Files: w.files, Bytes: w.bytes, Head: head}, nil
}

// addPlain makes the worktree with plain Git, for filesystems that cannot
// clone.
func addPlain(ctx context.Context, src, dest string, opts AddOptions,
	start time.Time) (AddResult, error) {

	if err := runGit(ctx, src, addArgs(opts, dest, false)...); err != nil {
		return AddResult{}, err
	}

	head, err := gitOut(ctx, dest, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, err
	}

	return AddResult{Method: MethodWorktree, Path: dest, Elapsed: time.Since(start), Head: head}, nil
}

// addArgs builds the "git worktree add" command line.
func addArgs(opts AddOptions, dest string, noCheckout bool) []string {
	args := []string{"worktree", "add"}

	if noCheckout {
		args = append(args, "--no-checkout")
	}

	if opts.Detach {
		args = append(args, "--detach")
	}

	if opts.Force {
		args = append(args, "--force")
	}

	if opts.Branch != "" {
		args = append(args, "-b", opts.Branch)
	}

	if opts.ResetBranch != "" {
		args = append(args, "-B", opts.ResetBranch)
	}

	args = append(args, dest)

	if opts.Commitish != "" {
		args = append(args, opts.Commitish)
	}

	return args
}

// rollbackAdd undoes a half-made worktree and the branch created for it.
func rollbackAdd(src, dest string, opts AddOptions) {
	ctx := context.Background()

	_ = runGit(ctx, src, "worktree", "remove", "--force", dest)
	_ = os.RemoveAll(dest)
	_ = runGit(ctx, src, "worktree", "prune")

	if opts.Branch != "" {
		_ = runGit(ctx, src, "branch", "-D", opts.Branch)
	}
}

// requireClean refuses a source whose tracked files differ from HEAD.
func requireClean(ctx context.Context, src string) error {
	status, err := gitOut(ctx, src, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return err
	}

	if status != "" {
		return refusef("the source checkout has uncommitted changes to tracked files; "+
			"commit or stash them, or use \"git worktree add\":\n%s", status)
	}

	return nil
}

// gitlinkPaths lists the submodule paths recorded in src's index.
func gitlinkPaths(ctx context.Context, src string) ([]string, error) {
	out, err := gitOut(ctx, src, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}

	var paths []string

	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if ok && strings.HasPrefix(meta, "160000 ") {
			paths = append(paths, p)
		}
	}

	return paths, nil
}

// copyIndex copies src's index file over dest's.
func copyIndex(ctx context.Context, src, dest string) error {
	from, err := gitOut(ctx, src, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return err
	}

	to, err := gitOut(ctx, dest, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return err
	}

	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}

	return os.WriteFile(to, data, 0o644)
}

// probeCoW reports whether files from src's Git directory can be cloned to
// the place dest will go, by cloning one small file there and removing it.
func probeCoW(ctx context.Context, src, dest string, forceNo bool) (bool, error) {
	if forceNo {
		return false, nil
	}

	common, err := gitOut(ctx, src, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, err
	}

	dir := filepath.Dir(dest)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}

		dir = filepath.Dir(dir)
	}

	probe := filepath.Join(dir, fmt.Sprintf(".git-cow-probe-%d", time.Now().UnixNano()))

	err = cloneFile(filepath.Join(common, "HEAD"), probe)
	_ = os.Remove(probe)

	switch {
	case err == nil:
		return true, nil

	case isUnsupported(err):
		return false, nil
	}

	return false, fmt.Errorf("probing for clone support: %w", err)
}

// absDir returns dir as an absolute path, treating "" as the current directory.
func absDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}

	return abs
}

// runGit runs git in dir with the user's own environment and returns a
// *GitError carrying git's exit status on failure.
func runGit(ctx context.Context, dir string, args ...string) error {
	_, err := gitOut(ctx, dir, args...)

	return err
}

// gitOut is runGit that also returns trimmed standard output. Only the
// trailing newline is trimmed, so NUL-separated output survives.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = absDir(dir)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", &GitError{
				Args:     args,
				ExitCode: exit.ExitCode(),
				Stderr:   strings.TrimSpace(stderr.String()),
			}
		}

		return "", err
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Passthrough runs "git worktree <sub> args..." in dir with the caller's
// streams and returns Git's exit status. It backs "list", "remove" and
// "prune", which need no cloning and so are Git's own.
func Passthrough(ctx context.Context, dir, sub string, args []string,
	stdout, stderr io.Writer) int {

	cmd := exec.CommandContext(ctx, "git", append([]string{"worktree", sub}, args...)...)
	cmd.Dir = absDir(dir)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0

	case errors.As(err, &exit):
		return exit.ExitCode()
	}

	_, _ = fmt.Fprintln(stderr, err)

	return 1
}
