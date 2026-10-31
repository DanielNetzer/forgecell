package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// maxCIPluginCalls bounds the call records of one plugin CI observation; a
// collection makes two calls.
const maxCIPluginCalls = 8

// pluginShadowRepo and pluginShadowPR stand in for a plugin observation's
// project and change when the shared GitHub-shaped snapshot validation runs, so
// required-check evaluation is exactly checks.Validate's.
const (
	pluginShadowRepo = "plugin/forge"
	pluginShadowPR   = 1
)

// PluginCITarget is the change and commit CollectCIPlugin observes through a
// plugin forge: the forge project, the change's opaque ref, the exact commit
// and the required check names, as the GitHub target has them, with the forge
// and the log its adapter's Observer writes to.
type PluginCITarget struct {
	Project  string
	Ref      string
	Commit   string
	Required []string
	Changes  forge.Changes
	Calls    *PluginCallLog
}

// CollectCIPlugin is CollectCI for a delivery published through a plugin
// forge. It holds the publication lock across validation, remote observation
// and append, reads the change's head with forge_change and its checks with
// forge_checks(ref, commit), and maps them to the same snapshot the GitHub path
// produces, so required-check evaluation is shared. The plugin's calls are
// recorded on the observation.
func CollectCIPlugin(ctx context.Context, o Options, target PluginCITarget) (CIObservation, error) {
	var a CIObservation
	if !safeID.MatchString(o.MoleculeID) {
		return a, fmt.Errorf("invalid Molecule id")
	}
	if target.Changes == nil {
		return a, fmt.Errorf("plugin CI collection needs the Formula's forge plugin")
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return a, fmt.Errorf("delivery/CI collection conflict: %w", err)
	}
	defer os.Remove(lock)
	if err := ValidatePluginCITarget(o, target.Project, target.Ref, target.Commit); err != nil {
		return a, err
	}
	s, err := pluginSnapshot(ctx, target)
	if err != nil {
		return a, err
	}
	calls, dropped := target.Calls.take()
	if len(calls) > maxCIPluginCalls {
		dropped += len(calls) - maxCIPluginCalls
		calls = calls[:maxCIPluginCalls]
	}
	return appendCIWith(o, s, calls, dropped)
}

// ValidatePluginCITarget is ValidateCITarget for a plugin forge delivery: it
// checks local linkage by project, change ref and commit before any lookup.
func ValidatePluginCITarget(o Options, project, ref, commit string) error {
	id, err := ciIdentity(o)
	if err != nil {
		return err
	}
	if id.Ref == "" || id.Repo != project || id.Ref != ref || id.Commit != commit {
		return fmt.Errorf("CI target differs from delivery")
	}
	_, err = ReadCI(o)
	return err
}

// ReadPluginChangeNoun returns the change noun a plugin forge receipt recorded
// at publication, or DefaultChangeNoun when it recorded none.
func ReadPluginChangeNoun(o Options) (string, error) {
	if !safeID.MatchString(o.MoleculeID) {
		return "", fmt.Errorf("invalid Molecule id")
	}
	raw, err := ciReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"), maxReceiptBytes)
	if err != nil {
		return "", err
	}
	var r Receipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return changeNoun(r.ChangeNoun), nil
}

// MatchesPluginPublishedEvidence is MatchesPublishedEvidence for a receipt
// published through a plugin forge: it compares the change's ref, its URL on
// the recorded forge host and the head commit, with the same local provenance.
// Publication does not prove merge, deployment or current remote state.
func MatchesPluginPublishedEvidence(receipt Receipt, r molecule.Record) bool {
	p := receipt.Preview
	return receipt.State == "published" && validPluginName(receipt.Forge) && cleanPluginText(receipt.Project, maxPluginProjectRunes) && validPluginRef(receipt.Ref) && validPluginURL(receipt.ForgeHost, receipt.URL) && len(receipt.Commit) == 40 && strings.Trim(receipt.Commit, "0123456789abcdef") == "" && digest(p) == p.Digest && p.MoleculeID == r.ID && p.Repo == r.Issue.Repo && p.BaseCommit == r.Workspace.BaseCommit && p.Branch == r.Workspace.Branch && r.Verification != nil && p.Tree == r.Verification.SourceTree
}

// pluginCIIdentity is the CI identity of a plugin forge delivery: the forge
// project stands in the repository, PR is 0 and Ref is the change ref.
func pluginCIIdentity(o Options, r Receipt, record molecule.Record) (CIIdentity, error) {
	if !MatchesPluginPublishedEvidence(r, record) {
		return CIIdentity{}, fmt.Errorf("CI requires a matching published delivery receipt and ledger")
	}
	return CIIdentity{MoleculeID: o.MoleculeID, PreviewDigest: r.Preview.Digest, Repo: r.Project, PR: 0, URL: r.URL, Commit: r.Commit, Ref: r.Ref}, nil
}

// validatePluginSnapshot binds a plugin snapshot to its identity and validates
// it exactly as checks.Validate does a GitHub one.
func validatePluginSnapshot(id CIIdentity, s checks.Snapshot) error {
	if s.Repo != id.Repo || s.PR != 0 || s.Commit != id.Commit {
		return fmt.Errorf("CI project, change or commit differs from delivery")
	}
	if !cleanPluginText(s.Repo, maxPluginProjectRunes) {
		return fmt.Errorf("invalid CI identity")
	}
	shadow := s
	shadow.Repo, shadow.PR = pluginShadowRepo, pluginShadowPR
	return checks.Validate(shadow)
}

// validCIPluginCalls reports whether an observation's call records fit its
// identity: none for GitHub, at most maxCIPluginCalls for a plugin forge.
func validCIPluginCalls(id CIIdentity, a CIObservation) bool {
	if id.Ref == "" {
		return len(a.PluginCalls) == 0 && a.PluginCallsDropped == 0
	}
	return len(a.PluginCalls) <= maxCIPluginCalls && a.PluginCallsDropped >= 0 && a.PluginCallsDropped <= MaxReceiptPluginCalls
}

// pluginCheckStatus maps the contract's check statuses to the GitHub ones the
// shared evaluation reads.
var pluginCheckStatus = map[string]string{"queued": "QUEUED", "in_progress": "IN_PROGRESS", "completed": "COMPLETED"}

// pluginCheckConclusion maps the contract's conclusions likewise; only SUCCESS
// passes, as on GitHub.
var pluginCheckConclusion = map[string]string{"": "", "success": "SUCCESS", "failure": "FAILURE", "neutral": "NEUTRAL", "cancelled": "CANCELLED", "skipped": "SKIPPED", "timed_out": "TIMED_OUT", "action_required": "ACTION_REQUIRED", "stale": "STALE"}

// pluginSnapshot observes the target's change head and checks and evaluates
// them as checks.Collect does: lookup failures and malformed or unbounded
// answers are unknown with a fixed detail, never passing evidence. An
// incomplete check list that would pass, or lacks a required check, is unknown,
// since the forge may still add a failing check. Only invalid required names
// are an error, as for GitHub.
func pluginSnapshot(ctx context.Context, t PluginCITarget) (r checks.Snapshot, err error) {
	defer func() {
		raw, _ := json.Marshal(r)
		if err == nil && len(raw) > checks.MaxSnapshotBytes {
			r.Status = "unknown"
			r.Checks = []checks.Check{}
			r.Detail = "Snapshot exceeds bound; check evidence unavailable."
		}
	}()
	r = checks.Snapshot{Repo: t.Project, PR: 0, Commit: t.Commit, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "unknown", Checks: []checks.Check{}}
	if !cleanPluginText(t.Project, maxPluginProjectRunes) || !validPluginRef(t.Ref) || len(t.Commit) != 40 || !pluginCommit.MatchString(t.Commit) {
		return r, fmt.Errorf("plugin checks require a project, a change ref and an exact commit SHA")
	}
	if len(t.Required) == 0 || len(t.Required) > 128 {
		return r, fmt.Errorf("name the required checks explicitly")
	}
	seen := map[string]bool{}
	for _, name := range t.Required {
		if strings.TrimSpace(name) == "" || len(name) > 256 {
			return r, fmt.Errorf("required check names cannot be blank")
		}
		if !seen[name] {
			r.Required = append(r.Required, name)
			seen[name] = true
		}
	}
	change, lookupErr := t.Changes.Change(ctx, t.Ref)
	if lookupErr != nil {
		r.Detail = "Forge plugin change lookup failed (" + string(mcp.ReasonOf(lookupErr)) + "). No passing evidence recorded."
		return r, nil
	}
	if !pluginCommit.MatchString(change.HeadCommit) {
		r.Detail = "Invalid forge plugin change response."
		return r, nil
	}
	r.ObservedHead = change.HeadCommit
	if len(change.HeadCommit) != 40 {
		r.ObservedHead = ""
		r.Detail = "Unsupported forge plugin commit format."
		return r, nil
	}
	got, lookupErr := t.Changes.Checks(ctx, t.Ref, t.Commit)
	if lookupErr != nil {
		r.Detail = "Forge plugin check lookup failed (" + string(mcp.ReasonOf(lookupErr)) + "). No passing evidence recorded."
		return r, nil
	}
	if got.Checks == nil {
		r.Detail = "Check data unavailable."
		return r, nil
	}
	if len(got.Checks) > checks.MaxChecks {
		r.Detail = "Check count exceeds bound."
		return r, nil
	}
	for _, e := range got.Checks {
		status, statusOK := pluginCheckStatus[e.Status]
		conclusion, conclusionOK := pluginCheckConclusion[e.Conclusion]
		if !statusOK {
			r.Detail = "Unrecognized check status."
			status = e.Status
		}
		if !conclusionOK {
			r.Detail = "Unrecognized check conclusion."
			conclusion = e.Conclusion
		}
		if status == "COMPLETED" && conclusion == "" {
			r.Detail = "Completed check without a conclusion."
		}
		c := checks.Check{Name: e.Name, Status: status, Conclusion: conclusion}
		if e.URL != nil {
			c.URL = *e.URL
		}
		if c.Name == "" || len(c.Name) > 256 || len(c.Status) > 64 || len(c.Conclusion) > 64 || len(c.URL) > 2048 {
			r.Detail = "Check fields missing or exceed bounds."
			r.Checks = []checks.Check{}
			return r, nil
		}
		r.Checks = append(r.Checks, c)
	}
	raw, _ := json.Marshal(r)
	if len(raw) > checks.MaxSnapshotBytes {
		r.Checks = []checks.Check{}
		r.Detail = "Snapshot exceeds bound."
		return r, nil
	}
	if r.Detail != "" {
		return r, nil
	}
	if change.HeadCommit != t.Commit {
		r.Status = "stale"
		return r, nil
	}
	present := map[string]bool{}
	pending, failed := false, false
	for _, c := range r.Checks {
		present[c.Name] = true
		if c.Status != "COMPLETED" {
			pending = true
		} else if c.Conclusion != "SUCCESS" {
			failed = true
		}
	}
	missing := false
	for _, name := range r.Required {
		if !present[name] {
			missing = true
		}
	}
	switch {
	case missing:
		r.Status = "missing"
	case failed:
		r.Status = "failed"
	case pending:
		r.Status = "pending"
	default:
		r.Status = "passed"
	}
	if !got.Complete && (r.Status == "passed" || r.Status == "missing") {
		r.Status = "unknown"
		r.Detail = "The forge reports that more checks may still be added. No passing evidence recorded."
	}
	return r, nil
}
