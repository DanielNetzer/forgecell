package harness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"time"
)

// InvokeBinding keeps native adapters in-process so cancellation reaches the provider.
// A saved launcher that is not the running executable is never executed; its release
// manifest decides whether the running executable's adapter may serve the binding.
func InvokeBinding(ctx context.Context, command []string, dir string, timeout time.Duration, request map[string]any) process.Result {
	if len(command) == 4 && command[1] == "__adapter" {
		if id := ResolveLauncher(command[0]); !id.Runnable() {
			return process.Result{Code: -1, Error: id.Describe(command[2])}
		}
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
	if request["kind"] == "ticket-analysis" {
		return process.Result{Code: -1, Error: "Custom binding has no verified read-only ticket-analysis protocol; it was preserved and not invoked."}
	}
	if request["kind"] == ReviewKind {
		return process.Result{Code: -1, Error: customReviewRefusal}
	}
	return process.Run(ctx, process.Options{Argv: command, Dir: dir, Timeout: timeout, Input: request})
}

const customReviewRefusal = "Custom binding has no verified read-only review protocol; it was preserved and not invoked."

// InvokeReviewBinding runs a review request through a native adapter only. A custom
// command is never executed. The provider process receives ReviewEnvironment and
// never the names in withheld, so plan credentials, GitHub tokens and CI tokens
// stay out of the reviewer. A successful result carries the final review text in
// Stdout.
func InvokeReviewBinding(ctx context.Context, command []string, dir string, timeout time.Duration, request map[string]any, withheld []string) process.Result {
	if request["kind"] != ReviewKind {
		return process.Result{Code: -1, Error: "InvokeReviewBinding only accepts review requests."}
	}
	if len(command) != 4 || command[1] != "__adapter" {
		return process.Result{Code: -1, Error: customReviewRefusal}
	}
	if id := ResolveLauncher(command[0]); !id.Runnable() {
		return process.Result{Code: -1, Error: id.Describe(command[2])}
	}
	result := Execute(ctx, ExecuteOptions{ID: command[2], Executable: command[3], Request: request, Dir: dir, Timeout: timeout, WithheldEnv: withheld})
	if result.Process.OK {
		result.Process.Stdout = result.Text
	}
	return result.Process
}
