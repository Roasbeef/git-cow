package cow

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// benchRepo generates a repository with a few thousand small files and one
// large incompressible blob, packed, so the object store dominates its size.
func benchRepo(b *testing.B) string {
	b.Helper()
	isolateGit(b)

	dir := filepath.Join(b.TempDir(), "tpl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		b.Fatal(err)
	}

	git(b, dir, "init", "-q", "-b", "main")

	rng := rand.New(rand.NewPCG(1, 2))
	buf := make([]byte, 4096)

	for i := range 3000 {
		for j := range buf {
			buf[j] = byte(rng.UintN(256))
		}

		p := filepath.Join(dir, fmt.Sprintf("d%d", i%50), fmt.Sprintf("f%d.bin", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}

		if err := os.WriteFile(p, buf, 0o644); err != nil {
			b.Fatal(err)
		}
	}

	big := make([]byte, 32<<20)
	for i := range big {
		big[i] = byte(rng.UintN(256))
	}

	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		b.Fatal(err)
	}

	git(b, dir, "add", "-A")
	git(b, dir, "commit", "-q", "-m", "bench")
	git(b, dir, "gc", "-q")

	return dir
}

// BenchmarkCopy times Copy on a generated repository, once with the
// platform's cloning and once with cloning disabled. The method each run
// used is logged and the file count is reported as a metric.
func BenchmarkCopy(b *testing.B) {
	tpl := benchRepo(b)

	for _, mode := range []struct {
		name string
		opts Options
	}{
		{"native", Options{}},
		{"plain-copy", Options{forceNoCoW: true}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			parent := b.TempDir()

			var last Result

			for i := 0; i < b.N; i++ {
				dest := filepath.Join(parent, fmt.Sprintf("c%d", i))

				res, err := Copy(context.Background(), tpl, dest, mode.opts)
				if err != nil {
					b.Fatal(err)
				}

				last = res

				b.StopTimer()
				removeAll(dest)
				b.StartTimer()
			}

			b.ReportMetric(float64(last.Files), "files/op")
			b.ReportMetric(float64(last.Bytes)/(1<<20), "MiB/op")
			b.Logf("method=%s", last.Method)
		})
	}
}

// BenchmarkAddWorktree times AddWorktree on a generated repository.
func BenchmarkAddWorktree(b *testing.B) {
	src := benchRepo(b)

	for _, mode := range []struct {
		name string
		opts AddOptions
	}{
		{"native", AddOptions{}},
		{"walk", AddOptions{forceWalk: true}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			parent := b.TempDir()

			var last AddResult

			for i := 0; i < b.N; i++ {
				dest := filepath.Join(parent, fmt.Sprintf("wt%d", i))
				opts := mode.opts
				opts.Dir = src
				opts.Path = dest
				opts.Detach = true

				res, err := AddWorktree(context.Background(), opts)
				if err != nil {
					b.Fatal(err)
				}

				last = res

				b.StopTimer()
				git(b, src, "worktree", "remove", "--force", dest)
				b.StartTimer()
			}

			b.ReportMetric(float64(last.Files), "files/op")
			b.ReportMetric(float64(last.Bytes)/(1<<20), "MiB/op")
			b.Logf("method=%s", last.Method)
		})
	}
}

// untrackedBenchRepo generates a repository with a large ignored build
// directory and many untracked files, some in directories of their own and
// some inside tracked directories.
func untrackedBenchRepo(b *testing.B) string {
	b.Helper()
	isolateGit(b)

	dir := filepath.Join(b.TempDir(), "tpl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		b.Fatal(err)
	}

	git(b, dir, "init", "-q", "-b", "main")
	write(b, filepath.Join(dir, ".gitignore"), "build/\n")

	for i := range 50 {
		write(b, filepath.Join(dir, fmt.Sprintf("src%d", i), "tracked.go"), "package x\n")
	}

	git(b, dir, "add", "-A")
	git(b, dir, "commit", "-q", "-m", "bench")

	blob := make([]byte, 8192)
	for i := range 20000 {
		p := filepath.Join(dir, "build", fmt.Sprintf("o%d", i%200), fmt.Sprintf("f%d.o", i))
		write(b, p, string(blob[:1+i%4096]))
	}

	for i := range 10000 {
		var p string
		switch {
		case i%2 == 0:
			p = filepath.Join(dir, fmt.Sprintf("src%d", i%50), fmt.Sprintf("scratch%d.txt", i))
		default:
			p = filepath.Join(dir, "scratch", fmt.Sprintf("d%d", i%100), fmt.Sprintf("n%d.txt", i))
		}

		write(b, p, "scratch\n")
	}

	return dir
}

// BenchmarkAddWorktreeUntracked times AddWorktree on a repository with a
// large ignored build directory and many untracked files, once cleaning the
// untracked files out (the default) and once keeping them.
func BenchmarkAddWorktreeUntracked(b *testing.B) {
	src := untrackedBenchRepo(b)

	for _, mode := range []struct {
		name      string
		untracked bool
	}{
		{"cleaned", false},
		{"kept", true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			parent := b.TempDir()

			for i := 0; i < b.N; i++ {
				dest := filepath.Join(parent, fmt.Sprintf("wt%d", i))

				res, err := AddWorktree(context.Background(), AddOptions{
					Dir: src, Path: dest, Detach: true, Untracked: mode.untracked,
				})
				if err != nil {
					b.Fatal(err)
				}

				if i == 0 {
					b.ReportMetric(float64(res.Files), "files/op")
					b.Logf("method=%s", res.Method)
				}

				b.StopTimer()
				git(b, src, "worktree", "remove", "--force", dest)
				b.StartTimer()
			}
		})
	}
}
