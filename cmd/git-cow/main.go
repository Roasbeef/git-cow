// Command git-cow makes copy-on-write copies of Git repositories and linked
// worktrees.
//
// "copy" makes an independent repository, which is the safe choice for code
// you do not trust. "add", "list", "remove" and "prune" mirror
// "git worktree" and share the source's Git directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
  add [-b <branch>] [-B <branch>] [--detach] [--force] [--require-cow] [--json] <path> [<commit-ish>]
        make a linked worktree whose files are clones of this checkout
  list [--porcelain]      same as git worktree list
  remove [--force] <path> same as git worktree remove
  prune                   same as git worktree prune
`

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

	if len(args) >= 2 && args[0] == "-C" {
		dir, args = args[1], args[2:]
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

	case "list", "remove", "prune":
		return cow.Passthrough(ctx, dir, sub, rest, stdout, stderr)

	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)

		return exitOK
	}

	_, _ = fmt.Fprintf(stderr, "git-cow: unknown command %q\n%s", sub, usage)

	return exitError
}

// copyJSON is the single line "copy --json" prints.
type copyJSON struct {
	Method    string `json:"method"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Head      string `json:"head"`
}

// addJSON is the single line "add --json" prints.
type addJSON struct {
	copyJSON
	Path string `json:"path"`
}

// runCopy implements "git-cow copy".
func runCopy(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	fs.SetOutput(stderr)

	requireCOW := fs.Bool("require-cow", false, "fail instead of falling back to a plain copy")
	asJSON := fs.Bool("json", false, "print one JSON line")

	if err := fs.Parse(args); err != nil {
		return exitError
	}

	if fs.NArg() != 2 {
		_, _ = fmt.Fprint(stderr, "usage: git-cow copy [--require-cow] [--json] <template> <dest>\n")

		return exitError
	}

	template, dest := inDir(dir, fs.Arg(0)), inDir(dir, fs.Arg(1))

	res, err := cow.Copy(ctx, template, dest, cow.Options{RequireCOW: *requireCOW})
	if err != nil {
		return fail(stderr, err)
	}

	j := copyJSON{res.Method, res.Files, res.Bytes, res.Elapsed.Milliseconds(), res.Head}
	if *asJSON {
		return printJSON(stdout, stderr, j)
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

		switch arg {
		case "-b", "-B":
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

		case "--detach", "-d":
			opts.Detach = true

		case "--force", "-f":
			opts.Force = true

		case "--require-cow":
			opts.RequireCOW = true

		case "--json":
			asJSON = true

		default:
			if len(arg) > 1 && arg[0] == '-' {
				_, _ = fmt.Fprintf(stderr, "git-cow add: unsupported option %q\n", arg)

				return exitError
			}

			positional = append(positional, arg)
		}
	}

	if len(positional) < 1 || len(positional) > 2 {
		_, _ = fmt.Fprint(stderr, "usage: git-cow add [-b <branch>] [-B <branch>] [--detach] "+
			"[--force] [--require-cow] [--json] <path> [<commit-ish>]\n")

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
		return printJSON(stdout, stderr, addJSON{
			copyJSON{res.Method, res.Files, res.Bytes, res.Elapsed.Milliseconds(), res.Head},
			res.Path,
		})
	}

	_, _ = fmt.Fprintf(stdout, "created worktree %s by %s in %dms at %.12s\n",
		res.Path, res.Method, res.Elapsed.Milliseconds(), res.Head)

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
