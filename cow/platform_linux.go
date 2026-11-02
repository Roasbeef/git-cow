package cow

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// errNoCoW marks a clone attempt that the platform cannot perform.
var errNoCoW = unix.EOPNOTSUPP

// wholeTreeClone is unsupported on Linux, which has no tree-level clone, so
// the caller walks the tree and reflinks file by file.
func wholeTreeClone(src, dst string) error {
	return errNoCoW
}

// cloneFile reflinks src to a new file dst with the FICLONE ioctl. On failure
// dst does not exist.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)

		return err
	}

	return out.Close()
}

// isUnsupported reports whether err means the filesystem cannot reflink, as
// opposed to a real failure. ENOTTY is what some filesystems return for an
// ioctl they do not implement.
func isUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS)
}

// renameNoReplace renames oldPath to newPath and fails if newPath exists.
// A plain rename would silently replace an empty directory that appeared in
// the meantime.
func renameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		if _, statErr := os.Lstat(newPath); statErr == nil {
			return fs.ErrExist
		}

		return os.Rename(oldPath, newPath)
	}

	return err
}

// lchtimes sets the modification time of a symlink itself.
func lchtimes(path string, t time.Time) error {
	ts := []unix.Timespec{unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())}

	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}
