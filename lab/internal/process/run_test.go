package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// watchedRun is a run under a Watchdog whose clock and sampling ticker the test
// drives.
type watchedRun struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	sampled chan struct{}
	ended   chan struct{}
	got     chan Result
}

// drivenWatchdog returns a Watchdog the test drives through the returned run.
func drivenWatchdog(wall time.Duration) (*Watchdog, *watchedRun) {
	run := &watchedRun{now: time.Unix(1_700_000_000, 0), ticks: make(chan time.Time), sampled: make(chan struct{}, 16), ended: make(chan struct{}), got: make(chan Result, 1)}
	w := &Watchdog{
		Wall: wall,
		// Each clock read marks a sample as taken: the start, then one per tick.
		Now: func() time.Time {
			run.mu.Lock()
			now := run.now
			run.mu.Unlock()
			select {
			case run.sampled <- struct{}{}:
			default:
			}
			return now
		},
		Tick: func(time.Duration) (<-chan time.Time, func()) { return run.ticks, func() {} },
	}
	return w, run
}

// start runs o in the background and waits until the watchdog read its start time.
func (r *watchedRun) start(o Options) {
	go func() {
		result := Run(context.Background(), o)
		r.got <- result
		close(r.ended)
	}()
	select {
	case <-r.sampled:
	case <-r.ended:
	}
}

// tick advances the clock by d and delivers one sample, waiting until the watchdog
// has read the clock for it, so the next advance never leaks into this sample; it
// reports false when the run had already ended, so a test never blocks on a
// watchdog that has stopped.
func (r *watchedRun) tick(d time.Duration) bool {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
	select {
	case r.ticks <- time.Time{}:
		<-r.sampled
		return true
	case <-r.ended:
		return false
	}
}

// TestWatchdogNeverStopsQuietWorkingRun: a run with no output for far longer than any
// quiet test phase, whose work (as in short-lived child processes) never shows as
// output, is never stopped while its wall clock stays under budget. The clock and
// samples are injected, so no wall-clock budget is involved.
func TestWatchdogNeverStopsQuietWorkingRun(t *testing.T) {
	t.Parallel()
	w, run := drivenWatchdog(time.Hour)
	o := options("wait")
	o.Watchdog = w
	run.start(o)
	// A watchdog that stopped the run takes no further sample, so each taken tick
	// shows the quiet run was still watched.
	for i := 0; i < 11; i++ {
		if !run.tick(5 * time.Minute) {
			t.Fatalf("a quiet run under its wall budget was stopped at sample %d: %+v", i, <-run.got)
		}
	}
	select {
	case r := <-run.got:
		t.Fatalf("a quiet run under its wall budget ended: %+v", r)
	default:
	}
	// Past the budget, as after the machine slept, it ends as stalled.
	if !run.tick(3 * time.Hour) {
		t.Fatalf("the run ended before its watchdog sampled it: %+v", <-run.got)
	}
	r := <-run.got
	if r.OK || !r.Stalled || !r.Interrupted || !r.Incomplete() || !strings.HasPrefix(r.Error, "interrupted: stalled: ") || !strings.Contains(r.Error, "wall clock passed the 1h0m0s budget") || !r.Reconciled {
		t.Fatalf("%+v", r)
	}
	if r.TerminalState() != StateInterrupted {
		t.Fatalf("terminal state %s", r.TerminalState())
	}
}

// TestWatchdogWallBudgetAfterSleep: a single sample far past the wall budget (as after
// sleep) ends the run as stalled; a run that ends on its own under a watchdog is an
// ordinary result.
func TestWatchdogWallBudgetAfterSleep(t *testing.T) {
	t.Parallel()
	w, run := drivenWatchdog(10 * time.Minute)
	o := options("wait")
	o.Watchdog = w
	run.start(o)
	if !run.tick(3 * time.Hour) { // the machine slept
		t.Fatalf("the run ended before its watchdog sampled it: %+v", <-run.got)
	}
	r := <-run.got
	if !r.Stalled || !r.Interrupted || !strings.Contains(r.Error, "wall clock passed the 10m0s budget") {
		t.Fatalf("%+v", r)
	}
	quick, _ := drivenWatchdog(time.Hour)
	o = options("echo")
	o.Stdin = []byte("ok")
	o.Watchdog = quick
	if r := Run(context.Background(), o); !r.OK || r.Stalled || r.Interrupted || r.Stdout != "ok" {
		t.Fatalf("%+v", r)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("FORGECELL_TEST_PROCESS") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "echo":
		io.Copy(os.Stdout, os.Stdin)
	case "unicode":
		for _, b := range []byte("Lab 🧪 café שלום") {
			os.Stdout.Write([]byte{b})
			os.Stderr.Write([]byte{b})
		}
		os.Exit(7)
	case "tree":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "sentinel")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if pidFile := os.Getenv("FORGECELL_TEST_CHILD_PID"); pidFile != "" {
			os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0600)
		}
		fmt.Fprint(os.Stdout, "child started")
		if ready := os.Getenv("FORGECELL_TEST_READY"); ready != "" {
			os.WriteFile(ready, nil, 0600)
		}
		time.Sleep(60 * time.Second)
	case "sentinel":
		time.Sleep(20 * time.Second)
		os.WriteFile(os.Getenv("FORGECELL_TEST_SENTINEL"), []byte("escaped"), 0600)
	case "wait":
		time.Sleep(60 * time.Second)
	case "flood":
		for i := 0; i < 100000; i++ {
			fmt.Fprint(os.Stdout, "0123456789")
			fmt.Fprint(os.Stderr, "0123456789")
		}
	}
	os.Exit(0)
}
func options(mode string) Options {
	return Options{Argv: []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode}, Env: append(os.Environ(), "FORGECELL_TEST_PROCESS=1"), Timeout: successBackstop}
}
func TestStructuredInputIsData(t *testing.T) {
	o := options("echo")
	o.Input = map[string]string{"ticket": "$(touch should-not-exist); 'quoted'"}
	o.Dir = t.TempDir()
	r := Run(context.Background(), o)
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got["ticket"] != o.Input.(map[string]string)["ticket"] {
		t.Fatal("input changed")
	}
	if entries, _ := os.ReadDir(o.Dir); len(entries) != 0 {
		t.Fatal("input executed")
	}
}
func TestUnicodeAndNonzeroExit(t *testing.T) {
	r := Run(context.Background(), options("unicode"))
	if r.OK || r.Code != 7 || r.Stdout != "Lab 🧪 café שלום" || r.Stderr != r.Stdout {
		t.Fatalf("%+v", r)
	}
}
func TestTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	o := options("wait")
	o.Timeout = stallTimeout
	r := Run(context.Background(), o)
	if r.OK || !r.TimedOut || r.ElapsedMS >= 60000 {
		t.Fatalf("%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = Run(ctx, options("wait"))
	if r.OK || !r.Interrupted {
		t.Fatalf("%+v", r)
	}
}
func TestOutputIsBoundedAndOverflowFails(t *testing.T) {
	o := options("flood")
	o.MaxOutputBytes = 1024
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || len(r.Stdout)+len(r.Stderr) > 1024 {
		t.Fatalf("overflow=%v ok=%v bytes=%d", r.Overflow, r.OK, len(r.Stdout)+len(r.Stderr))
	}
}
func TestMissingExecutableAndInvalidInput(t *testing.T) {
	r := Run(context.Background(), Options{Argv: []string{"/nonexistent/forgecell-test"}})
	if r.OK || r.Error == "" {
		t.Fatalf("%+v", r)
	}
	r = Run(context.Background(), Options{})
	if r.OK || !strings.Contains(r.Error, "argv") {
		t.Fatalf("%+v", r)
	}
	o := options("echo")
	o.Input = make(chan int)
	r = Run(context.Background(), o)
	if r.OK || r.Error == "" {
		t.Fatal("invalid JSON input accepted")
	}
}

func TestCancellationStopsDescendants(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process tree cancellation is only supported on release target operating systems")
	}
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "escaped")
	pidFile := filepath.Join(dir, "child.pid")
	ready := filepath.Join(dir, "ready")
	// The run expires once the fixture has recorded its child and printed, not
	// after a real deadline that start-up under load could race; the fixture
	// and its child sleep past the marker backstop.
	o := options("tree")
	o.Timeout = time.Hour
	o.Env = append(o.Env, "FORGECELL_TEST_SENTINEL="+marker, "FORGECELL_TEST_CHILD_PID="+pidFile, "FORGECELL_TEST_READY="+ready)
	r := Run(expireOnMarker(t, ready, false), o)
	if !r.TimedOut || !strings.Contains(r.Stdout, "child started") || !r.Reconciled {
		t.Fatalf("fixture did not start its child: %+v", r)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fixture never recorded its descendant: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("unusable descendant pid %q", raw)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	// The descendant sleeps well past the timeout, so seeing it gone proves the
	// group stop rather than a natural exit; a killed process may linger as a
	// zombie until init reaps it, so poll instead of assuming it is already gone.
	for deadline := time.Now().Add(markerBackstop); proc.Signal(syscall.Signal(0)) == nil; time.Sleep(25 * time.Millisecond) {
		if time.Now().After(deadline) {
			proc.Kill()
			t.Fatal("descendant survived cancellation")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation")
	}
}

func TestRawProviderInputIsNotJSONQuoted(t *testing.T) {
	o := options("echo")
	o.Stdin = []byte("Instruction\n{\"kind\":\"molecule\"}\n")
	r := Run(context.Background(), o)
	if !r.OK || r.Stdout != string(o.Stdin) {
		t.Fatalf("%+v", r)
	}
	o.Input = map[string]string{"kind": "molecule"}
	r = Run(context.Background(), o)
	if r.OK {
		t.Fatal("ambiguous raw and JSON input accepted")
	}
}
