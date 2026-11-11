package cow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Method names how file contents were duplicated.
const (
	// MethodClonefile means one clonefile(2) call cloned the whole tree.
	MethodClonefile = "clonefile"

	// MethodReflink means every non-empty file was cloned individually.
	MethodReflink = "reflink"

	// MethodCopy means at least one file was copied byte by byte.
	MethodCopy = "copy"
)

// ErrNoCoW is returned when Options.RequireCOW is set and the filesystem
// cannot clone files.
var ErrNoCoW = errors.New("copy-on-write cloning is not available")

// RefusalError reports that the template, the destination or the tree
// contents make an independent copy impossible or unsafe. Nothing has been
// written when it is returned.
type RefusalError struct {
	Reason string
}

// Error implements the error interface.
func (e *RefusalError) Error() string {
	return "refused: " + e.Reason
}

// refusef builds a RefusalError from a format string.
func refusef(format string, args ...any) error {
	return &RefusalError{Reason: fmt.Sprintf(format, args...)}
}

// Options tunes a copy.
type Options struct {
	// RequireCOW makes Copy fail with ErrNoCoW instead of falling back to
	// an ordinary file copy.
	RequireCOW bool

	// The fields below let the package tests force paths that the host
	// filesystem would not otherwise take.
	forceWalk  bool
	forceNoCoW bool
}

// Result describes a finished copy.
type Result struct {
	// Method is one of MethodClonefile, MethodReflink or MethodCopy.
	Method string

	// Path is the absolute path of the new repository.
	Path string

	// Files is the number of regular files in the copy.
	Files int

	// Bytes is the total size of those regular files.
	Bytes int64

	// Elapsed is the wall time of the whole operation.
	Elapsed time.Duration

	// Head is the commit HEAD resolves to, empty for an unborn branch.
	Head string
}

// Test seams. Production code leaves both nil.
var (
	// perFileHook runs before each file in the walking copier is
	// duplicated. A non-nil error aborts the copy.
	perFileHook func(srcPath string) error

	// cloneTree is the whole-tree clone, replaceable by tests.
	cloneTree = wholeTreeClone

	// beforeRenameHook runs after the copy is built and verified but
	// before it is renamed into place.
	beforeRenameHook func(tree string) error
)

// Copy duplicates the repository at template into dest.
//
// The tree is built inside a temporary directory next to dest and renamed
// into place as the last step, so dest only ever exists whole. The
// temporary directory is removed on every path out of the function.
func Copy(ctx context.Context, template, dest string, opts Options) (Result, error) {
	start := time.Now()

	tpl, dst, err := resolvePaths(template, dest)
	if err != nil {
		return Result{}, err
	}

	if err := checkTemplate(tpl); err != nil {
		return Result{}, err
	}

	if err := verifyRoots(ctx, tpl); err != nil {
		return Result{}, err
	}

	head, err := gitHead(ctx, tpl)
	if err != nil {
		return Result{}, fmt.Errorf("reading template HEAD: %w", err)
	}

	// The scan refuses special files before anything is written and
	// supplies the file and byte counts for both copy strategies.
	stats, err := scan(tpl)
	if err != nil {
		return Result{}, err
	}

	// The container is created in dest's parent so the final rename never
	// crosses a filesystem. clonefile(2) needs a destination that does not
	// exist, so the tree lives one level below the container.
	container, err := os.MkdirTemp(filepath.Dir(dst), ".git-cow-tmp-")
	if err != nil {
		return Result{}, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer removeAll(container)

	tree := filepath.Join(container, "tree")

	method, err := build(ctx, tpl, tree, stats, opts)
	if err != nil {
		return Result{}, err
	}

	refreshIndexStats(tree, filepath.Join(tree, ".git", "index"), tpl)

	if err := verify(ctx, tree, head); err != nil {
		return Result{}, err
	}

	if beforeRenameHook != nil {
		if err := beforeRenameHook(tree); err != nil {
			return Result{}, err
		}
	}

	if err := renameNoReplace(tree, dst); err != nil {
		return Result{}, fmt.Errorf("moving copy into place: %w", err)
	}

	return Result{
		Method:  method,
		Path:    dst,
		Files:   stats.files,
		Bytes:   stats.bytes,
		Elapsed: time.Since(start),
		Head:    head,
	}, nil
}

// build duplicates tpl into tree and reports the method that was used.
func build(ctx context.Context, tpl, tree string, stats treeStats,
	opts Options) (string, error) {

	// One clonefile(2) of the whole tree is the fastest path and is
	// atomic. When it is unsupported we fall through to the walk, which
	// decides per file whether cloning works.
	if hasWholeTreeClone && !opts.forceWalk && !opts.forceNoCoW {
		err := cloneTree(tpl, tree)
		switch {
		case err == nil:
			detachCopyGitDir(tree)

			return MethodClonefile, nil

		case !isUnsupported(err):
			return "", fmt.Errorf("cloning tree: %w", err)
		}

		removeAll(tree)
	}

	w := &walker{
		opts: opts,
		skip: map[string]struct{}{
			".git/worktrees":             {},
			".git/fsmonitor--daemon.ipc": {},
		},
	}
	if err := w.copyDir(ctx, tpl, tree, ""); err != nil {
		return "", err
	}

	return w.method(), nil
}

// detachCopyGitDir removes host-specific runtime artifacts from a whole-tree
// clone's .git directory so the copy does not list the template's linked
// worktrees or inherit its fsmonitor socket.
func detachCopyGitDir(tree string) {
	removeAll(filepath.Join(tree, ".git", "worktrees"))
	removeAll(filepath.Join(tree, ".git", "fsmonitor--daemon.ipc"))
}

// makeWritable makes every directory under dir owner-writable so a read-only
// directory does not block removal.
func makeWritable(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}

		return nil
	})
}

// removeAll deletes dir, first making every directory in it writable so a
// read-only directory in the template cannot strand the temporary tree.
func removeAll(dir string) {
	makeWritable(dir)
	_ = os.RemoveAll(dir)
}
