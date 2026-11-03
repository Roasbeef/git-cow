//go:build !darwin && !linux

package cow

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

// Platform capabilities for whole-tree and per-file cloning.
const (
	hasWholeTreeClone  = false
	clonePreservesMeta = false
	methodFileClone    = MethodCopy
)

// errNoCoW marks a clone attempt that the platform cannot perform.
var errNoCoW = errors.New("no clone support on this platform")

// wholeTreeClone is unsupported here.
func wholeTreeClone(src, dst string) error { return errNoCoW }

// cloneFile is unsupported here.
func cloneFile(src, dst string) error { return errNoCoW }

// isUnsupported reports whether err is the missing-support marker.
func isUnsupported(err error) bool { return errors.Is(err, errNoCoW) }

// renameNoReplace checks and then renames. It is not atomic against a
// concurrent creator of newPath.
func renameNoReplace(oldPath, newPath string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return fs.ErrExist
	}

	return os.Rename(oldPath, newPath)
}

// lchtimes does nothing: symlink times are not preserved on this platform.
func lchtimes(path string, t time.Time) error { return nil }

// statIndexFields is a no-op where unix stat fields are not read directly.
func statIndexFields(path string) (indexStat, bool) { return indexStat{}, false }
