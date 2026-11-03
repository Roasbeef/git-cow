//go:build unix

package main

import (
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMCancelsContext checks that SIGTERM cancels the command's
// context, which is what makes a terminated copy clean up. Without the
// handler the signal would kill this test process. That cancellation leaves
// no temporary directory is tested in the cow package. A SIGTERM sent to a
// real copy part-way is not tested, because its timing cannot be made
// reliable.
func TestSIGTERMCancelsContext(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
}
