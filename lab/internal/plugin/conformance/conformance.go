// Package conformance is the contract-mode plugin kit behind forgecell plugin
// test: offline checks that a plugin starts, declares its contract, lists the
// contract's tools with compatible schemas, describes itself strictly and
// deterministically, answers malformed input with MCP errors, and stays within
// its output and time bounds. It never calls a write tool.
package conformance

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

const (
	defaultGrace     = 2 * time.Second
	defaultHandshake = 30 * time.Second
	// describeProject is the neutral project a forge describe call names.
	describeProject = "example/project"
)

// Options configure a run. The zero value uses the contract timeouts, a 30 s
// handshake bound and a 2 s shutdown grace.
type Options struct {
	// Root is the directory under which the plugin's private run directory is
	// created.
	Root string
	// LookupEnv reads the caller's environment for PATH, LANG and needs.env.
	LookupEnv func(string) (string, bool)
	// Grace returns the channel that ends the shutdown grace period.
	Grace func() <-chan time.Time
	// Timeout overrides every tool's contract timeout when non-zero.
	Timeout time.Duration
	// Handshake bounds initialize and tools/list when non-zero.
	Handshake time.Duration
}

type kit struct {
	ctx      context.Context
	p        *plugin.Process
	c        *contract.Contract
	opts     Options
	results  map[string]Result
	calls    int
	tooLarge bool
	timedOut bool
}

// Run checks the plugin whose manifest is at the absolute path manifestPath.
func Run(ctx context.Context, manifestPath string, opts Options) Report {
	r := Report{SchemaVersion: "v1", Mode: "contract", Manifest: filepath.Base(manifestPath)}
	k := &kit{ctx: ctx, opts: opts, results: map[string]Result{}}
	m, digest, err := plugin.LoadFile(manifestPath)
	if err != nil {
		k.record(CheckStart, err)
		return k.report(r)
	}
	r.Plugin, r.Kind, r.Contract, r.ManifestSHA256 = m.Name, m.Kind, m.Contract, digest
	k.c, _ = contract.Lookup(m.Kind)
	k.p, err = plugin.Start(ctx, m, plugin.Options{Root: opts.Root, LookupEnv: opts.LookupEnv})
	k.record(CheckStart, err)
	if err != nil {
		return k.report(r)
	}
	k.run()
	grace := opts.Grace
	if grace == nil {
		grace = func() <-chan time.Time { return time.After(defaultGrace) }
	}
	if k.p.Shutdown(grace()) != nil {
		r.Cleanup = cleanupLeft
	}
	return k.report(r)
}

func (k *kit) run() {
	handshake := k.opts.Handshake
	if handshake <= 0 {
		handshake = defaultHandshake
	}
	ctx, cancel := context.WithTimeout(k.ctx, handshake)
	_, err := k.p.Initialize(ctx)
	cancel()
	if !k.record(CheckInitialize, k.observe(err)) {
		return
	}
	ctx, cancel = context.WithTimeout(k.ctx, handshake)
	tools, err := k.p.ListTools(ctx)
	cancel()
	if err == nil {
		err = k.covers(tools)
	}
	if !k.record(CheckToolsList, k.observe(err)) {
		return
	}
	describe := k.c.Describe()
	first, err := k.call(describe, describeArgs(k.c.Kind()))
	if !k.record(CheckDescribe, err) {
		return
	}
	second, err := k.call(describe, describeArgs(k.c.Kind()))
	if err == nil && !reflect.DeepEqual(first, second) {
		err = mcp.Fail(mcp.Failed, describe.Name, "two describe calls returned different output")
	}
	if !k.record(CheckDeterministic, err) {
		return
	}
	k.record(CheckMalformed, k.malformed())
}

// covers requires every required contract tool in tools/list with an input
// schema compatible with the contract's.
func (k *kit) covers(tools []mcp.ToolInfo) error {
	listed := map[string]mcp.ToolInfo{}
	for _, t := range tools {
		listed[t.Name] = t
	}
	for i := range k.c.Tools {
		t := &k.c.Tools[i]
		info, ok := listed[t.Name]
		if !ok {
			if t.Required {
				return mcp.Fail(mcp.ContractMismatch, t.Name, "tools/list does not list a required contract tool")
			}
			continue
		}
		if err := t.CheckAdvertised(info.InputSchema); err != nil {
			return mcp.Fail(mcp.ContractMismatch, t.Name, err.Error())
		}
	}
	return nil
}

// call calls a contract tool within its bounds and decodes the output.
func (k *kit) call(t *contract.Tool, args any) (any, error) {
	k.calls++
	out, err := k.p.Call(k.ctx, t, args, k.opts.Timeout)
	return out, k.observe(err)
}

// malformed sends invalid arguments to every read tool and requires an MCP
// error each time, then a working describe call from a live server. Write
// tools are never called, not even with invalid input.
func (k *kit) malformed() error {
	for i := range k.c.Tools {
		t := &k.c.Tools[i]
		if t.Access != contract.Read {
			continue
		}
		for _, args := range malformedInputs(t) {
			timeout := k.opts.Timeout
			if timeout <= 0 {
				timeout = t.Timeout()
			}
			k.calls++
			_, err := k.p.CallRaw(k.ctx, t.Name, args, t.MessageBytes(), timeout)
			var e *mcp.Error
			switch {
			case err == nil:
				return mcp.Fail(mcp.Failed, t.Name, "the tool accepted malformed input")
			case !errors.As(k.observe(err), &e) || !e.Remote:
				return err
			}
		}
	}
	if _, err := k.call(k.c.Describe(), describeArgs(k.c.Kind())); err != nil {
		return err
	}
	if k.p.Exited() {
		return mcp.Fail(mcp.Failed, "", "the server exited after malformed input")
	}
	return nil
}

// malformedInputs are arguments every contract input schema refuses: no
// arguments at all, and the first required property with the wrong JSON type.
func malformedInputs(t *contract.Tool) []map[string]any {
	inputs := []map[string]any{{}}
	if len(t.Input.Required) > 0 {
		name := t.Input.Required[0]
		wrong := map[string]any{"string": 12345, "integer": "x", "object": "x", "array": "x", "boolean": "x"}[t.Input.Properties[name].Type]
		inputs = append(inputs, map[string]any{name: wrong})
	}
	return inputs
}

func describeArgs(kind string) map[string]any {
	if kind == "forge" {
		return map[string]any{"project": describeProject}
	}
	return map[string]any{"config": map[string]any{}}
}

// observe notes bound failures for the output and timeout checks. Only tool
// calls count toward k.calls, so the bound checks are skipped when none ran.
func (k *kit) observe(err error) error {
	if err == nil {
		return nil
	}
	switch mcp.ReasonOf(err) {
	case mcp.OutputTooLarge:
		k.tooLarge = true
	case mcp.Timeout:
		k.timedOut = true
	}
	return err
}

// record stores a check's outcome and reports whether it passed.
func (k *kit) record(id string, err error) bool {
	if err == nil {
		k.results[id] = Result{ID: id, Status: Pass, Summary: summaries[id]}
		return true
	}
	k.results[id] = Result{ID: id, Status: Fail, Reason: string(mcp.ReasonOf(err)), Summary: summaries[id]}
	return false
}

// report fills in the bound checks and skips, in the fixed order.
func (k *kit) report(r Report) Report {
	if k.calls > 0 {
		k.results[CheckOutputBound] = Result{ID: CheckOutputBound, Status: Pass, Summary: summaries[CheckOutputBound]}
		k.results[CheckTimeoutBound] = Result{ID: CheckTimeoutBound, Status: Pass, Summary: summaries[CheckTimeoutBound]}
		if k.tooLarge {
			k.results[CheckOutputBound] = Result{ID: CheckOutputBound, Status: Fail, Reason: string(mcp.OutputTooLarge), Summary: summaries[CheckOutputBound]}
		}
		if k.timedOut {
			k.results[CheckTimeoutBound] = Result{ID: CheckTimeoutBound, Status: Fail, Reason: string(mcp.Timeout), Summary: summaries[CheckTimeoutBound]}
		}
	}
	r.Passed = true
	for _, id := range checkOrder {
		result, ok := k.results[id]
		if !ok {
			result = Result{ID: id, Status: Skip, Summary: summaries[id]}
		}
		r.Passed = r.Passed && result.Status == Pass
		r.Checks = append(r.Checks, result)
	}
	r.Passed = r.Passed && r.Cleanup == ""
	return r
}
