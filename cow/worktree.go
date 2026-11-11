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

	// Quiet is "-q" or "--quiet".
	Quiet bool

	// Lock is "--lock", which keeps the new worktree locked after creation.
	Lock bool

	// LockReason is "--reason", the explanation recorded with "--lock".
	LockReason string

	// Track is "--track".
	Track bool

	// NoTrack is "--no-track".
	NoTrack bool

	// GuessRemote is "--guess-remote".
	GuessRemote bool

	// NoGuessRemote is "--no-guess-remote".
	NoGuessRemote bool

	// RequireCOW makes AddWorktree fail with ErrNoCoW instead of falling
	// back to plain "git worktree add".
	RequireCOW bool

	// Untracked carries the source's untracked files that are not ignored
	// into the new worktree. By default they are left behind, so the new
	// worktree has nothing for "git add -A" to sweep into a commit.
	// Ignored files such as build output are carried either way.
	Untracked bool

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
// filled by cloning the source checkout, so ignored files such as build
// output come along, and made to match the requested commit the way a
// checkout would.
//
// Untracked files that are not ignored, the ones "git status" shows as "??",
// are left behind unless opts.Untracked is set. They are a person's own
// notes and scratch files, and in a new worktree they would be one
// "git add -A" from a commit. Git decides which files they are, with
// "git ls-files --others --exclude-standard", and the walk then leaves those
// paths out of the copy. Nothing is ever deleted: the source is only read,
// and the destination is built without the files instead of cleaned after.
// A wholly untracked directory is not created. A directory that also holds
// ignored files is created with only those. An untracked nested repository,
// which Git lists as one entry, is left behind whole and never entered.
// Afterwards Git is asked again, in the new worktree, and the add fails and
// is rolled back if anything untracked is there.
//
// The source must have no staged or unstaged changes to tracked files. That
// is the one state in which its files are exactly its HEAD commit, so the
// result can be made to match any commit-ish with a hard reset that rewrites
// only the files which differ. With opts.Untracked, the copied untracked
// files are not protected from that reset: a commit-ish that tracks a path
// where the source has an untracked file replaces it. Without it, the file
// was never copied and the reset creates the tracked one.
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

	dest = resolveLoose(filepath.Clean(dest))

	if dest == src || within(filepath.Join(src, ".git"), dest) {
		return AddResult{}, refusef("destination %q is inside the source checkout root or .git", opts.Path)
	}

	if err := requireClean(ctx, src); err != nil {
		return AddResult{}, err
	}

	srcHead, err := gitOut(ctx, src, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, refusef("the source checkout has no commits")
	}

	supported, err := probeCoW(src, dest, opts.forceNoCoW)
	if err != nil {
		return AddResult{}, err
	}

	existedBefore := branchExistedBeforeAdd(ctx, src, dest, opts)
	createdParent := missingSubtreeRoot(src, dest)

	if !supported {
		if opts.RequireCOW {
			return AddResult{}, fmt.Errorf("%w: the filesystem cannot clone files", ErrNoCoW)
		}

		return addPlain(ctx, src, dest, createdParent, existedBefore, opts, start)
	}

	if err := runGit(ctx, src, addArgs(opts, dest, true)...); err != nil {
		return AddResult{}, err
	}

	res, err := populateWorktree(ctx, src, srcHead, dest, createdParent, opts)
	if err != nil {
		rollbackAdd(src, dest, createdParent, existedBefore)

		return AddResult{}, err
	}

	res.Elapsed = time.Since(start)

	return res, nil
}

// populateWorktree fills the empty worktree dest from src and checks the
// result against its HEAD.
func populateWorktree(ctx context.Context, src, srcHead, dest, createdParent string,
	opts AddOptions) (AddResult, error) {

	gitlinks, err := gitlinkPaths(ctx, src)
	if err != nil {
		return AddResult{}, err
	}

	nestedWTs, err := worktreesInside(ctx, src)
	if err != nil {
		return AddResult{}, err
	}

	w := &walker{
		opts:        Options{RequireCOW: opts.RequireCOW, forceNoCoW: opts.forceNoCoW},
		skipSpecial: true,
		treeClone:   hasWholeTreeClone && !opts.forceWalk && !opts.forceNoCoW,
	}

	w.addSkip(".git")

	for _, p := range gitlinks {
		w.addSkip(p)
	}

	for _, p := range nestedWTs {
		w.addSkip(p)
	}

	if createdParent != "" {
		w.addSkip(createdParent)
	} else if within(src, dest) {
		if rel, relErr := filepath.Rel(src, dest); relErr == nil {
			w.addSkip(filepath.ToSlash(rel))
		}
	}

	if !opts.Untracked {
		left, err := untrackedPaths(ctx, src)
		if err != nil {
			return AddResult{}, err
		}

		for _, p := range left {
			w.addSkip(p)
		}
	}

	if err := w.populate(ctx, src, dest, ""); err != nil {
		return AddResult{}, err
	}

	// Copy the source index (and any split-index base) into the new
	// worktree and update its stat cache with the clones' own inode and
	// ctime values so Git does not rehash the working tree.
	indexPath, err := copyIndex(ctx, src, dest)
	if err != nil {
		return AddResult{}, err
	}

	refreshIndexStats(dest, indexPath, "")

	head, err := gitOut(ctx, dest, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, err
	}

	// When the target commit differs from the source HEAD or the repository
	// has submodules, a hard reset rewrites only the files that differ
	// between the two commits and creates the empty submodule directories.
	if head != srcHead || len(gitlinks) > 0 {
		_ = runGit(ctx, dest, "-c", "core.checkStat=minimal", "-c", "core.trustctime=false",
			"update-index", "--refresh", "-q")

		if err := runGit(ctx, dest, "-c", "core.checkStat=minimal", "-c", "core.trustctime=false",
			"reset", "--hard", "-q"); err != nil {
			return AddResult{}, err
		}
	}

	status, err := gitOut(ctx, dest, "status", "--porcelain=v1", "--untracked-files=no", "--ignore-submodules=dirty")
	if err != nil {
		return AddResult{}, err
	}

	if status != "" {
		return AddResult{}, fmt.Errorf("new worktree does not match its commit:\n%s", status)
	}

	if !opts.Untracked {
		if err := requireNoUntracked(ctx, dest); err != nil {
			return AddResult{}, err
		}
	}

	return AddResult{Method: w.method(), Path: dest, Files: w.files, Bytes: w.bytes, Head: head}, nil
}

// addPlain makes the worktree with plain Git, for filesystems that cannot
// clone.
func addPlain(ctx context.Context, src, dest, createdParent string,
	existedBefore bool, opts AddOptions, start time.Time) (AddResult, error) {

	if err := runGit(ctx, src, addArgs(opts, dest, false)...); err != nil {
		return AddResult{}, err
	}

	head, err := gitOut(ctx, dest, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return AddResult{}, err
	}

	// A checkout holds no untracked files, which is already the default.
	// Asking for them means copying them over, one by one.
	if opts.Untracked {
		if err := copyUntracked(ctx, src, dest); err != nil {
			rollbackAdd(src, dest, createdParent, existedBefore)

			return AddResult{}, err
		}
	}

	return AddResult{Method: MethodWorktree, Path: dest, Elapsed: time.Since(start), Head: head}, nil
}

// addArgs builds the "git worktree add" command line.
func addArgs(opts AddOptions, dest string, noCheckout bool) []string {
	args := []string{"worktree", "add"}

	if noCheckout {
		args = append(args, "--no-checkout")
	}

	if opts.Quiet {
		args = append(args, "--quiet")
	}

	if opts.Detach {
		args = append(args, "--detach")
	}

	if opts.Force {
		args = append(args, "--force")
	}

	if opts.Lock {
		args = append(args, "--lock")
	}

	if opts.LockReason != "" {
		args = append(args, "--reason", opts.LockReason)
	}

	if opts.Track {
		args = append(args, "--track")
	}

	if opts.NoTrack {
		args = append(args, "--no-track")
	}

	if opts.GuessRemote {
		args = append(args, "--guess-remote")
	}

	if opts.NoGuessRemote {
		args = append(args, "--no-guess-remote")
	}

	if opts.Branch != "" {
		args = append(args, "-b", opts.Branch)
	}

	if opts.ResetBranch != "" {
		args = append(args, "-B", opts.ResetBranch)
	}

	args = append(args, "--", dest)

	if opts.Commitish != "" {
		args = append(args, opts.Commitish)
	}

	return args
}

// branchExistedBeforeAdd reports whether the branch that "git worktree add"
// would check out in dest already existed in src before the add.
func branchExistedBeforeAdd(ctx context.Context, src, dest string, opts AddOptions) bool {
	if opts.Detach || opts.Branch != "" {
		return false
	}

	var candidate string
	switch {
	case opts.ResetBranch != "":
		candidate = "refs/heads/" + opts.ResetBranch
	case opts.Commitish != "":
		candidate = opts.Commitish
	default:
		candidate = "refs/heads/" + filepath.Base(dest)
	}

	out, err := gitOut(ctx, src, "rev-parse", "--verify", "--quiet", "--symbolic-full-name", candidate)

	return err == nil && strings.HasPrefix(out, "refs/heads/")
}

// rollbackAdd undoes a half-made worktree and any branch created for it.
//
// It never runs "git worktree prune", which would also delete the records of
// unrelated worktrees whose directories happen to be missing. If Git cannot
// remove the worktree, only this worktree's own record is deleted.
func rollbackAdd(src, dest, createdParent string, branchExistedBefore bool) {
	ctx := context.Background()

	record := worktreeRecord(dest)
	var createdBranch string
	if !branchExistedBefore && record != "" {
		if data, err := os.ReadFile(filepath.Join(record, "HEAD")); err == nil {
			if b, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/"); ok {
				createdBranch = b
			}
		}
	}

	makeWritable(dest)

	if err := runGit(ctx, src, "worktree", "remove", "--force", "--force", dest); err != nil && record != "" {
		_ = os.RemoveAll(record)
	}

	removeAll(dest)
	if createdParent != "" {
		removeAll(filepath.Join(src, filepath.FromSlash(createdParent)))
	}

	if createdBranch != "" {
		_ = runGit(ctx, src, "branch", "-D", createdBranch)
	}
}

// missingSubtreeRoot returns the highest slash-separated relative path beneath
// src on the way to dest that does not yet exist before "git worktree add"
// creates it, or "" when dest is outside src.
func missingSubtreeRoot(src, dest string) string {
	if !within(src, dest) || dest == src {
		return ""
	}

	rel, err := filepath.Rel(src, dest)
	if err != nil {
		return ""
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	cur := src
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		if _, err := os.Lstat(cur); errors.Is(err, os.ErrNotExist) {
			return strings.Join(parts[:i+1], "/")
		}
	}

	return filepath.ToSlash(rel)
}

// worktreesInside returns the slash-separated paths, relative to src, of any
// linked worktrees currently located beneath src.
func worktreesInside(ctx context.Context, src string) ([]string, error) {
	out, err := gitOut(ctx, src, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}

	var rels []string
	for _, tok := range strings.Split(out, "\x00") {
		p, ok := strings.CutPrefix(tok, "worktree ")
		if !ok {
			continue
		}

		p = resolveLoose(filepath.Clean(p))
		if p == src || !within(src, p) {
			continue
		}

		if rel, relErr := filepath.Rel(src, p); relErr == nil {
			rels = append(rels, filepath.ToSlash(rel))
		}
	}

	return rels, nil
}

// worktreeRecord returns the administrative directory named by dest/.git.
func worktreeRecord(dest string) string {
	data, err := os.ReadFile(filepath.Join(dest, ".git"))
	if err != nil {
		return ""
	}

	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return ""
	}

	if !filepath.IsAbs(dir) {
		dir = filepath.Join(dest, dir)
	}

	return dir
}

// requireClean refuses a source whose tracked files differ from HEAD.
func requireClean(ctx context.Context, src string) error {
	status, err := gitOut(ctx, src, "status", "--porcelain=v1", "--untracked-files=no", "--ignore-submodules=dirty")
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

// copyIndex copies src's index file (and any split-index base file) into
// dest's Git directory, and returns the path of dest's index.
func copyIndex(ctx context.Context, src, dest string) (string, error) {
	from, err := gitOut(ctx, src, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}

	to, err := gitOut(ctx, dest, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}

	if err := copyOrCloneFile(from, to); err != nil {
		return "", err
	}

	fromDir, toDir := filepath.Dir(from), filepath.Dir(to)
	if fromDir != toDir {
		entries, err := os.ReadDir(fromDir)
		if err != nil {
			return "", err
		}

		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "sharedindex.") {
				if err := copyOrCloneFile(filepath.Join(fromDir, e.Name()), filepath.Join(toDir, e.Name())); err != nil {
					return "", err
				}
			}
		}
	}

	return to, nil
}

// copyOrCloneFile duplicates a file inside the Git directory, cloning when
// possible and copying bytes otherwise.
func copyOrCloneFile(from, to string) error {
	_ = os.Remove(to)
	if err := cloneFile(from, to); err == nil {
		if !clonePreservesMeta {
			return os.Chmod(to, 0o644)
		}

		return nil
	}

	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}

	return os.WriteFile(to, data, 0o644)
}

// probeCoW reports whether files from src can be cloned to the place dest
// will go, by cloning one small file there and removing it.
func probeCoW(src, dest string, forceNo bool) (bool, error) {
	if forceNo {
		return false, nil
	}

	target := filepath.Join(src, ".git")
	info, err := os.Lstat(target)
	if err != nil {
		return false, err
	}

	if info.IsDir() {
		target = filepath.Join(target, "HEAD")
	}

	dir := filepath.Dir(dest)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}

		dir = filepath.Dir(dir)
	}

	probe := filepath.Join(dir, fmt.Sprintf(".git-cow-probe-%d", time.Now().UnixNano()))

	err = cloneFile(target, probe)
	_ = os.Remove(probe)

	switch {
	case err == nil:
		return true, nil

	case isUnsupported(err):
		return false, nil
	}

	return false, fmt.Errorf("probing for clone support: %w", err)
}

// resolveLoose resolves symlinks in the part of p that exists and keeps the
// rest as written, so a path that does not exist yet can be compared with
// the resolved paths Git reports.
func resolveLoose(p string) string {
	rest := ""

	for cur := p; ; cur = filepath.Dir(cur) {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}

		if filepath.Dir(cur) == cur {
			return p
		}

		rest = filepath.Join(filepath.Base(cur), rest)
	}
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
