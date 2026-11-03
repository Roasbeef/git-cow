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
