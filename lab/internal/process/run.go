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
}
type capture struct {
	sync.Mutex
	left           int
	stdout, stderr bytes.Buffer
	cancel         context.CancelCauseFunc
	overflow       bool
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
	c := &capture{left: o.MaxOutputBytes, cancel: cancel}
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
