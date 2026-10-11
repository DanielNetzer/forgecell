// Package process invokes approved argv without interpreting ticket text as shell code.
package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"time"
)

var errOutputLimit = errors.New("command output exceeded the configured byte limit")

type Options struct {
	Stdin          []byte // Raw provider input; mutually exclusive with Input.
	Argv           []string
	Dir            string
	Env            []string
	Input          any
	Timeout        time.Duration
	MaxOutputBytes int
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
	Overflow    bool   `json:"overflow"`
	ElapsedMS   int64  `json:"elapsedMs"`
	Error       string `json:"error,omitempty"`
	// PermissionDenials records bounded summaries of tool calls the harness
	// denied during an otherwise valid run; they are evidence, not failure.
	PermissionDenials []string `json:"permissionDenials,omitempty"`
	// Activity is nil when no pipe observation was recorded (older records, or a
	// process that never started). A non-nil value with zero bytes is a recorded
	// silent run.
	Activity *Activity `json:"activity,omitempty"`
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
	StateSucceeded    TerminalState = "succeeded"
	StateTimeout      TerminalState = "timeout"
	StateInterrupted  TerminalState = "interrupted"
	StateOutputLimit  TerminalState = "output-limit"
	StateUnreconciled TerminalState = "unreconciled"
	StateFailed       TerminalState = "failed"
	StateUnknown      TerminalState = "unknown"
)

// ReconnectUnknown is the only reconnect evidence reported: no provider-neutral
// reconnect signal exists, so none is invented.
const ReconnectUnknown = "unknown"

// TerminalState classifies the run from its existing flags alone. An unreconciled
// process outranks the cause that stopped it, because it may still be running.
// Timeout and interruption come from the first context cause, so they outrank the
// output cap, which can also be reached while the process is being stopped.
func (r Result) TerminalState() TerminalState {
	switch {
	case !r.Reconciled && r.Uncertainty != "":
		return StateUnreconciled
	case r.TimedOut:
		return StateTimeout
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
}
type stream struct {
	capture *capture
	stderr  bool
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
	timed, stop := context.WithTimeout(parent, o.Timeout)
	defer stop()
	ctx, cancel := context.WithCancelCause(timed)
	defer cancel(nil)
	c := &capture{left: o.MaxOutputBytes, cancel: cancel, started: started}
	cmd := exec.CommandContext(ctx, o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stream{capture: c}
	cmd.Stderr = stream{capture: c, stderr: true}
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	err := cmd.Run()
	if cmd.Process != nil {
		result.Reconciled, result.Uncertainty = reconcileProcess(cmd)
	}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	c.Lock()
	result.Stdout = c.stdout.String()
	result.RawStdout = result.Stdout
	result.Stderr = c.stderr.String()
	result.Overflow = c.overflow
	if cmd.Process != nil {
		result.Activity = &Activity{StdoutBytes: c.stdoutBytes, StderrBytes: c.stderrBytes}
		if c.sawOutput {
			last := c.lastOutput.Milliseconds()
			result.Activity.LastOutputMS = &last
		}
	}
	c.Unlock()
	cause := context.Cause(ctx)
	result.TimedOut = errors.Is(cause, context.DeadlineExceeded)
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
	return
}
