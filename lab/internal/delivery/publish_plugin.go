package delivery

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Bounds of a plugin forge's identities and records.
const (
	// MaxReceiptPluginCalls bounds Receipt.PluginCalls; a call past it only
	// counts in Receipt.PluginCallsDropped.
	MaxReceiptPluginCalls = 256
	maxPluginNameBytes    = 64
	maxPluginProjectRunes = 200
	maxPluginRefRunes     = 200
	maxPluginURLBytes     = 2048
	maxPluginHostBytes    = 253
	maxChangeNounRunes    = 40
)

// DefaultChangeNoun names a change request when a plugin gave no noun.
const DefaultChangeNoun = "change request"

var (
	pluginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pluginHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	pluginCommit      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// PluginForge names the plugin forge PublishOptions.Changes serves: the
// Formula's plugin name and forge.project, and the log its adapter's Observer
// writes to, whose records the receipt keeps.
type PluginForge struct {
	Name    string
	Project string
	Calls   *PluginCallLog
}

// PluginCallLog collects the records of plugin calls for one receipt or CI
// observation, through the adapter's Observer. Records hold digests, never
// content. At most MaxReceiptPluginCalls are pending; the rest only count. It
// is safe for concurrent use, and a nil log keeps nothing.
type PluginCallLog struct {
	mu      sync.Mutex
	calls   []mcpadapter.CallRecord
	dropped int
}

// Observe is an mcpadapter Observer.
func (l *PluginCallLog) Observe(rec mcpadapter.CallRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) >= MaxReceiptPluginCalls {
		l.dropped++
		return
	}
	l.calls = append(l.calls, rec)
}

// take returns and clears the pending records and drop count.
func (l *PluginCallLog) take() ([]mcpadapter.CallRecord, int) {
	if l == nil {
		return nil, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	calls, dropped := l.calls, l.dropped
	l.calls, l.dropped = nil, 0
	return calls, dropped
}

// absorbReceiptCalls moves the log's pending records into r, keeping at most
// MaxReceiptPluginCalls in all. It reports whether r changed.
func absorbReceiptCalls(r *Receipt, l *PluginCallLog) bool {
	calls, dropped := l.take()
	for _, rec := range calls {
		if len(r.PluginCalls) >= MaxReceiptPluginCalls {
			dropped++
			continue
		}
		r.PluginCalls = append(r.PluginCalls, rec)
	}
	r.PluginCallsDropped += dropped
	return len(calls) > 0 || dropped > 0
}

// PluginPublishOutcome is the reconciliation outcome of a plugin forge
// receipt: created when Forgecell opened the draft, existing-draft when it
// adopted the open draft it found, mismatch when reconciliation is required,
// or "" for a receipt that is not a published plugin forge delivery.
func PluginPublishOutcome(r Receipt) string {
	switch {
	case r.Forge == "":
		return ""
	case r.Lifecycle != nil && r.Lifecycle.Reconciliation != "":
		return "mismatch"
	case r.State != "published":
		return ""
	case r.CreationAttemptedAt != "":
		return "created"
	}
	return "existing-draft"
}

// pluginReasonText is the plain account of each fixed plugin reason.
var pluginReasonText = map[mcp.Reason]string{
	mcp.NotInstalled:     "the plugin is not installed",
	mcp.ManifestChanged:  "the plugin manifest changed since the Formula pinned it",
	mcp.ContractMismatch: "the plugin does not serve the forge contract",
	mcp.AuthMissing:      "the plugin has no credentials for the forge",
	mcp.RateLimited:      "the forge rate-limited the plugin",
	mcp.Timeout:          "the plugin did not answer in time",
	mcp.OutputTooLarge:   "the plugin's answer exceeded its bound",
	mcp.SchemaInvalid:    "the plugin's answer did not match the forge contract",
	mcp.Network:          "the plugin could not reach the forge",
	mcp.Failed:           "the plugin call failed",
}

// pluginForgeError is a failed plugin forge operation in plain words: the step
// and the fixed reason's account, never server text, stderr or a local path.
// It unwraps to the adapter's error, so mcp.ReasonOf still reports the reason.
type pluginForgeError struct {
	step   string
	reason mcp.Reason
	err    error
}

func (e *pluginForgeError) Error() string {
	return "the forge plugin could not " + e.step + ": " + pluginReasonText[e.reason] + " (" + string(e.reason) + ")"
}

func (e *pluginForgeError) Unwrap() error { return e.err }

// pluginError maps a plugin forge failure to a plain error for step.
func pluginError(step string, err error) error {
	reason := mcp.ReasonOf(err)
	if _, ok := pluginReasonText[reason]; !ok {
		reason = mcp.Failed
	}
	return &pluginForgeError{step: step, reason: reason, err: err}
}

// cleanPluginText reports non-empty valid UTF-8 of at most limit runes with no
// control or format characters.
func cleanPluginText(s string, limit int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > limit {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validPluginName(name string) bool {
	return len(name) <= maxPluginNameBytes && pluginNamePattern.MatchString(name)
}

func validForgeHost(host string) bool {
	return len(host) <= maxPluginHostBytes && pluginHostPattern.MatchString(host)
}

func validPluginRef(ref string) bool { return cleanPluginText(ref, maxPluginRefRunes) }

// validPluginURL is the plugin forge's URL check: an https URL on exactly the
// host forge_describe named, with no user information, of bounded size.
func validPluginURL(host, raw string) bool {
	if !validForgeHost(host) || len(raw) > maxPluginURLBytes || !cleanPluginText(raw, maxPluginURLBytes) || !strings.HasPrefix(raw, "https://"+host+"/") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == host && u.User == nil && u.Opaque == ""
}

// changeNoun is the forge's noun for a change request, or DefaultChangeNoun.
func changeNoun(noun string) string {
	if !cleanPluginText(noun, maxChangeNounRunes) {
		return DefaultChangeNoun
	}
	return noun
}

// pluginChangeState maps a contract change state to the receipt's lifecycle
// states.
func pluginChangeState(state string) string {
	switch state {
	case "open":
		return "OPEN"
	case "closed":
		return "CLOSED"
	case "merged":
		return "MERGED"
	}
	return "UNKNOWN"
}

// publishPlugin is Publish through a plugin forge, after the shared steps
// committed the reviewed tree. It mirrors GitHub's order: forge_describe (so a
// broken plugin fails before anything is pushed), git fetch, forge_policy and
// its requirements, git push, forge_find_change, forge_publish_draft only when
// no change exists for the branch, then forge_change to verify. Every plugin
// call reaches the receipt's PluginCalls.
func publishPlugin(ctx context.Context, o PublishOptions, r Receipt, record molecule.Record, path string, call func(...string) (string, error)) (out Receipt, err error) {
	changes, meta := o.Changes, o.Plugin
	if !validPluginName(meta.Name) || !cleanPluginText(meta.Project, maxPluginProjectRunes) {
		return r, fmt.Errorf("a plugin forge delivery needs the plugin's name and project")
	}
	if r.Forge == "" && (r.URL != "" || r.Ref != "") {
		return r, fmt.Errorf("delivery receipt was published through GitHub; deliver it with the Formula that published it")
	}
	// A GitHub receipt whose PR creation was attempted but never resolved may
	// still have opened a pull request; a plugin forge never adopts it.
	if r.Forge == "" && r.CreationAttemptedAt != "" {
		return r, fmt.Errorf("delivery receipt records a GitHub pull request creation attempt; reconcile it with the Formula that attempted it")
	}
	if r.Forge != "" && (r.Forge != meta.Name || r.Project != meta.Project) {
		return r, fmt.Errorf("delivery receipt was published through another forge plugin or project")
	}
	r.Forge, r.Project = meta.Name, meta.Project
	save := func() error {
		absorbReceiptCalls(&r, meta.Calls)
		return writeReceipt(o.Options, r)
	}
	// Calls made after the last write reach the receipt on every return path.
	defer func() {
		if absorbReceiptCalls(&r, meta.Calls) {
			if e := writeReceipt(o.Options, r); e != nil && err == nil {
				err = e
			}
		}
		out = r
	}()
	d, err := changes.Describe(ctx)
	if err != nil {
		return r, pluginError("describe the forge", err)
	}
	if !validForgeHost(d.Host) {
		return r, fmt.Errorf("the forge plugin described an invalid host")
	}
	if r.ForgeHost != "" && r.ForgeHost != d.Host {
		return r, fmt.Errorf("the forge plugin's host differs from the host recorded for this delivery")
	}
	noun := changeNoun(d.ChangeNoun)
	r.ForgeHost, r.ChangeNoun = d.Host, noun
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	reconcile := func(state, reason string) (Receipt, error) {
		r.Lifecycle = &LifecycleObservation{State: state, ObservedAt: now(), Reconciliation: reason}
		if e := save(); e != nil {
			return r, e
		}
		return r, fmt.Errorf("%s reconciliation required: %s", noun, reason)
	}
	verify := func() (Receipt, error) {
		c, e := changes.Change(ctx, r.Ref)
		if e != nil {
			return reconcile("UNKNOWN", pluginError("read the "+noun, e).Error())
		}
		state := pluginChangeState(c.State)
		if state == "UNKNOWN" {
			return reconcile("UNKNOWN", "unknown or missing "+noun+" state")
		}
		if c.URL != r.URL || !validPluginURL(r.ForgeHost, c.URL) {
			return reconcile(state, "target project or "+noun+" URL mismatch")
		}
		if c.HeadCommit != r.Commit {
			return reconcile(state, "head commit missing or moved")
		}
		if state != "OPEN" {
			return reconcile(state, noun+" is "+state)
		}
		if !c.Draft {
			return reconcile(state, noun+" is not a confirmed draft")
		}
		r.Lifecycle = &LifecycleObservation{State: state, ObservedAt: now()}
		if r.State != "published" {
			r.State = "published"
			r.DraftAtPublication = true
		}
		return r, save()
	}
	if r.URL != "" || r.Ref != "" {
		if !validPluginRef(r.Ref) || !validPluginURL(r.ForgeHost, r.URL) || (r.State != "published" && r.State != "committed") {
			return r, fmt.Errorf("invalid recorded %s identity", noun)
		}
		return verify()
	}
	r.State = "committed"
	if err = save(); err != nil {
		return r, err
	}
	if _, err = validatedHead(ctx, o.Options, r.Commit); err != nil {
		return r, err
	}
	if _, err = call("git", "fetch", "--no-tags", "origin", "+refs/heads/"+r.Preview.BaseBranch+":refs/remotes/origin/"+r.Preview.BaseBranch); err != nil {
		return r, err
	}
	if err = validateBase(ctx, path, r.Preview.BaseCommit, r.Preview.BaseBranch); err != nil {
		return r, err
	}
	observed, policyErr := changes.Policy(ctx, r.Preview.BaseBranch)
	policy := molecule.PluginRemotePolicy(meta.Name, observed, policyErr)
	r.Policy = &policy
	if err = save(); err != nil {
		return r, err
	}
	plan := record.Readiness.Plans[len(record.Readiness.Plans)-1].Plan
	if err = molecule.ValidatePluginRequirements(policy, plan.Inputs.MandatoryConstraints); err != nil {
		return r, err
	}
	if _, err = call("git", "push", "origin", r.Commit+":refs/heads/"+r.Preview.Branch); err != nil {
		return r, err
	}
	found, err := changes.FindChange(ctx, r.Preview.Branch)
	if err != nil {
		return reconcile("UNKNOWN", pluginError("find the "+noun+" for the branch", err).Error())
	}
	if len(found.Changes) > 0 {
		if len(found.Changes) != 1 {
			return reconcile("AMBIGUOUS", "more than one "+noun+" found")
		}
		c := found.Changes[0]
		if !validPluginRef(c.Ref) || !validPluginURL(r.ForgeHost, c.URL) {
			return reconcile("UNKNOWN", "unexpected "+noun+" ref or URL")
		}
		r.Ref, r.URL = c.Ref, c.URL
		if err = save(); err != nil {
			return r, err
		}
		state := pluginChangeState(c.State)
		if c.HeadCommit != r.Commit {
			return reconcile(state, "head commit missing or moved")
		}
		if state != "OPEN" {
			return reconcile(state, "listed "+noun+" is not confirmed OPEN")
		}
		if !c.Draft {
			return reconcile("OPEN", "listed "+noun+" is not a confirmed draft")
		}
	} else {
		if r.CreationAttemptedAt != "" {
			return reconcile("MISSING", "no "+noun+" found for unresolved creation attempt; replacement requires reconciliation")
		}
		body := fmt.Sprintf("Implements issue %s.\n\nMolecule: `%s`\nFormula: `%s`\nBase: `%s`\nDelivery: `%s`\n\nAwaiting exact-commit CI and human review. Harness-reported checks are not independent CI evidence. No merge, deployment or issue closure is authorized.\n", record.Issue.DisplayKey(), record.ID, record.FormulaID, r.Preview.BaseCommit, r.Commit)
		r.CreationAttemptedAt = now()
		r.Lifecycle = &LifecycleObservation{State: "UNKNOWN", ObservedAt: r.CreationAttemptedAt, Reconciliation: noun + " creation intent recorded; outcome unresolved"}
		if err = save(); err != nil {
			return r, err
		}
		published, createErr := changes.PublishDraft(ctx, forge.PublishInput{HeadBranch: r.Preview.Branch, Base: r.Preview.BaseBranch, Title: r.Title, Body: body, Commit: r.Commit})
		// A change the plugin reports is kept even when its answer is refused, so
		// a retry finds it by its ref and never creates a replacement.
		if validPluginRef(published.Ref) && validPluginURL(r.ForgeHost, published.URL) {
			r.Ref, r.URL = published.Ref, published.URL
		}
		if createErr != nil {
			return reconcile("UNKNOWN", pluginError("publish the draft "+noun, createErr).Error())
		}
		if r.URL == "" {
			return reconcile("UNKNOWN", "creation returned a missing or unexpected "+noun+" ref or URL")
		}
		if published.HeadCommit != r.Commit || !published.Draft {
			return reconcile("UNKNOWN", "the created "+noun+" is not a draft of the approved commit")
		}
	}
	// Retain the returned identity before verification so retries never create a replacement.
	if err = save(); err != nil {
		return r, err
	}
	return verify()
}
