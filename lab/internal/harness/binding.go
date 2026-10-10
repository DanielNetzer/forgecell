package harness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"os/exec"
	"time"
)

// InvokeBinding keeps native adapters in-process so cancellation reaches the provider.
func InvokeBinding(ctx context.Context, command []string, dir string, timeout time.Duration, request map[string]any) process.Result {
	if len(command) == 4 && command[1] == "__adapter" {
		current, err := os.Executable()
		bound, boundErr := exec.LookPath(command[0])
		if err == nil && boundErr == nil {
			a, aerr := os.Stat(current)
			b, berr := os.Stat(bound)
			if aerr == nil && berr == nil && os.SameFile(a, b) {
				result := Execute(ctx, ExecuteOptions{ID: command[2], Executable: command[3], Request: request, Dir: dir, Timeout: timeout})
				if request["kind"] == "molecule" {
					result.Process.RawStdout = result.Raw
					result.Process.Stdout = result.Text
					if result.Coding != nil && result.Coding.Outcome == "unknown" {
						result.Process.Stdout = ""
						if result.Process.Error == "" {
							result.Process.Error = result.Coding.Reason
						}
					}
				} else if result.Process.OK {
					result.Process.Stdout = result.Text
				}
				return result.Process
			}
		}
		return process.Result{Code: -1, Error: "Saved adapter belongs to a different Forgecell executable. Rebind the harness through init before running."}
	}
	if request["kind"] == "ticket-analysis" {
		return process.Result{Code: -1, Error: "Custom binding has no verified read-only ticket-analysis protocol; it was preserved and not invoked."}
	}
	return process.Run(ctx, process.Options{Argv: command, Dir: dir, Timeout: timeout, Input: request})
}
