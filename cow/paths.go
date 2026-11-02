package cow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolvePaths returns the template and destination as absolute paths with
// symlinks in their parent components resolved, and refuses a destination
// that exists or that nests with the template.
func resolvePaths(template, dest string) (string, string, error) {
	tplAbs, err := filepath.Abs(template)
	if err != nil {
		return "", "", err
	}

	tpl, err := filepath.EvalSymlinks(tplAbs)
	if err != nil {
		return "", "", refusef("template %q is not readable: %v", template, err)
	}

	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return "", "", err
	}

	parent, err := filepath.EvalSymlinks(filepath.Dir(destAbs))
	if err != nil {
		return "", "", fmt.Errorf("destination parent: %w", err)
	}

	dst := filepath.Join(parent, filepath.Base(destAbs))

	if within(tpl, dst) {
		return "", "", refusef("destination %q is inside the template", dest)
	}

	if within(dst, tpl) {
		return "", "", refusef("template %q is inside the destination", template)
	}

	if _, err := os.Lstat(dst); err == nil {
		return "", "", refusef("destination %q already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}

	return tpl, dst, nil
}

// within reports whether child equals parent or lies beneath it.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// checkTemplate refuses a template whose Git directory could be shared with
// something else, since the copy would then not be independent.
func checkTemplate(tpl string) error {
	gitDir := filepath.Join(tpl, ".git")

	info, err := os.Lstat(gitDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refusef("%q has no .git, so it is not a non-bare repository", tpl)

	case err != nil:
		return err

	case info.Mode()&fs.ModeSymlink != 0:
		return refusef(".git in %q is a symlink", tpl)

	case !info.IsDir():
		return refusef(".git in %q is a file, so this is a linked worktree", tpl)
	}

	alternates := filepath.Join(gitDir, "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return refusef("%q borrows objects through objects/info/alternates", tpl)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// treeStats are the counts gathered by scan.
type treeStats struct {
	files int
	bytes int64
}

// scan walks the template without following symlinks, counts regular files
// and refuses anything that is not a directory, regular file or symlink.
func scan(root string) (treeStats, error) {
	stats, _, err := scanTree(root, func(path string, m fs.FileMode) error {
		return refusef("%q is a %s, which cannot be copied", path, kind(m))
	})

	return stats, err
}

// scanTree counts the regular files under root. For every entry that is
// neither a directory, regular file nor symlink it calls onSpecial, which may
// be nil, and counts it. A non-nil error from onSpecial stops the walk.
func scanTree(root string, onSpecial func(string, fs.FileMode) error) (treeStats, int, error) {
	var (
		stats   treeStats
		special int
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		switch {
		case d.IsDir(), d.Type()&fs.ModeSymlink != 0:
			return nil

		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}

			stats.files++
			stats.bytes += info.Size()

			return nil
		}

		special++
		if onSpecial != nil {
			return onSpecial(path, d.Type())
		}

		return nil
	})

	return stats, special, err
}

// kind names a non-regular file type for an error message.
func kind(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}

	return "special file"
}
