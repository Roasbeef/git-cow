package cow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// untrackedPaths lists what "git status" shows as "??" in the checkout at
// dir, as slash-separated paths relative to dir. Git decides, so the answer
// follows every ignore source it knows: .gitignore files, .git/info/exclude
// and core.excludesFile.
//
// Files are listed one by one, so a directory that holds both untracked and
// ignored files is not hidden behind one entry. A directory that is another
// repository is the exception: Git lists it as a single entry with a
// trailing slash and does not look inside, and neither do we. The trailing
// slash is removed here. Names come NUL-separated, so spaces, newlines,
// quotes and leading dashes survive.
//
// Git on macOS reports names precomposed (NFC) by default, but a file made
// by Finder is often stored decomposed (NFD), and skipping is by the name
// the walk reads from the directory. Turning precomposition off makes Git
// report the names exactly as stored, which are the names the walk sees.
func untrackedPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOut(ctx, dir, "-c", "core.precomposeunicode=false",
		"ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}

	var paths []string

	for _, p := range strings.Split(out, "\x00") {
		if p = strings.TrimSuffix(p, "/"); p != "" {
			paths = append(paths, p)
		}
	}

	return paths, nil
}

// requireNoUntracked fails when the new worktree at dir holds any untracked
// file. Skipping is by name, so this is the check that the skip did its job.
// It catches the two ways a file can still arrive: an untracked ignore file
// that hid build output in the source and is not itself carried, and a name
// that Git and the filesystem spell in different Unicode forms.
func requireNoUntracked(ctx context.Context, dir string) error {
	left, err := untrackedPaths(ctx, dir)
	if err != nil {
		return err
	}

	if len(left) == 0 {
		return nil
	}

	return refusef("the new worktree would hold %d untracked file(s), such as %q, "+
		"most likely because an untracked ignore file was left behind or a file name "+
		"is spelled differently by Git and by the filesystem; commit the file or use --untracked",
		len(left), left[0])
}

// copyUntracked copies the source's untracked files into the plain worktree
// at dest, which holds only what the commit-ish tracks. It backs "--untracked"
// where files cannot be cloned.
//
// Nothing already in dest is replaced, so a path the commit-ish tracks wins
// over the source's untracked file, the same as the cloning path where the
// reset overwrites it. A path whose parent is a symlink or a file in dest is
// skipped and never written through, so a tracked symlink cannot send a copy
// outside the worktree. The source is only read.
func copyUntracked(ctx context.Context, src, dest string) error {
	paths, err := untrackedPaths(ctx, src)
	if err != nil {
		return err
	}

	w := &walker{skipSpecial: true}

	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}

		ok, err := prepareParents(dest, rel)
		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		from := filepath.Join(src, filepath.FromSlash(rel))
		to := filepath.Join(dest, filepath.FromSlash(rel))

		if _, err := os.Lstat(to); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		info, err := os.Lstat(from)
		if err != nil {
			return err
		}

		if err := w.copyEntry(ctx, from, to, rel, fs.FileInfoToDirEntry(info)); err != nil {
			return err
		}
	}

	return nil
}

// prepareParents creates the directories above rel inside dest and reports
// whether rel may be written there. Each component is examined with Lstat,
// never followed, and a component that exists as anything but a directory
// makes the answer false. A path with a ".", ".." or ".git" component is an
// error: Git never lists one, so seeing one means something is wrong.
func prepareParents(dest, rel string) (bool, error) {
	parts := strings.Split(rel, "/")

	for _, part := range parts {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false, fmt.Errorf("unexpected untracked path %q", rel)
		}
	}

	cur := dest

	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)

		info, err := os.Lstat(cur)

		switch {
		case err == nil && info.IsDir():
			continue

		case err == nil:
			return false, nil

		case !errors.Is(err, fs.ErrNotExist):
			return false, err
		}

		if err := os.Mkdir(cur, 0o755); err != nil {
			return false, err
		}
	}

	return true, nil
}
