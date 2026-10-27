// Package process invokes approved argv without interpreting ticket text as shell code.
package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

var (
	errOutputLimit = errors.New("command output exceeded the configured byte limit")
	errStreamLimit = errors.New("command output stream exceeded the hard byte limit")
	errLineLimit   = errors.New("command output line exceeded the length limit")
)

// Hard ceilings for a streamed stdout. A LineSink may ask for less, never more.
const (
	StreamHardLimitBytes = 64 << 20
	StreamMaxLineBytes   = 1 << 20
)

// LineSink opts a caller in to reading the complete stdout line by line while the
// process runs. Reaching MaxOutputBytes then only truncates the retained copy; the
// stream itself is bounded by MaxBytes and MaxLineBytes, and the run fails closed
// (Overflow, process group stopped) beyond either bound or when Line returns an
// error. Line must not retain the slice, and stderr keeps its cap.
type LineSink struct {
	Line         func(line []byte) error
	MaxBytes     int64
	MaxLineBytes int
}

func (s LineSink) limits() (int64, int) {
	total, line := s.MaxBytes, s.MaxLineBytes
	if total <= 0 || total > StreamHardLimitBytes {
		total = StreamHardLimitBytes
	}
	if line <= 0 || line > StreamMaxLineBytes {
		line = StreamMaxLineBytes
	}
	return total, line
}

type Options struct {
	Stdin          []byte // Raw provider input; mutually exclusive with Input.
	Argv           []string
	Dir            string
	Env            []string
	Input          any
	Timeout        time.Duration
	MaxOutputBytes int
	// StdoutLines is nil for every caller that keeps the capture cap and overflow.
	StdoutLines *LineSink
	// Watchdog is nil for every caller except verification checks, which end a stuck
	// run as interrupted: stalled instead of letting it run for hours.
	Watchdog *Watchdog
}

// DefaultWatchdogInterval paces a Watchdog's samples when it names none.
const DefaultWatchdogInterval = 30 * time.Second

// Watchdog ends a run as stalled when the wall clock has passed Wall since it
// started. The process timeout runs on the monotonic clock, which does not advance
// while the machine sleeps; the watchdog reads the wall clock, so a run that slept
// far past its budget still ends. A zero Wall disables it. It never judges a run by
// silence or CPU use: a quiet check whose work happens in short-lived child processes
// is still working, and only its own timeout or the wall clock ends it.
type Watchdog struct {
	Wall     time.Duration
	Interval time.Duration
	// Seams for tests; nil uses the wall clock and a real ticker.
	Now  func() time.Time
	Tick func(interval time.Duration) (<-chan time.Time, func())
}

// StalledError is the cancellation cause of a run its Watchdog stopped. It unwraps to
// context.Canceled, so the run is recorded as interrupted and therefore incomplete.
type StalledError struct{ Reason string }

func (e *StalledError) Error() string { return "stalled: " + e.Reason }
func (e *StalledError) Unwrap() error { return context.Canceled }

func wallNow() time.Time { return time.Now().Round(0) }

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// watch samples the run until done is closed or it stops the run as stalled.
func (w *Watchdog) watch(c *capture, done <-chan struct{}) {
	now, tick := w.Now, w.Tick
	if now == nil {
		now = wallNow
	}
	if tick == nil {
		tick = realTicker
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultWatchdogInterval
	}
	ticks, stop := tick(interval)
	defer stop()
	start := now()
	for {
		select {
		case <-done:
			return
		case <-ticks:
		}
		at := now()
		if w.Wall > 0 && at.Sub(start) >= w.Wall {
			reason := fmt.Sprintf("the wall clock passed the %s budget (%s elapsed; the machine may have slept)", w.Wall, at.Sub(start).Round(time.Second))
			c.Lock()
			c.stalled = true
			c.Unlock()
			c.cancel(&StalledError{Reason: reason})
			return
		}
	}
}

type Result struct {
	Reconciled  bool   `json:"reconciled"`
	Uncertainty string `json:"uncertainty,omitempty"`
	RawStdout   string `json:"rawStdout,omitempty"`
	OK          bool   `json:"ok"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Code        int    `json:"code"`
	TimedOut    bool   `json:"timedOut"`
	Interrupted bool   `json:"interrupted"`
	// Stalled means the run's Watchdog stopped it; Interrupted is then true too and
	// Error starts with "interrupted: stalled:".
	Stalled  bool `json:"stalled,omitempty"`
	Overflow bool `json:"overflow"`
	// ParentDeadline means the caller's context deadline expired before
	// Options.Timeout could. It is exclusive with TimedOut, which is only the
	// process's own timeout. A parent cancelled with a deadline cause but carrying
	// no deadline cannot be told apart and stays TimedOut.
	ParentDeadline bool `json:"parentDeadline,omitempty"`
	// Truncated means a streamed run produced more stdout than MaxOutputBytes, so
	// Stdout is only its prefix. Overflow stays false unless a stream bound failed.
	Truncated bool   `json:"truncated,omitempty"`
	ElapsedMS int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`
	// PermissionDenials records bounded summaries of tool calls the harness
	// denied during an otherwise valid run; they are evidence, not failure.
	PermissionDenials []string `json:"permissionDenials,omitempty"`
	// Activity is nil when no pipe observation was recorded (older records, or a
	// process that never started). A non-nil value with zero bytes is a recorded
	// silent run.
	Activity *Activity `json:"activity,omitempty"`
}

// Incomplete reports a run that was cut short by its own timeout, a caller
// deadline, cancellation or the output cap; its output cannot be trusted as the
// process's full answer.
func (r Result) Incomplete() bool {
	return r.TimedOut || r.ParentDeadline || r.Interrupted || r.Overflow
}

// Activity counts bytes observed on the child's stdout and stderr pipes,
// including bytes dropped past the output cap. It is derived only from those
// pipes: never from PID liveness, reconciliation or output content. Silence is
// not proof of a stall, and output is not proof of progress.
type Activity struct {
	StdoutBytes int64 `json:"stdoutBytes"`
	StderrBytes int64 `json:"stderrBytes"`
	// LastOutputMS is the offset from process start of the last observed output
	// byte; absent when none was observed.
	LastOutputMS *int64 `json:"lastOutputMs,omitempty"`
}

// TerminalState is a provider-neutral reading of how a run ended.
type TerminalState string

const (
	StateSucceeded      TerminalState = "succeeded"
	StateTimeout        TerminalState = "timeout"
	StateParentDeadline TerminalState = "parent-deadline"
	StateInterrupted    TerminalState = "interrupted"
	StateOutputLimit    TerminalState = "output-limit"
	StateUnreconciled   TerminalState = "unreconciled"
	StateFailed         TerminalState = "failed"
	StateUnknown        TerminalState = "unknown"
)

// ReconnectUnknown is the only reconnect evidence reported: no provider-neutral
// reconnect signal exists, so none is invented.
const ReconnectUnknown = "unknown"

// TerminalState classifies the run from its recorded flags alone. An unreconciled
// process outranks the cause that stopped it, because it may still be running.
// Timeout, parent deadline and interruption come from the first context cause, so
// they outrank the output cap, which can also be reached while the process is
// being stopped.
func (r Result) TerminalState() TerminalState {
	switch {
	case !r.Reconciled && r.Uncertainty != "":
		return StateUnreconciled
	case r.TimedOut:
		return StateTimeout
	case r.ParentDeadline:
		return StateParentDeadline
	case r.Interrupted:
		return StateInterrupted
	case r.Overflow:
		return StateOutputLimit
	case r.OK:
		return StateSucceeded
	case r.Code != 0 || r.Error != "":
		return StateFailed
	}
	return StateUnknown
}

// Diagnostics is the persisted, secret-free summary of a run: only an enumerated
// state, byte counts and millisecond offsets, never output text, argv or env.
type Diagnostics struct {
	ElapsedMS     int64         `json:"elapsedMs"`
	Activity      *Activity     `json:"activity,omitempty"`
	TerminalState TerminalState `json:"terminalState"`
	Reconnect     string        `json:"reconnect"`
}

func (r Result) Diagnostics() Diagnostics {
	d := Diagnostics{ElapsedMS: r.ElapsedMS, TerminalState: r.TerminalState(), Reconnect: ReconnectUnknown}
	if r.Activity != nil {
		a := *r.Activity
		if a.LastOutputMS != nil {
			last := *a.LastOutputMS
			a.LastOutputMS = &last
		}
		d.Activity = &a
	}
	return d
}

type capture struct {
	sync.Mutex
	left           int
	stdout, stderr bytes.Buffer
	cancel         context.CancelCauseFunc
	overflow       bool
	started        time.Time
	stdoutBytes    int64
	stderrBytes    int64
	lastOutput     time.Duration
	sawOutput      bool
	sink           *LineSink
	maxStream      int64
	maxLine        int
	partial        []byte
	truncated      bool
	sinkClosed     bool
	stalled        bool
}
type stream struct {
	capture *capture
	stderr  bool
}

// stop fails the run closed and cancels the process group with the given cause.
func (c *capture) stop(cause error) {
	c.overflow = true
	c.cancel(cause)
}

// feed splits stdout into lines for the sink, holding at most one bounded partial line.
func (c *capture) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(c.partial)+len(p) > c.maxLine {
				c.stop(errLineLimit)
				return
			}
			c.partial = append(c.partial, p...)
			return
		}
		line := p[:i]
		if len(c.partial) > 0 {
			if len(c.partial)+i > c.maxLine {
				c.stop(errLineLimit)
				return
			}
			c.partial = append(c.partial, line...)
			line = c.partial
		} else if i > c.maxLine {
			c.stop(errLineLimit)
			return
		}
		err := c.sink.Line(line)
		c.partial = c.partial[:0]
		if err != nil {
			c.stop(err)
			return
		}
		p = p[i+1:]
	}
}

// writeStreamed keeps the capped prefix of stdout and feeds the whole of it to the
// sink. The caller holds the capture lock.
func (c *capture) writeStreamed(p []byte) {
	if c.sinkClosed {
		return
	}
	keep := min(len(p), c.left)
	c.left -= keep
	c.stdout.Write(p[:keep])
	if keep < len(p) {
		c.truncated = true
	}
	if c.overflow {
		return
	}
	if c.stdoutBytes > c.maxStream {
		c.stop(errStreamLimit)
		return
	}
	c.feed(p)
}

func (s stream) Write(p []byte) (int, error) {
	c := s.capture
	c.Lock()
	defer c.Unlock()
	n := len(p)
	if n > 0 {
		if s.stderr {
			c.stderrBytes += int64(n)
		} else {
			c.stdoutBytes += int64(n)
		}
		c.lastOutput = time.Since(c.started)
		c.sawOutput = true
	}
	if c.sink != nil && !s.stderr {
		c.writeStreamed(p)
		return n, nil
	}
	keep := min(n, c.left)
	c.left -= keep
	if s.stderr {
		c.stderr.Write(p[:keep])
	} else {
		c.stdout.Write(p[:keep])
	}
	if keep < n {
		c.overflow = true
		c.cancel(errOutputLimit)
	}
	return n, nil
}
func Run(parent context.Context, o Options) (result Result) {
	started := time.Now()
	result.Code = -1
	result.Reconciled = true // No process has been started yet.
	defer func() { result.ElapsedMS = time.Since(started).Milliseconds() }()
	if len(o.Argv) == 0 || o.Argv[0] == "" {
		result.Error = "command argv is required"
		return
	}
	input := o.Stdin
	if input != nil && o.Input != nil {
		result.Error = "raw stdin and JSON input are mutually exclusive"
		return
	}
	if input == nil {
		encoded, err := json.Marshal(o.Input)
		if err != nil {
			result.Error = err.Error()
			return
		}
		input = append(encoded, '\n')
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = 1_000_000
	}
	// WithDeadline starts no timer of its own when the parent's deadline is earlier
	// than ours, so a deadline cause then can only have come from the caller.
	deadline := time.Now().Add(o.Timeout)
	parentDeadline, hasParentDeadline := parent.Deadline()
	callerDeadlineFirst := hasParentDeadline && parentDeadline.Before(deadline)
	timed, stop := context.WithDeadline(parent, deadline)
	defer stop()
	ctx, cancel := context.WithCancelCause(timed)
	defer cancel(nil)
	c := &capture{left: o.MaxOutputBytes, cancel: cancel, started: started}
	if o.StdoutLines != nil && o.StdoutLines.Line != nil {
		c.sink = o.StdoutLines
		c.maxStream, c.maxLine = o.StdoutLines.limits()
	}
	cmd := exec.CommandContext(ctx, o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stream{capture: c}
	cmd.Stderr = stream{capture: c, stderr: true}
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	err := cmd.Start()
	if err == nil {
		if o.Watchdog != nil {
			done, finished := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(finished)
				o.Watchdog.watch(c, done)
			}()
			err = cmd.Wait()
			close(done)
			<-finished
		} else {
			err = cmd.Wait()
		}
	}
	if cmd.Process != nil {
		result.Reconciled, result.Uncertainty = reconcileProcess(cmd)
	}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	c.Lock()
	if c.sink != nil && !c.overflow && len(c.partial) > 0 {
		if sinkErr := c.sink.Line(c.partial); sinkErr != nil {
			c.stop(sinkErr)
		}
		c.partial = c.partial[:0]
	}
	c.sinkClosed = true
	result.Stdout = c.stdout.String()
	result.RawStdout = result.Stdout
	result.Stderr = c.stderr.String()
	result.Overflow = c.overflow
	result.Truncated = c.truncated
	result.Stalled = c.stalled
	if cmd.Process != nil {
		result.Activity = &Activity{StdoutBytes: c.stdoutBytes, StderrBytes: c.stderrBytes}
		if c.sawOutput {
			last := c.lastOutput.Milliseconds()
			result.Activity.LastOutputMS = &last
		}
	}
	c.Unlock()
	cause := context.Cause(ctx)
	expired := errors.Is(cause, context.DeadlineExceeded)
	result.TimedOut = expired && !callerDeadlineFirst
	result.ParentDeadline = expired && callerDeadlineFirst
	result.Interrupted = errors.Is(cause, context.Canceled)
	result.OK = err == nil && cause == nil && !result.Overflow && result.Reconciled
	if err != nil {
		result.Error = err.Error()
	}
	if !result.Reconciled {
		result.Error = result.Uncertainty
	}
	if cause != nil {
		result.Error = cause.Error()
	}
	var stalled *StalledError
	if errors.As(cause, &stalled) {
		result.Stalled = true
		result.Error = "interrupted: " + stalled.Error()
	}
	return
}
