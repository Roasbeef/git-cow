package cow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// walker copies a tree entry by entry. It is the Linux strategy, the
// fallback on macOS when a whole-tree clone is unsupported, and the engine
// that populates a linked worktree from a source checkout.
type walker struct {
	opts Options

	// skip holds slash-separated paths, relative to the walk root, that are
	// left out of the copy. A skipped path is not created at all.
	skip map[string]struct{}

	// skipSpecial leaves out sockets, FIFOs and devices instead of
	// refusing them. A working tree can hold a dev server's socket, which
	// is a runtime artifact and not something to duplicate.
	skipSpecial bool

	// treeClone lets a directory that contains no skipped path be cloned
	// with one whole-tree call where the platform has one.
	treeClone bool

	// fileCloneUnsupported is set once cloneFile reports that the
	// filesystem cannot clone files, so later files go straight to a byte
	// copy instead of retrying.
	fileCloneUnsupported bool

	// copied counts files that fell back to a byte copy.
	copied int

	// wholeTrees counts directories cloned with one whole-tree call.
	wholeTrees int

	// files and bytes count what was written, for walks whose totals are
	// not known up front.
	files int
	bytes int64
}

// method reports how the walker duplicated files.
func (w *walker) method() string {
	if w.copied > 0 || w.opts.forceNoCoW {
		return MethodCopy
	}

	if w.wholeTrees > 0 {
		return MethodClonefile
	}

	return methodFileClone
}

// copyDir recreates directory src at dst and everything beneath it.
//
// The directory is created owner-writable and only given its final mode and
// times after its children exist, so a read-only template directory does not
// block its own population and its mtime is not disturbed by the children.
func (w *walker) copyDir(ctx context.Context, src, dst, rel string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}

	if err := w.populate(ctx, src, dst, rel); err != nil {
		return err
	}

	if rel != "" && w.containsSkip(rel) {
		if entries, err := os.ReadDir(dst); err == nil && len(entries) == 0 {
			return os.Remove(dst)
		}
	}

	return finish(dst, info)
}

// populate copies the children of src into the existing directory dst.
func (w *walker) populate(ctx context.Context, src, dst, rel string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		childRel := path.Join(rel, entry.Name())
		if _, skipped := w.skip[childRel]; skipped {
			continue
		}

		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())

		if err := w.copyEntry(ctx, from, to, childRel, entry); err != nil {
			return err
		}
	}

	return nil
}

// containsSkip reports whether a skipped path lies beneath the directory rel.
func (w *walker) containsSkip(rel string) bool {
	for k := range w.skip {
		if strings.HasPrefix(k, rel+"/") {
			return true
		}
	}

	return false
}

// copyEntry duplicates one directory entry.
func (w *walker) copyEntry(ctx context.Context, from, to, rel string,
	entry fs.DirEntry) error {

	switch t := entry.Type(); {
	case t.IsDir():
		done, err := w.cloneWhole(from, to, rel)
		if err != nil || done {
			return err
		}

		return w.copyDir(ctx, from, to, rel)

	case t&fs.ModeSymlink != 0:
		return copySymlink(from, to)

	case t.IsRegular():
		return w.copyFile(from, to)

	case w.skipSpecial:
		return nil
	}

	return refusef("%q is a %s, which cannot be copied", from, kind(entry.Type()))
}

// cloneWhole clones a whole directory in one call when that is allowed and
// safe, and reports whether it did. A tree holding a special file is left to
// the walk, which knows what to do with one.
func (w *walker) cloneWhole(from, to, rel string) (bool, error) {
	if !w.treeClone || w.containsSkip(rel) {
		return false, nil
	}

	stats, special, err := scanTree(from, func(p string, m fs.FileMode) error {
		if m&fs.ModeSymlink != 0 || !w.skipSpecial {
			return nil
		}

		if subRel, relErr := filepath.Rel(from, p); relErr == nil {
			if w.skip == nil {
				w.skip = make(map[string]struct{})
			}

			w.skip[path.Join(rel, filepath.ToSlash(subRel))] = struct{}{}
		}

		return nil
	})
	if err != nil || special > 0 {
		return false, err
	}

	err = cloneTree(from, to)
	switch {
	case err == nil:
		w.files += stats.files
		w.bytes += stats.bytes
		w.wholeTrees++

		return true, nil

	case isUnsupported(err):
		// Every later directory would fail the same way, so stop scanning
		// them for a whole-tree clone.
		w.treeClone = false

		return false, nil
	}

	return false, fmt.Errorf("cloning %q: %w", from, err)
}

// copySymlink recreates a symlink with the same target text. The target is
// never resolved or followed, so a link that points outside the tree stays
// a link to that path and the copy holds none of its contents.
func copySymlink(from, to string) error {
	target, err := os.Readlink(from)
	if err != nil {
		return err
	}

	if err := os.Symlink(target, to); err != nil {
		return err
	}

	info, err := os.Lstat(from)
	if err != nil {
		return err
	}

	return lchtimes(to, info.ModTime())
}

// copyFile duplicates a regular file, cloning it when the filesystem can and
// otherwise copying the bytes unless RequireCOW forbids that.
func (w *walker) copyFile(from, to string) error {
	if perFileHook != nil {
		if err := perFileHook(from); err != nil {
			return err
		}
	}

	info, err := os.Lstat(from)
	if err != nil {
		return err
	}

	w.files++
	w.bytes += info.Size()

	// An empty file has no blocks to share, so cloning it proves nothing
	// about the filesystem and is not counted as a fallback.
	if info.Size() == 0 {
		if err := createEmpty(to); err != nil {
			return err
		}

		return finish(to, info)
	}

	err = errNoCoW
	if !w.opts.forceNoCoW && !w.fileCloneUnsupported {
		err = cloneFile(from, to)
	}

	switch {
	case err == nil:
		if clonePreservesMeta {
			return nil
		}

	case !isUnsupported(err):
		return err

	case w.opts.RequireCOW:
		return fmt.Errorf("%w: cloning %q: %v", ErrNoCoW, from, err)

	default:
		if !w.fileCloneUnsupported {
			w.fileCloneUnsupported = true

			// The failed clone must not leave a partial destination behind
			// before the byte copy creates it exclusively.
			if rmErr := os.Remove(to); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				return rmErr
			}
		}

		if err := copyBytes(from, to); err != nil {
			return err
		}

		w.copied++
	}

	return finish(to, info)
}

// createEmpty creates the empty file to exclusively.
func createEmpty(to string) error {
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	return out.Close()
}

// copyBytes creates to exclusively and fills it from from.
func copyBytes(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}

	return out.Close()
}

// finish applies the template's permission bits and modification time.
func finish(path string, info fs.FileInfo) error {
	mode := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if err := os.Chmod(path, mode); err != nil {
		return err
	}

	return os.Chtimes(path, info.ModTime(), info.ModTime())
}
