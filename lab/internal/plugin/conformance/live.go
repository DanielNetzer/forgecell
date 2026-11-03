package conformance

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Live configures the opt-in live mode (design §8): after contract mode passes,
// the kit calls the plugin's read tools against the target the contributor
// names, and forge_publish_draft only with AllowWrite.
type Live struct {
	// Target is the tracker project or board key (shown only; the plugin reads
	// it from Config), the forge project, or the reviewer's forge identity.
	Target string
	// Config is the tracker or reviewer config object. It is passed to the
	// plugin only, never shown.
	Config map[string]any
	// Change and Commit name an existing change request and its head commit.
	Change string
	Commit string
	// AllowWrite opens one draft change request from Head onto Base at Commit.
	AllowWrite bool
	Head       string
	Base       string
}

const (
	modeLive = "live"
	// maxTargetRunes bounds the cleaned target in a report.
	maxTargetRunes = 200
	// reasonIncomplete fails the check that reached the call bound.
	reasonIncomplete mcp.Reason = "incomplete"
	// reasonNoKeys skips the get and states checks of an empty tracker list.
	reasonNoKeys = "no-keys"
)

// maxLiveCalls bounds the tool calls of one live run, the contract checks'
// calls included. Tests lower it.
var maxLiveCalls = 40

// liveTimeout overrides Options.Timeout for live calls only when non-zero, so a
// test can time out a live call that never answers while the contract calls
// keep their hang guard.
var liveTimeout time.Duration

type liveKey struct{}

// liveRun is the state of one live run, shared with Run through its context.
type liveRun struct {
	opts    Live
	k       *kit
	checks  []string
	results map[string]Result
	// stopped is the reason every check not yet recorded is skipped with.
	stopped string
}

// RunLive runs contract mode on the plugin whose manifest is at the absolute
// path manifestPath, then the live checks for its kind in the same session.
// Live checks run only when every contract check passed so far, and the run
// stops at the first live check that does not pass. The report holds the
// contract checks, then the live ones, with Mode "live" and the cleaned target.
func RunLive(ctx context.Context, manifestPath string, opts Options, live Live) Report {
	lr := &liveRun{opts: live, results: map[string]Result{}}
	r := Run(context.WithValue(ctx, liveKey{}, lr), manifestPath, opts)
	r.Mode = modeLive
	r.Target = safetext.Text(live.Target, maxTargetRunes)
	if lr.checks == nil {
		lr.checks = plannedChecks(r.Kind, live)
	}
	for _, id := range lr.checks {
		result, ok := lr.results[id]
		if !ok {
			result = Result{ID: id, Status: Skip, Reason: lr.stopped, Summary: summaries[id]}
		}
		r.Passed = r.Passed && result.Status == Pass
		r.Checks = append(r.Checks, result)
	}
	return r
}

// plannedChecks are the live checks of kind, in order. live-change needs a
// change and live-publish needs AllowWrite; neither is planned otherwise.
func plannedChecks(kind string, live Live) []string {
	switch kind {
	case "tracker":
		return []string{CheckLiveList, CheckLiveGet, CheckLiveStates}
	case "forge":
		checks := []string{CheckLiveDescribe, CheckLivePolicy}
		if live.Change != "" {
			checks = append(checks, CheckLiveChange)
		}
		if live.AllowWrite {
			checks = append(checks, CheckLivePublish)
		}
		return checks
	case "reviewer":
		return []string{CheckLiveFindings}
	}
	return []string{}
}

// runLive is Run's hook: it runs the live checks when Run was called by
// RunLive and every contract check so far passed. It does nothing otherwise.
func (k *kit) runLive() {
	lr, _ := k.ctx.Value(liveKey{}).(*liveRun)
	if lr == nil || k.c == nil {
		return
	}
	lr.k = k
	lr.checks = plannedChecks(k.c.Kind(), lr.opts)
	if !k.contractPassed() {
		return
	}
	switch k.c.Kind() {
	case "tracker":
		lr.tracker()
	case "forge":
		lr.forge()
	case "reviewer":
		lr.reviewer()
	}
}

// contractPassed reports whether every contract check that ran passed and no
// call broke an output or time bound.
func (k *kit) contractPassed() bool {
	for _, id := range []string{CheckStart, CheckInitialize, CheckToolsList, CheckDescribe, CheckDeterministic, CheckMalformed} {
		if k.results[id].Status != Pass {
			return false
		}
	}
	return !k.tooLarge && !k.timedOut
}

// call makes one live tool call. The arguments must pass the tool's input
// schema, a write tool needs AllowWrite, and the run's call bound is checked
// before the call: reaching it fails as incomplete without calling.
func (lr *liveRun) call(name string, args map[string]any) (any, error) {
	t, ok := lr.k.c.Tool(name)
	if !ok {
		return nil, mcp.Fail(mcp.ContractMismatch, name, "the tool is not part of the contract")
	}
	if t.Access == contract.Write && !lr.opts.AllowWrite {
		return nil, mcp.Fail(mcp.Failed, name, "a write tool needs --allow-write")
	}
	if err := t.ValidateInput(args); err != nil {
		return nil, mcp.Fail(mcp.SchemaInvalid, name, "the live arguments fail the tool's input schema")
	}
	if lr.k.calls >= maxLiveCalls {
		return nil, mcp.Fail(reasonIncomplete, name, "the run reached its tool call bound")
	}
	timeout := lr.k.opts.Timeout
	if liveTimeout > 0 {
		timeout = liveTimeout
	}
	lr.k.calls++
	// observe notes an output or time bound broken by a live call, so the
	// output-bound and timeout-bound checks fail with it.
	out, err := lr.k.p.Call(lr.k.ctx, t, args, timeout)
	return out, lr.k.observe(err)
}

// sentSince reports whether a live call reached the plugin after calls was
// read: one that answered, or one whose answer broke a bound or failed to
// decode, rather than a well-formed MCP error.
func (lr *liveRun) sentSince(calls int, err error) bool {
	var e *mcp.Error
	return lr.k.calls > calls && (err == nil || !errors.As(err, &e) || !e.Remote)
}

// record stores a live check's outcome and reports whether the run goes on. A
// rate-limited error skips this check and every later one, with no retry.
func (lr *liveRun) record(id string, err error) bool {
	switch {
	case err == nil:
		lr.results[id] = Result{ID: id, Status: Pass, Summary: summaries[id]}
		return true
	case mcp.ReasonOf(err) == mcp.RateLimited:
		lr.stopped = string(mcp.RateLimited)
		lr.results[id] = Result{ID: id, Status: Skip, Reason: lr.stopped, Summary: summaries[id]}
	default:
		lr.results[id] = Result{ID: id, Status: Fail, Reason: string(mcp.ReasonOf(err)), Summary: summaries[id]}
	}
	return false
}

// config is the tracker or reviewer config object, never nil.
func (lr *liveRun) config() map[string]any {
	if lr.opts.Config == nil {
		return map[string]any{}
	}
	return lr.opts.Config
}

// as returns a decoded output as its contract type.
func as[T any](out any, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	v, ok := out.(T)
	if !ok {
		return zero, mcp.Fail(mcp.SchemaInvalid, "", "the output has an unexpected type")
	}
	return v, nil
}

// reviewer runs live-findings: two reviewer_findings calls for the named
// change must decode and return the same findings.
func (lr *liveRun) reviewer() {
	const tool = "reviewer_findings"
	args := map[string]any{
		"forge":  map[string]any{"identity": lr.opts.Target},
		"ref":    lr.opts.Change,
		"commit": lr.opts.Commit,
		"config": lr.config(),
	}
	first, err := as[contract.ReviewerFindings](lr.call(tool, args))
	if err == nil {
		var second contract.ReviewerFindings
		second, err = as[contract.ReviewerFindings](lr.call(tool, args))
		if err == nil && !reflect.DeepEqual(first, second) {
			err = mcp.Fail(mcp.Failed, tool, "two calls returned different findings")
		}
	}
	lr.record(CheckLiveFindings, err)
}
