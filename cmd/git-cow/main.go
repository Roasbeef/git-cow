// Command git-cow makes copy-on-write copies of Git repositories and linked
// worktrees.
//
// "copy" makes an independent repository, which is the safe choice for code
// you do not trust. "add", "list", "remove", "prune", "move", "lock",
// "unlock" and "repair" mirror "git worktree" and share the source's Git
// directory. "add" clones ignored files such as build output and leaves the
// source's untracked files behind unless "--untracked" asks for them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/roasbeef/git-cow/cow"
)

// Exit statuses. Passthrough commands and failed Git calls return Git's own.
const (
	exitOK      = 0
	exitError   = 1
	exitRefused = 2
	exitNoCoW   = 3
)

const usage = `usage: git-cow [-C <dir>] <command> [options]

commands:
  copy [--require-cow] [--json] <template> <dest>
        make an independent repository that shares disk blocks
  add [-b <branch>] [-B <branch>] [-d|--detach] [-f|--force] [-q|--quiet]
      [--lock [--reason <string>]] [--track|--no-track]
      [--guess-remote|--no-guess-remote] [--require-cow] [--untracked]
      [--json] <path> [<commit-ish>]
        make a linked worktree whose files are clones of this checkout;
        untracked files stay behind unless --untracked is given
  list [--porcelain]         same as git worktree list
  remove [--force] <path>    same as git worktree remove
  prune                      same as git worktree prune
  move <worktree> <new-path> same as git worktree move
  lock <worktree>            same as git worktree lock
  unlock <worktree>          same as git worktree unlock
  repair [<path>...]         same as git worktree repair
`

const copyUsage = "usage: git-cow copy [--require-cow] [--json] <template> <dest>\n"

const addUsage = "usage: git-cow add [-b <branch>] [-B <branch>] [--detach] " +
	"[--force] [--quiet] [--lock [--reason <string>]] [--track|--no-track] " +
	"[--guess-remote|--no-guess-remote] [--require-cow] [--untracked] [--json] " +
	"<path> [<commit-ish>]\n"

func main() {
	ctx, stop := signalContext()
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// signalContext returns a context cancelled by SIGINT or SIGTERM, so a copy
// that is interrupted or terminated removes its temporary directory.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// run executes one command line and returns the process exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir := "."

	for len(args) > 0 {
		if args[0] == "-C" {
			if len(args) < 2 {
				_, _ = fmt.Fprint(stderr, "git-cow: -C needs a directory\n")

				return exitError
			}

			dir = inDir(dir, args[1])
			args = args[2:]

			continue
		}

		if after, ok := strings.CutPrefix(args[0], "-C"); ok && after != "" {
			dir = inDir(dir, after)
			args = args[1:]

			continue
		}

		break
	}

	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)

		return exitError
	}

	sub, rest := args[0], args[1:]

	switch sub {
	case "copy":
		return runCopy(ctx, dir, rest, stdout, stderr)

	case "add":
		return runAdd(ctx, dir, rest, stdout, stderr)

	case "list", "remove", "prune", "move", "lock", "unlock", "repair":
		return cow.Passthrough(ctx, dir, sub, rest, stdout, stderr)

	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)

		return exitOK
	}

	_, _ = fmt.Fprintf(stderr, "git-cow: unknown command %q\n%s", sub, usage)

	return exitError
}

// resultJSON is the single line "copy --json" and "add --json" print.
type resultJSON struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Head      string `json:"head"`
}

// runCopy implements "git-cow copy".
func runCopy(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) int {
	var (
		requireCOW bool
		asJSON     bool
		positional []string
	)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch arg {
		case "-h", "--help":
			_, _ = fmt.Fprint(stdout, copyUsage)

			return exitOK

		case "--require-cow":
			requireCOW = true

		case "--json":
			asJSON = true

		case "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)

		default:
			if len(arg) > 1 && arg[0] == '-' {
				_, _ = fmt.Fprintf(stderr, "git-cow copy: unsupported option %q\n", arg)

				return exitError
			}

			positional = append(positional, arg)
		}
	}

	if len(positional) != 2 {
		_, _ = fmt.Fprint(stderr, copyUsage)

		return exitError
	}

	template, dest := inDir(dir, positional[0]), inDir(dir, positional[1])

	res, err := cow.Copy(ctx, template, dest, cow.Options{RequireCOW: requireCOW})
	if err != nil {
		return fail(stderr, err)
	}

	if asJSON {
		return printJSON(stdout, stderr, resultJSON{
			Method:    res.Method,
			Path:      res.Path,
			Files:     res.Files,
			Bytes:     res.Bytes,
			ElapsedMS: res.Elapsed.Milliseconds(),
			Head:      res.Head,
		})
	}

	_, _ = fmt.Fprintf(stdout, "copied %d files (%d bytes) by %s in %dms to %s\n",
		res.Files, res.Bytes, res.Method, res.Elapsed.Milliseconds(), dest)

	return exitOK
}

// runAdd implements "git-cow add".
func runAdd(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) int {
	opts := cow.AddOptions{Dir: dir}
	asJSON := false

	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "-h" || arg == "--help":
			_, _ = fmt.Fprint(stdout, addUsage)

			return exitOK

		case arg == "-b" || arg == "-B":
			if i+1 >= len(args) {
				_, _ = fmt.Fprintf(stderr, "git-cow add: %s needs a branch name\n", arg)

				return exitError
			}

			i++
			if arg == "-b" {
				opts.Branch = args[i]
			} else {
				opts.ResetBranch = args[i]
			}

		case arg == "--detach" || arg == "-d":
			opts.Detach = true

		case arg == "--force" || arg == "-f":
			opts.Force = true

		case arg == "--quiet" || arg == "-q":
			opts.Quiet = true

		case arg == "--lock":
			opts.Lock = true

		case arg == "--reason":
			if i+1 >= len(args) {
				_, _ = fmt.Fprint(stderr, "git-cow add: --reason needs a message\n")

				return exitError
			}

			i++
			opts.LockReason = args[i]

		case strings.HasPrefix(arg, "--reason="):
			opts.LockReason = strings.TrimPrefix(arg, "--reason=")

		case arg == "--track":
			opts.Track = true

		case arg == "--no-track":
			opts.NoTrack = true

		case arg == "--guess-remote":
			opts.GuessRemote = true

		case arg == "--no-guess-remote":
			opts.NoGuessRemote = true

		case arg == "--require-cow":
			opts.RequireCOW = true

		case arg == "--untracked":
			opts.Untracked = true

		case arg == "--json":
			asJSON = true

		case arg == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)

		default:
			if len(arg) > 1 && arg[0] == '-' {
				_, _ = fmt.Fprintf(stderr, "git-cow add: unsupported option %q\n", arg)

				return exitError
			}

			positional = append(positional, arg)
		}
	}

	if len(positional) < 1 || len(positional) > 2 {
		_, _ = fmt.Fprint(stderr, addUsage)

		return exitError
	}

	opts.Path = positional[0]
	if len(positional) == 2 {
		opts.Commitish = positional[1]
	}

	res, err := cow.AddWorktree(ctx, opts)
	if err != nil {
		return fail(stderr, err)
	}

	if asJSON {
		return printJSON(stdout, stderr, resultJSON{
			Method:    res.Method,
			Path:      res.Path,
			Files:     res.Files,
			Bytes:     res.Bytes,
			ElapsedMS: res.Elapsed.Milliseconds(),
			Head:      res.Head,
		})
	}

	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "created worktree %s by %s in %dms at %.12s\n",
			res.Path, res.Method, res.Elapsed.Milliseconds(), res.Head)
	}

	return exitOK
}

// inDir resolves p against dir when p is relative.
func inDir(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}

	return filepath.Join(dir, p)
}

// printJSON writes v as one line.
func printJSON(stdout, stderr io.Writer, v any) int {
	line, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "git-cow:", err)

		return exitError
	}

	_, _ = fmt.Fprintf(stdout, "%s\n", line)

	return exitOK
}

// fail prints err and maps it to an exit status.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "git-cow: %v\n", err)

	var (
		refusal *cow.RefusalError
		gitErr  *cow.GitError
	)

	switch {
	case errors.As(err, &refusal):
		return exitRefused

	case errors.Is(err, cow.ErrNoCoW):
		return exitNoCoW

	case errors.As(err, &gitErr) && gitErr.ExitCode > 0:
		return gitErr.ExitCode
	}

	return exitError
}
