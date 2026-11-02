//go:build unix

package cow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestSpecialFilesRefused checks that a FIFO in the template is refused
// with a message naming it, and that nothing is left behind.
func TestSpecialFilesRefused(t *testing.T) {
	for _, mode := range copyModes {
		t.Run(mode.name, func(t *testing.T) {
			tpl := newRepo(t)
			parent := filepath.Dir(tpl)

			if err := unix.Mkfifo(filepath.Join(tpl, "src", "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := Copy(context.Background(), tpl, filepath.Join(parent, "copy"), mode.opts)

			var r *RefusalError
			if !errors.As(err, &r) {
				t.Fatalf("err = %v, want a RefusalError", err)
			}

			assertNothingLeft(t, parent, filepath.Join(parent, "copy"), "tpl")
		})
	}
}

// TestAddSkipsSpecialFiles checks that a worktree made from a checkout with a
// FIFO in an untracked directory succeeds and leaves the FIFO out.
func TestAddSkipsSpecialFiles(t *testing.T) {
	src := newRepo(t)
	write(t, filepath.Join(src, "build", "keep.o"), "obj\n")

	if err := unix.Mkfifo(filepath.Join(src, "build", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(filepath.Dir(src), "wt")

	if _, err := AddWorktree(context.Background(), AddOptions{Dir: src, Path: dest, Branch: "feat"}); err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(dest, "build", "keep.o")); got != "obj\n" {
		t.Errorf("artifact content %q", got)
	}

	if _, err := os.Lstat(filepath.Join(dest, "build", "pipe")); err == nil {
		t.Error("the FIFO was copied")
	}
}

// TestAddIndexHoldsCloneInodes checks the new worktree's index records the
// inode of the cloned file and not the source's. "git diff-files" would pass
// either way because it rehashes, so the stat data is read directly.
func TestAddIndexHoldsCloneInodes(t *testing.T) {
	src := newRepo(t)

	res, err := AddWorktree(context.Background(), AddOptions{
		Dir: src, Path: filepath.Join(filepath.Dir(src), "wt"), Branch: "feat",
	})
	if err != nil {
		t.Fatal(err)
	}

	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(res.Path, "README.md"), &st); err != nil {
		t.Fatal(err)
	}

	out := git(t, res.Path, "ls-files", "--debug", "README.md")
	if want := fmt.Sprintf("ino: %d", st.Ino); !strings.Contains(out, want) {
		t.Errorf("index stat data lacks %q:\n%s", want, out)
	}
}
