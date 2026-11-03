# git-cow

git-cow makes copy-on-write copies of Git repositories. A copy shares disk
blocks with the original until either side changes a file, so it is created
in a fraction of a second and costs almost no disk space at first.

It has two ways to make a copy, and they have different safety properties:

- `git-cow copy` makes an independent repository with its own `.git`.
  Nothing written in the copy can reach the original. Use this for code you
  do not trust, such as an agent working on a ticket.
- `git-cow add` makes a linked worktree, like `git worktree add`, whose
  files are clones of the files in your current checkout. It shares the
  original's Git directory. Use this when you work in parallel yourself.

## Why it exists

A normal `git clone` of a large repository writes every object and every
file again. On a 415 MiB pack that takes seconds and four clones cost about
2 GiB. A linked worktree is cheaper, but it shares the original's Git
directory: whoever works in it can write hooks and config that the original
repository will then run. That is fine for you and not fine for an agent you
want to confine.

The tool [git-worktree-apfs](https://github.com/dannolan/git-worktree-apfs)
makes linked worktrees from APFS clones. git-cow covers the same job with
the same Git metadata, adds Linux reflinks, adds the independent `copy`
mode, and mirrors `git worktree`'s arguments. The comparison below says what
we learned from it.

## Install

```
go install github.com/roasbeef/git-cow/cmd/git-cow@latest
```

Git runs any executable named `git-<name>` on `PATH` as `git <name>`, so the
same binary answers to `git cow add ...`.

## Quick start

Make an independent copy of a host-owned template repository:

```
git-cow copy /srv/templates/app /srv/work/ticket-123
```

Make a linked worktree with a new branch from your current checkout. Build
output and other untracked files come along:

```
git cow add -b feature-x ../app-feature-x
git cow list
git cow remove ../app-feature-x
```

## Commands

```
git-cow [-C <dir>] copy [--require-cow] [--json] <template> <dest>
git-cow [-C <dir>] add [-b <branch>] [-B <branch>] [--detach] [--force]
                       [--require-cow] [--json] <path> [<commit-ish>]
git-cow [-C <dir>] list [--porcelain]
git-cow [-C <dir>] remove [--force] <path>
git-cow [-C <dir>] prune
```

`-C <dir>` runs the command as if started in `<dir>`, as Git's `-C` does.

### copy

`<template>` must be a non-bare repository whose `.git` is a real
directory. These are refused, with exit status 2:

- a linked worktree (`.git` is a file) or a symlinked `.git`;
- a repository with `.git/objects/info/alternates`;
- a repository with any linked worktree registered in `.git/worktrees`, since
  the copy would list the host's worktree;
- a symlink anywhere under `.git`, since a write through it could reach the
  host;
- a `core.worktree` setting or a `.git/commondir` file that makes Git in the
  copy act on another path. This is found by the check after copying, so
  the copy is built and then removed;
- a destination that exists, or that is inside the template or contains it;
- a tree holding a device, socket or FIFO. This includes the socket that
  Git's fsmonitor daemon leaves at `.git/fsmonitor--daemon.ipc`, so stop the
  daemon in the template first.

The tree is built in a hidden temporary directory next to `<dest>` and
renamed into place as the last step with a rename that fails if `<dest>`
appeared meanwhile. `<dest>` therefore exists whole or not at all, and the
temporary directory is removed on every failure. `<dest>`'s parent must
already exist. Before the rename the copy is checked: `.git` is a directory,
there are no alternates, and `HEAD` equals the template's. Git runs for that
check with `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
`core.hooksPath=/dev/null` and `core.fsmonitor=false`.

`--json` prints one line:

```
{"method":"clonefile","files":3524,"bytes":143499022,"elapsed_ms":310,"head":"ae8d2539..."}
```

`method` is `clonefile`, `reflink` or `copy`. `head` is empty for a
repository with no commits. Without `--json` one short line is printed.

### add

`add` calls `git worktree add --no-checkout` so Git writes its own
metadata, then fills the directory with clones of the files of the checkout
that contains the current directory (or `-C`). Git's rules therefore hold:
a branch checked out elsewhere is refused, `--force` means what Git says,
and `git worktree list`, `remove` and `prune` work on the result, and
`git-cow list`, `remove` and `prune` work on worktrees that plain Git made.
Those three commands run `git worktree` directly.

The source checkout is not allowed to be dirty, which here means any staged
or unstaged change to a tracked file. Untracked and ignored files are fine,
and they are the point: they carry over. A dirty source is refused with a
message and status 2, because only a clean source has files that equal its
`HEAD`. Given that, the new worktree is made to match `<commit-ish>` by
copying the source index and running a hard reset, which rewrites only the
files that differ between the two commits, the same result a checkout gives.
The first `git status` in the new worktree is not slow, because the index
carries valid stat data.

Further behavior:

- Submodules are left as empty directories, which is what `git worktree add`
  does. Their `.git` files point at paths relative to the source and would
  not resolve.
- Sockets, FIFOs and devices in untracked directories are skipped.
- If the filesystem cannot clone, `add` runs plain `git worktree add`, or
  exits 3 with `--require-cow`.
- If anything fails after Git created the worktree, the worktree and any
  branch made by `-b` are removed. A branch Git named after the path's
  basename, when neither `-b` nor `-B` is given, is left behind, and an empty
  directory given as `<path>` is removed. `add` never runs `git worktree prune`.
- A commit-ish that tracks a path where the source has an untracked file
  replaces that file.

### Exit codes

| Status | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | Any other error, including bad usage. |
| 2 | A refusal: unsuitable template or destination, or a dirty source for `add`. |
| 3 | `--require-cow` was given and the filesystem cannot clone. |

`add` returns Git's own status when Git fails (usually 128). `list`,
`remove` and `prune` return Git's status.

## How cloning works

On macOS, `copy` clones the whole tree with one `clonefile(2)` call through
`golang.org/x/sys/unix`, with `CLONE_NOFOLLOW`. The man page says the call is
expected to be atomic and discourages cloning directory hierarchies. Nothing
here depends on that, because the clone goes into a temporary directory that
is renamed into place. The call does not follow symlinks. `add` clones each top-level directory that holds no
submodule the same way, and single files otherwise.

On Linux, git-cow walks the tree and clones each file with the `FICLONE`
ioctl (`unix.IoctlFileClone`). That needs a filesystem with reflinks such as
btrfs, or XFS made with reflink support.

When a clone fails with `ENOTSUP`, `EOPNOTSUPP`, `EXDEV` or `EINVAL` (or
`ENOTTY` on Linux, which some filesystems return), git-cow copies that file
byte by byte, unless `--require-cow` makes it stop with status 3. A copy is
never made with hard links, because a hard-linked file is shared for writes.
A walk keeps file modes and modification times, keeps symlinks as symlinks
without following them (including a link pointing outside the tree), and
refuses special files. Other platforms get the plain copy.

## Security model

| | `copy` | `add` |
| --- | --- | --- |
| `.git` directory | Own copy. Writes in the copy never reach the template. | Shared with the source. |
| Hooks and config | Own copies. | Shared. A hook or config written in the worktree is run by the source repository. |
| Commits and refs | Separate. | Shared. A branch made in the worktree is visible in the source. |
| Working tree files | Clones. Writes are not shared. | Clones. Writes are not shared. |
| Intended for | Untrusted code. | People working in parallel. |

Do not use `add` to confine untrusted code. Use `copy`.

What `copy` does not protect against:

- Secrets in the template are copied. That includes tokens in
  `.git/config`, credentials in tracked or untracked files, and anything in
  history. The template must be made by the host and be clean.
- Hooks in the template's `.git/hooks` are copied, and Git will run them
  when the copy's user runs Git commands. A template needs no hooks.
- A symlink that points outside the tree is kept as a symlink. It resolves
  to the same host path from inside the copy.
- The template is not locked. Copy a template that nothing is writing to.
- Disk contents the copy never changes stay shared with the template on
  disk. This is not visible through the filesystem interface, but it is not
  encryption or separation of storage either.

## Comparison with git-worktree-apfs

The source of git-worktree-apfs at commit 15abaf4 was read in full and its
binary was built and run on throwaway repositories. Its install and release
scripts were not run. It is MIT licensed. No code from it is used here; the
idea of having Git create the worktree metadata with `--no-checkout` and
then filling the directory with clones is shared by both tools.

### Features

In this section `add.go` is `internal/worktree/add.go`, and `apfs_darwin.go` and
`unsupported.go` are in `internal/clone/`, all in the git-worktree-apfs repository.

| | git-worktree-apfs | git-cow |
| --- | --- | --- |
| Command line | `add <path> --new-branch X` and `--branch X`, own flags | Git's: `add -b X`, `-B`, `--detach`, `--force`, `<path> [<commit-ish>]` |
| `list`, `remove`, `prune` | None | Same as `git worktree` |
| Independent copy (own `.git`) | No | `copy` |
| macOS | APFS only, found by running `df` and `diskutil` | Tries the clone and falls back |
| Linux | Builds, then reports unsupported (`unsupported.go:18`) | FICLONE per file, unmeasured |
| Non-CoW filesystem | Refuses | Falls back to a copy, or exit 3 with `--require-cow` |
| Untracked, non-ignored files | Source refused as dirty (`add.go:100`) | Copied |
| Dirty tracked files | Refused, or `--allow-dirty` copies them | Refused |
| Different commit-ish than source HEAD | Refused, or `--allow-mismatched-ref` shows the difference as local changes | Tracked files made to match the commit |
| Staged changes | Lost with `--allow-dirty` | Not applicable, a source with staged changes is refused |
| Submodules | Copied directory has a broken `.git` file | Empty directory, as Git does |
| Index | Rebuilt with no stat data (`add.go:193`) | Source index copied and refreshed |
| Cloning of `.git` | Clones the source `.git`, then deletes it (`apfs_darwin.go:36-54`) | Never touches it |
| Sockets and FIFOs | Not handled | Skipped by `add`, refused by `copy` |

### Measured

Template: a `git clone --no-hardlinks` of the Loom repository. It has 3,523
tracked files, a 144 MB working tree, a 415 MiB pack and one ignored 64 MiB
file. Each run makes four copies or worktrees, with three runs per row, on an Apple M4 Max with APFS
and Git 2.54. Disk growth is the change in free space reported by `df` after
`sync`, not `du`, which counts clones twice. Other processes write to the
disk, so single values vary by several MiB. Each value is the median of three
runs, with the range in brackets.

Independent copies:

| Tool | Wall time, 4 copies | Disk growth, 4 copies | First `git status` |
| --- | --- | --- | --- |
| `git clone --no-hardlinks` | 2.69 s (2.59 to 2.77) | 2,026 MiB (2,022 to 2,038) | 0.22 s |
| `git-cow copy` | 0.31 s (0.30 to 0.45) | 13 MiB (12 to 14) | 0.52 s |

Linked worktrees:

| Tool | Wall time, 4 worktrees | Disk growth, 4 worktrees | First `git status` |
| --- | --- | --- | --- |
| `git worktree add` | 1.56 s (1.53 to 1.58) | 330 MiB (324 to 464) | 0.22 s |
| `git worktree-apfs add` | 1.82 s (1.81 to 2.03) | 9.7 MiB (1.6 to 10.1) | 0.51 s |
| `git-cow add` | 2.74 s (2.68 to 2.84) | 9.0 MiB (2.5 to 9.6) | 0.03 s |

Notes on these numbers:

- `git worktree add` copies the tracked files only. The other rows also carry
  the ignored 64 MiB file.
- `git-cow add` is the slowest worktree row. Its last step runs a full
  `git status` that hashes every tracked file and writes fresh stat data
  into the index. That is why its first later `git status` takes 0.03 s
  while the others pay 0.5 s on first use. The time is moved, not removed.
- `git-cow copy` leaves the index with the template's inode numbers, so its
  first `git status` rehashes the files, as `git worktree-apfs` leaves it too.
- Linux: unmeasured. The reflink path is built and vetted for Linux and its
  fallback is covered by tests, but it has not been timed or run on a
  filesystem with reflinks.

## Limits

- Cloning needs the template and destination on the same volume of a
  filesystem that supports it. Otherwise the fallback copies bytes.
- `add` needs a source with no changes to tracked files, and does not handle
  sparse checkouts or populate submodules.
- Linux reflinks are untested on a real reflink filesystem. Windows is not a
  target; the plain copy path compiles there and is untested.
- Cloned files share blocks, so freeing space needs both sides to be
  removed or rewritten.

## Development

```
go vet ./...
go test -race ./...
go test -run '^$' -bench . ./cow
```

The tests make real repositories under `t.TempDir()` and run real Git, with
no user config. Tests that need cloning skip, with a message, only when the
filesystem returns that cloning is unsupported. The `add`, `list`, `remove`
and `prune` tests make worktrees with git-cow and with Git and manage each
with the other.

The packages are `cow` (the library, with `Copy` and `AddWorktree`) and
`cmd/git-cow`. Platform code is in `cow/platform_darwin.go`,
`cow/platform_linux.go` and `cow/platform_other.go`.

## License

See `LICENSE`.
