package molecule

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// writeStub writes an executable stub script and is a drop-in for os.WriteFile.
// A child forked by any parallel test while this process still holds the
// script open for writing inherits that handle until it execs, and exec of the
// script fails with "text file busy" (ETXTBSY) meanwhile. The stubs are
// executed by production code that a test cannot wrap in a retry, so instead
// of retrying the exec the write holds syscall.ForkLock for reading: every
// fork in this process waits until the handle is closed, so no child can carry
// it and the exec never meets a busy file.
func writeStub(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(name, data, perm)
}

// expireWhenStarted returns a context that is cancelled with
// context.DeadlineExceeded as soon as marker exists. process.Run reports that
// cause as its own timeout, so a test reaches a harness or check timeout once
// the fake process has provably started instead of waiting a real deadline
// out. The Formula timeout under test must stay far above any start-up delay,
// so that only the marker ends the run, and the context must only reach the
// one run whose process writes the marker. The returned stop releases the
// watcher.
func expireWhenStarted(t *testing.T, marker string) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for ctx.Err() == nil {
			if _, err := os.Stat(marker); err == nil {
				cancel(context.DeadlineExceeded)
				return
			}
			// A process that never starts is reported as an interruption, which
			// no timeout assertion accepts.
			if time.Now().After(deadline) {
				cancel(context.Canceled)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	stop := func() { cancel(context.Canceled) }
	t.Cleanup(stop)
	return ctx, stop
}
