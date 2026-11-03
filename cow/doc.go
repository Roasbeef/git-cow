// Package cow makes an independent copy of a Git repository that shares disk
// blocks with the original until either side changes.
//
// The copy is a full repository with its own .git directory. It is not a
// linked worktree, so nothing written in the copy can reach the template's
// Git directory, hooks or config. Hard links are never used, because a
// hard-linked file is shared for writes.
//
// Where the filesystem supports it, file contents are cloned with
// clonefile(2) on macOS or the FICLONE ioctl on Linux. Where it does not, the
// package falls back to an ordinary byte copy, unless the caller asked for
// copy-on-write only.
package cow
