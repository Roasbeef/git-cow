package cow

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Platform capabilities for whole-tree and per-file cloning.
const (
	hasWholeTreeClone  = true
	clonePreservesMeta = true
	methodFileClone    = MethodClonefile
)

// errNoCoW marks a clone attempt that the platform cannot perform.
var errNoCoW = unix.ENOTSUP

// wholeTreeClone clones the directory src to dst with one clonefile(2) call.
//
// The call is atomic, so dst either appears whole or not at all, and
// CLONE_NOFOLLOW stops it following a symlink at the top of src. Symlinks
// inside the tree are cloned as symlinks.
func wholeTreeClone(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

// cloneFile clones one file with clonefile(2). On failure dst does not exist.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

// isUnsupported reports whether err means the filesystem cannot clone, as
// opposed to a real failure.
func isUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS)
}

// renameNoReplace renames oldPath to newPath and fails if newPath exists.
// A plain rename would silently replace an empty directory that appeared in
// the meantime.
func renameNoReplace(oldPath, newPath string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return renameFallback(oldPath, newPath)
	}

	return err
}

// renameFallback is the check-then-rename used where the atomic form is
// missing.
func renameFallback(oldPath, newPath string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return fs.ErrExist
	}

	return os.Rename(oldPath, newPath)
}

// lchtimes sets the modification time of a symlink itself.
func lchtimes(path string, t time.Time) error {
	ts := []unix.Timespec{unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())}

	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// statIndexFields reads the Git index stat fields of path without following symlinks.
func statIndexFields(path string) (indexStat, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return indexStat{}, false
	}

	return indexStat{
		ctimeSec:  uint32(st.Ctim.Sec),
		ctimeNsec: uint32(st.Ctim.Nsec),
		mtimeSec:  uint32(st.Mtim.Sec),
		mtimeNsec: uint32(st.Mtim.Nsec),
		dev:       uint32(st.Dev),
		ino:       uint32(st.Ino),
		uid:       uint32(st.Uid),
		gid:       uint32(st.Gid),
		size:      uint32(st.Size),
	}, true
}
