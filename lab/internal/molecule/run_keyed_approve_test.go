package molecule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// trackerPluginFixture is keyedFixture with the Formula's intake.source set to
// the declared tracker plugin "jira". The plugin is never started: the keyed
// reader stands in for it.
func trackerPluginFixture(t *testing.T, issue Issue) (Options, *[]string) {
	t.Helper()
	o, calls := keyedFixture(t, issue)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const github = "intake: {source: github-issues, repo: acme/widgets}\n"
	if !strings.Contains(string(raw), github) {
		t.Fatalf("fixture Formula changed:\n%s", raw)
	}
	yaml := strings.Replace(string(raw), github, "intake: {source: jira, repo: acme/widgets}\nplugins:\n  - {name: jira, manifest: plugins/jira/plugin.json, sha256: "+strings.Repeat("a", 64)+"}\n", 1)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, yaml, "tracker-plugin")
	return o, calls
}

func intakeSource(r Record) string {
	for _, a := range r.Atoms {
		if a.ID == "intake" && a.Provenance != nil {
			for _, action := range a.Provenance.Actions {
				if action.AttemptID == "intake" {
					return action.ResolvedSource
				}
			}
		}
	}
	return ""
}

func TestRunKeyedWithTrackerPluginApprovesTheSameMolecule(t *testing.T) {
	t.Parallel()
	o, calls := trackerPluginFixture(t, keyedIssueFixture())
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	if r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("status = %q readiness = %+v notes = %q", r.Status, r.Readiness, r.Notes)
	}
	if got := intakeSource(r); got != "jira" {
		t.Fatalf("intake resolved source = %q", got)
	}
	digest := r.Readiness.Plans[0].Digest

	// Without the opt-in, an approval keeps its refusal.
	o.Approve = digest
	if _, err := RunKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != "approving a keyed Molecule is not supported yet" {
		t.Fatalf("approve without opt-in: %v", err)
	}

	o.AllowKeyedApproval = true
	done, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if done.ID != r.ID || done.Issue.Key != "ENG-1234" || done.Issue.Number != 0 {
		t.Fatalf("approved %q (%+v), want %q", done.ID, done.Issue, r.ID)
	}
	if done.Status != "waiting" || done.Readiness.Phase == "scope-waiting" || len(done.HarnessAttempts) != 1 {
		t.Fatalf("status = %q phase = %q attempts = %d notes = %q", done.Status, done.Readiness.Phase, len(done.HarnessAttempts), done.Notes)
	}
	approved := false
	for _, e := range done.Readiness.Events {
		approved = approved || e.Digest == digest && e.Kind == "approved"
	}
	if !approved {
		t.Fatalf("no approval event for %s: %+v", digest, done.Readiness.Events)
	}
	if data, _ := os.ReadFile(filepath.Join(done.Workspace.Path, "code.txt")); string(data) != "changed" {
		t.Fatalf("the coding harness did not run in the keyed workspace: %q", data)
	}
	// The Issue is read again at approval, by its key.
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[1], "acme/widgets ENG-1234 ") {
		t.Fatalf("reader calls = %q", *calls)
	}
}

func TestRunKeyedApprovalRefusals(t *testing.T) {
	t.Parallel()
	o, calls := trackerPluginFixture(t, keyedIssueFixture())
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil || r.Readiness == nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	digest := r.Readiness.Plans[0].Digest
	o.AllowKeyedApproval = true
	for name, c := range map[string]struct {
		key, digest, want string
	}{
		"unknown digest": {"ENG-1234", strings.Repeat("b", 64), "no Molecule matches this approval digest"},
		"invalid digest": {"ENG-1234", "nope", "invalid approval digest"},
		"other key":      {"ENG-9999", digest, "issue or Formula changed; refresh intake"},
	} {
		o.Approve = c.digest
		if _, err := RunKeyed(context.Background(), o, c.key); err == nil || err.Error() != c.want {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("a refused approval read the Issue: %q", *calls)
	}

	// A closed Issue at approval starts no coding.
	closed := keyedIssueFixture()
	closed.State = "CLOSED"
	o.ReadIssueKey = func(context.Context, string, string, string) (Issue, error) { return closed, nil }
	o.Approve = digest
	if _, err := RunKeyed(context.Background(), o, "ENG-1234"); err == nil || !strings.Contains(err.Error(), "closed or does not match") {
		t.Fatalf("closed at approval: %v", err)
	}
	saved := readLedgerRecord(t, o.LabDir, r.ID)
	if saved.Status != "waiting" || len(saved.HarnessAttempts) != 0 || saved.Readiness.Phase != "scope-waiting" {
		t.Fatalf("after refusals: status %q phase %q attempts %d", saved.Status, saved.Readiness.Phase, len(saved.HarnessAttempts))
	}

	// The keyed policy path checks the same identity, then the policy, which a
	// human scope gate never satisfies.
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	o.ReadIssueKey = func(context.Context, string, string, string) (Issue, error) { return keyedIssueFixture(), nil }
	if _, err := resumeByPolicyKeyed(context.Background(), o, loaded, "ENG-9999", "acme/widgets"); err == nil || err.Error() != "issue or Formula changed; refresh intake" {
		t.Fatalf("policy, other key: %v", err)
	}
	if _, err := resumeByPolicyKeyed(context.Background(), o, loaded, "ENG-1234", "acme/widgets"); err == nil {
		t.Fatal("a human scope gate was approved by policy")
	}
	if saved = readLedgerRecord(t, o.LabDir, r.ID); len(saved.HarnessAttempts) != 0 || len(saved.PolicyDecisions) != 0 {
		t.Fatalf("policy refusal changed the Molecule: attempts %d decisions %d", len(saved.HarnessAttempts), len(saved.PolicyDecisions))
	}
}

// TestTrackerPluginSourceIsKeyedOnly: a numeric run with a tracker-plugin
// Formula is still refused as an unsupported intake source.
func TestTrackerPluginSourceIsKeyedOnly(t *testing.T) {
	t.Parallel()
	o, calls := trackerPluginFixture(t, keyedIssueFixture())
	o.Issue = "12"
	numeric := 0
	o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
		numeric++
		return Issue{}, nil
	}
	if _, err := Run(context.Background(), o); err == nil || err.Error() != "unsupported intake source" {
		t.Fatalf("Run = %v", err)
	}
	if numeric != 0 || len(*calls) != 0 {
		t.Fatalf("an Issue was read: %d, %q", numeric, *calls)
	}
	if entries, _ := os.ReadDir(filepath.Join(o.LabDir, "ledgers")); len(entries) != 0 {
		t.Fatalf("a ledger was written: %d", len(entries))
	}
}
