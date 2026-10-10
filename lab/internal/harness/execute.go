package harness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ExecuteOptions struct {
	ID         string
	Executable string
	Request    map[string]any
	Dir        string
	Env        []string
	Timeout    time.Duration
}
type Execution struct {
	Coding  *CodingOutcome `json:"coding,omitempty"`
	Text    string         `json:"text"`
	Raw     string         `json:"raw"`
	Process process.Result `json:"process"`
}

func Execute(ctx context.Context, o ExecuteOptions) (result Execution) {
	providerStarted := false
	fail := func(message string) Execution {
		if o.Request["kind"] == "molecule" && providerStarted {
			out := CodingOutcome{SchemaVersion: "v1", Outcome: "unknown", Reason: message, Paths: []string{}}
			result.Coding = &out
			return result
		}
		result.Process.OK = false
		result.Process.Error = message
		return result
	}
	result.Process.Code = -1
	if o.Executable == "" {
		return fail("harness executable is required")
	}
	dir, err := os.MkdirTemp("", "forgecell-adapter-")
	if err != nil {
		return fail(err.Error())
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "result.txt")
	call, err := BuildInvocation(o.ID, o.Request, output)
	if err != nil {
		return fail(err.Error())
	}
	schema := proposalSchema
	coding := o.Request["kind"] == "molecule"
	if coding {
		schema = CodingOutcomeSchema()
	}
	if o.Request["kind"] == "ticket-analysis" {
		schema = readiness.AnalysisSchema()
	}
	if err = os.WriteFile(output+".schema.json", []byte(schema), 0600); err != nil {
		return fail(err.Error())
	}
	if o.Request["kind"] == "ticket-analysis" {
		call.Args, err = constrainAnalysis(ctx, o, call.Args)
		if err != nil {
			return fail(err.Error())
		}
		// Analysis has no access to the coding working directory. Evidence is supplied
		// through stdin; no host configuration or credentials are copied here.
		o.Dir = dir
	}
	result.Process = process.Run(ctx, process.Options{Argv: append([]string{o.Executable}, call.Args...), Dir: o.Dir, Env: o.Env, Stdin: []byte(call.Input), Timeout: o.Timeout})
	providerStarted = true
	if !coding && !result.Process.OK {
		return result
	}
	result.Raw = result.Process.Stdout
	if o.ID == "codex" {
		info, err := os.Lstat(output)
		if err != nil {
			return fail("Codex did not write a final output file")
		}
		if !info.Mode().IsRegular() {
			return fail("Codex output must be a bounded regular file")
		}
		f, err := os.Open(output)
		if err != nil {
			return fail(err.Error())
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 1_000_001))
		if err != nil {
			return fail(err.Error())
		}
		result.Raw = string(raw[:min(len(raw), 1_000_000)])
		result.Process.RawStdout = result.Raw
		if len(raw) > 1_000_000 || info.Size() > 1_000_000 {
			return fail("Codex output exceeded byte limit; bounded prefix retained")
		}
	}
	result.Process.RawStdout = result.Raw
	if coding {
		result.Text, result.Process.PermissionDenials, err = ReadCodingResult(o.ID, result.Raw)
	} else {
		result.Text, err = ReadResult(o.ID, result.Raw, true)
	}
	if err != nil {
		return fail(err.Error())
	}
	if strings.TrimSpace(result.Text) == "" {
		return fail("harness did not return a final result")
	}
	if o.Request["kind"] == "ticket-analysis" {
		if _, err = readiness.DecodeAnalysis([]byte(result.Text)); err != nil {
			return fail("invalid readiness analysis: " + err.Error())
		}
	}
	if coding {
		out := CodingOutcomeFromResult(result.Text)
		result.Coding = &out
	}
	return result
}
