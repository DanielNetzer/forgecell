package fixround

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	frID       = "mol-7"
	frCommit   = "0123456789abcdef0123456789abcdef01234567"
	frTree     = "1111111111111111111111111111111111111111"
	frBase     = "2222222222222222222222222222222222222222"
	frRound1   = "3333333333333333333333333333333333333333"
	frParent   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	frRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// frFormula declares the same reviewer plugin twice with different autoFix
// levels, so each reviewer identity keeps its own threshold.
var frFormula = formula.Formula{ReviewerPlugins: []formula.ReviewerRef{{Plugin: "critic", AutoFix: "medium"}, {Plugin: "critic", AutoFix: "high"}}}

type frFinding struct {
	index          int
	severity, path string
	line           int64
	title          string
}

func (f frFinding) fingerprint() string {
	return delivery.FindingFingerprint(delivery.ReviewerKey(f.index, "critic"), f.severity, f.path, f.title)
}

var (
	frHigh0   = frFinding{0, "high", "src/app.go", 12, "Unchecked error"}
	frLow0    = frFinding{0, "low", "src/style.go", 0, "Style nit"}
	frMedium1 = frFinding{1, "medium", "src/db.go", 3, "Leaked handle"}
	frHigh1   = frFinding{1, "high", "", 0, "No timeout"}
)

// frLab is a Lab with a published GitHub receipt and one complete findings
// round on its head, whose ledger reads and amendments go through stubs.
type frLab struct {
	t       *testing.T
	lab     string
	receipt delivery.Receipt
	record  molecule.Record
	readErr error
	amended []molecule.FixRoundRequest
}

func newFRLab(t *testing.T, findings ...frFinding) *frLab {
	t.Helper()
	fx := &frLab{t: t, lab: t.TempDir()}
	fx.receipt = delivery.Receipt{Preview: delivery.PreviewResult{MoleculeID: frID, Repo: "acme/widgets", Tree: frTree}, Title: "Fix", State: "published", Commit: frCommit, URL: "https://github.com/acme/widgets/pull/7"}
	fx.writeReceipt()
	fx.writeFindings(frCommit, true, findings...)
	fx.record = molecule.Record{ID: frID, FormulaID: "sample", Issue: molecule.Issue{Repo: "acme/widgets", Number: 9}, Readiness: &readiness.State{Plans: []readiness.Proposal{{Digest: frParent, Plan: readiness.Plan{Revision: 1, Inputs: readiness.Inputs{BaseCommit: frBase, TargetBranch: "main"}}}}}}
	return fx
}

func (fx *frLab) writeReceipt() {
	fx.t.Helper()
	raw, err := json.Marshal(fx.receipt)
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.lab, "deliveries"), 0o700); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.lab, "deliveries", frID+".json"), raw, 0o600); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *frLab) writeFindings(commit string, complete bool, findings ...frFinding) {
	fx.t.Helper()
	round := delivery.FindingsRound{Commit: commit, Ref: "7", CollectedAt: "2026-11-02T12:00:00Z", Complete: complete}
	for i := 0; i < 2; i++ {
		entry := delivery.ReviewerRound{Reviewer: delivery.ReviewerKey(i, "critic"), Plugin: "critic", Status: delivery.FindingsComplete, Findings: []delivery.Finding{}}
		for _, f := range findings {
			if f.index == i {
				entry.Findings = append(entry.Findings, delivery.Finding{Fingerprint: f.fingerprint(), Severity: f.severity, Path: f.path, Line: f.line, Title: f.title, PluginID: "F"})
			}
		}
		round.Reviewers = append(round.Reviewers, entry)
	}
	raw, err := json.Marshal(delivery.FindingsRecord{SchemaVersion: "v1", MoleculeID: frID, Rounds: []delivery.FindingsRound{round}})
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.lab, "deliveries", frID+".findings.json"), raw, 0o600); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *frLab) addFixRound(findings ...frFinding) {
	fps := []string{}
	for _, f := range findings {
		fps = append(fps, f.fingerprint())
	}
	fx.receipt.FixRounds = []delivery.FixRound{{Round: 1, Parent: frCommit, Commit: frRound1, Tree: frTree, PlanDigest: frParent, VerificationTree: frTree, Findings: fps, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: frRound1}}
	fx.writeReceipt()
}

func (fx *frLab) options() Options {
	f := frFormula
	return Options{
		Formula: &f,
		ReadRecord: func(string, string) (molecule.Record, error) {
			return fx.record, fx.readErr
		},
		Amend: func(_ context.Context, _ string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error) {
			fx.amended = append(fx.amended, req)
			out := r
			state := *r.Readiness
			state.Plans = append(append([]readiness.Proposal{}, state.Plans...), readiness.Proposal{Digest: frRevision, Plan: readiness.Plan{Revision: 2, Inputs: state.Plans[0].Plan.Inputs, OperatorNote: req.Note}})
			out.Readiness = &state
			return out, nil
		},
	}
}

func (fx *frLab) propose(includes ...string) (Proposal, error) {
	fx.amended = nil
	return Propose(context.Background(), fx.lab, frID, includes, fx.options())
}

func pickedFingerprints(p Proposal) []string {
	out := []string{}
	for _, f := range p.Picked {
		out = append(out, f.Fingerprint)
	}
	return out
}

func TestProposePicksAtOrAboveEachReviewersAutoFix(t *testing.T) {
	fx := newFRLab(t, frHigh0, frLow0, frMedium1, frHigh1)
	p, err := fx.propose()
	if err != nil || p.None || len(fx.amended) != 1 || len(p.Skipped) != 0 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	if want := []string{frHigh0.fingerprint(), frHigh1.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) {
		t.Fatalf("picked %q, want %q", pickedFingerprints(p), want)
	}
	req := fx.amended[0]
	if !reflect.DeepEqual(req, p.Request) || p.Round != 1 || p.Record.Readiness.Plans[1].Digest != frRevision {
		t.Fatalf("proposal = %+v", p)
	}
	if req.ID != frID || req.Head != frCommit || req.HeadTree != frTree || req.Round != 1 || req.ExpectParent != frParent {
		t.Fatalf("request = %+v", req)
	}
	wantAcceptance := []readiness.Criterion{
		{Description: "Fix the reviewer finding [high] src/app.go:12: Unchecked error (finding:" + frHigh0.fingerprint() + ")", Evidence: []string{"finding:" + frHigh0.fingerprint()}},
		{Description: "Fix the reviewer finding [high] -: No timeout (finding:" + frHigh1.fingerprint() + ")", Evidence: []string{"finding:" + frHigh1.fingerprint()}},
	}
	if !reflect.DeepEqual(req.Acceptance, wantAcceptance) {
		t.Fatalf("acceptance = %+v", req.Acceptance)
	}
	if want := []molecule.ScopeAddition{{Path: "src/app.go", Reason: "reviewer finding " + frHigh0.fingerprint()}}; !reflect.DeepEqual(req.Scope, want) {
		t.Fatalf("scope = %+v", req.Scope)
	}
	if req.Findings[0].Reviewer != "0:critic" || req.Findings[1].Reviewer != "1:critic" || req.Findings[0].Line != 12 {
		t.Fatalf("findings = %+v", req.Findings)
	}
	want := "Fix round 1: 2 reviewer finding(s) from findings round 1 of 1 on commit " + frCommit + ", by reviewer 0:critic (1), 1:critic (1); 1 scope path(s)."
	if req.Note != want {
		t.Fatalf("note %q, want %q", req.Note, want)
	}
}

func TestProposeIncludes(t *testing.T) {
	fx := newFRLab(t, frHigh0, frLow0, frMedium1)
	p, err := fx.propose(frLow0.fingerprint(), frMedium1.fingerprint())
	if err != nil || len(fx.amended) != 1 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	if want := []string{frHigh0.fingerprint(), frMedium1.fingerprint(), frLow0.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) {
		t.Fatalf("picked %q, want %q", pickedFingerprints(p), want)
	}
	// An include that is not an open finding of the round, or not a fingerprint, is refused.
	if _, err := fx.propose(strings.Repeat("e", 64)); err == nil || !strings.Contains(err.Error(), "is not an open finding") || len(fx.amended) != 0 {
		t.Fatalf("unknown include: %v", err)
	}
	if _, err := fx.propose("not-a-fingerprint"); err == nil || len(fx.amended) != 0 {
		t.Fatalf("malformed include: %v", err)
	}
	var many []string
	for i := 0; i <= MaxIncludes; i++ {
		many = append(many, fmt.Sprintf("%064x", i))
	}
	if _, err := fx.propose(many...); err == nil || !strings.Contains(err.Error(), "at most 50") || len(fx.amended) != 0 {
		t.Fatalf("51 includes: %v", err)
	}
}

func TestProposeLeavesARepeatedFindingOut(t *testing.T) {
	fx := newFRLab(t)
	fx.addFixRound(frHigh0)
	fx.writeFindings(frRound1, true, frHigh0, frHigh1)
	p, err := fx.propose()
	if err != nil || len(fx.amended) != 1 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	if want := []Skip{{Fingerprint: frHigh0.fingerprint(), Reason: SkipRepeated}}; !reflect.DeepEqual(p.Skipped, want) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	if want := []string{frHigh1.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) || p.Round != 2 || fx.amended[0].Head != frRound1 {
		t.Fatalf("proposal %+v", p)
	}
	// An include proposes it again explicitly.
	p, err = fx.propose(frHigh0.fingerprint())
	if err != nil || len(p.Skipped) != 0 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	if want := []string{frHigh0.fingerprint(), frHigh1.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) {
		t.Fatalf("picked %q", pickedFingerprints(p))
	}
	// Only a repeated finding: nothing is proposed and nothing amended.
	fx.writeFindings(frRound1, true, frHigh0)
	p, err = fx.propose()
	if err != nil || !p.None || len(fx.amended) != 0 || len(p.Skipped) != 1 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	// The skipped findings come back with a refusal after selection.
	p, err = fx.propose(strings.Repeat("e", 64))
	if err == nil || len(p.Skipped) != 1 || p.Skipped[0].Fingerprint != frHigh0.fingerprint() {
		t.Fatalf("proposal %+v, %v", p, err)
	}
}

func TestProposeWithNothingAtThreshold(t *testing.T) {
	fx := newFRLab(t, frLow0, frMedium1)
	p, err := fx.propose()
	if err != nil || !p.None || len(p.Picked) != 0 || len(fx.amended) != 0 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
}

func TestProposeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fx *frLab)
		want   string
	}{
		{"no round on the head", func(fx *frLab) { fx.addFixRound() }, "collect findings for the current head first"},
		{"an incomplete round on the head", func(fx *frLab) { fx.writeFindings(frCommit, false, frHigh0) }, "collect findings for the current head first"},
		{"an unpublished receipt", func(fx *frLab) {
			fx.receipt.State = "committed"
			fx.writeReceipt()
		}, "not published"},
		{"no receipt", func(fx *frLab) {
			os.Remove(filepath.Join(fx.lab, "deliveries", frID+".json"))
		}, "no delivery receipt exists"},
		{"the third round already pushed", func(fx *frLab) {
			parent := frCommit
			for i, c := range []string{"4", "5", "6"} {
				commit := strings.Repeat(c, 40)
				fx.receipt.FixRounds = append(fx.receipt.FixRounds, delivery.FixRound{Round: i + 1, Parent: parent, Commit: commit, Tree: frTree, PlanDigest: frParent, VerificationTree: frTree, Findings: []string{}, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: commit})
				parent = commit
			}
			fx.writeReceipt()
		}, "fix rounds stop"},
		{"an unreadable ledger", func(fx *frLab) { fx.readErr = errors.New("read " + fx.lab) }, "the Molecule ledger cannot be read"},
		{"no readiness plan", func(fx *frLab) { fx.record.Readiness = nil }, "no readiness plan to amend"},
		{"more findings than one round answers", func(fx *frLab) {
			var many []frFinding
			for i := 0; i <= MaxProposed; i++ {
				many = append(many, frFinding{0, "high", "src/app.go", 1, fmt.Sprintf("finding %d", i)})
			}
			fx.writeFindings(frCommit, true, many...)
		}, "more than one fix round answers (64)"},
		{"a symlinked findings record", func(fx *frLab) {
			file := filepath.Join(fx.lab, "deliveries", frID+".findings.json")
			if err := os.Rename(file, filepath.Join(fx.lab, "elsewhere.json")); err != nil {
				fx.t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(fx.lab, "elsewhere.json"), file); err != nil {
				fx.t.Fatal(err)
			}
		}, "not a bounded regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFRLab(t, frHigh0)
			tc.mutate(fx)
			_, err := fx.propose()
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(fx.amended) != 0 || strings.Contains(err.Error(), fx.lab) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProposeLoadsTheActiveFormulaWhenNoneIsGiven(t *testing.T) {
	fx := newFRLab(t, frHigh0)
	o := fx.options()
	o.Formula = nil
	if _, err := Propose(context.Background(), fx.lab, frID, nil, o); err == nil || !strings.Contains(err.Error(), "no approved active Formula") || len(fx.amended) != 0 {
		t.Fatalf("no active Formula: %v", err)
	}
}

func TestProposeNeverPicksADismissedFinding(t *testing.T) {
	fx := newFRLab(t, frHigh0, frLow0, frHigh1)
	now := func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }
	if err := delivery.DismissFinding(fx.lab, frID, frHigh0.fingerprint(), "Checked by the caller", now); err != nil {
		t.Fatal(err)
	}
	if err := delivery.DismissFinding(fx.lab, frID, frLow0.fingerprint(), "Style is fine", now); err != nil {
		t.Fatal(err)
	}
	p, err := fx.propose()
	if err != nil || len(fx.amended) != 1 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
	// The dismissed autoFix finding is skipped; the dismissed low one was never a candidate.
	if want := []Skip{{Fingerprint: frHigh0.fingerprint(), Reason: SkipDismissed}}; !reflect.DeepEqual(p.Skipped, want) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	if want := []string{frHigh1.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) {
		t.Fatalf("picked %q", pickedFingerprints(p))
	}
	// Even an explicit include does not pick a dismissed finding, and it is not refused as not open.
	p, err = fx.propose(frHigh0.fingerprint(), frLow0.fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{frHigh1.fingerprint()}; !reflect.DeepEqual(pickedFingerprints(p), want) || len(p.Skipped) != 2 {
		t.Fatalf("proposal %+v", p)
	}
	// Once undismissed, it is proposed again.
	if err := delivery.UndismissFinding(fx.lab, frID, frHigh0.fingerprint()); err != nil {
		t.Fatal(err)
	}
	p, err = fx.propose()
	if err != nil || !reflect.DeepEqual(pickedFingerprints(p), []string{frHigh0.fingerprint(), frHigh1.fingerprint()}) || len(p.Skipped) != 0 {
		t.Fatalf("proposal %+v, %v", p, err)
	}
}

const frFormulaYAML = "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  source: github-issues\n  repo: acme/widgets\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n"

const frForgeYAML = "plugins:\n  - name: gitlab\n    manifest: plugins/gitlab/plugin.json\n    sha256: " + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "\nforge:\n  plugin: gitlab\n  project: acme/widgets\n"

func frWriteLedger(t *testing.T, lab, id, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func frSnapshotLedger(t *testing.T, lab, id, yaml string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schemaVersion": "v0", "kind": "molecule", "id": id, "formulaSnapshot": map[string]string{"yaml": yaml, "sha256": strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	frWriteLedger(t, lab, id, string(raw))
}

// frActivate makes yaml the Lab's approved active Formula.
func frActivate(t *testing.T, lab, yaml string) {
	t.Helper()
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "fixround", SourceKind: "test-review", SourceID: "fixround"}
	if err := formula.BeginActivation(lab, yaml, a, "absent", "absent"); err != nil {
		t.Fatal(err)
	}
	if err := formula.ApplyActivation(lab, a); err != nil {
		t.Fatal(err)
	}
	if err := formula.CompleteActivation(lab, a); err != nil {
		t.Fatal(err)
	}
}

func TestMoleculeForgeUsesTheLedgerSnapshot(t *testing.T) {
	lab := t.TempDir()
	// The active Formula delivers through GitHub; the Molecule's snapshot through a plugin.
	frActivate(t, lab, frFormulaYAML)
	frSnapshotLedger(t, lab, "mol-plugin", frFormulaYAML+frForgeYAML)
	frSnapshotLedger(t, lab, "mol-github", frFormulaYAML)
	f, plugin, err := SnapshotFormula(lab, "mol-plugin")
	if err != nil || !plugin || f.Forge == nil || f.Forge.Plugin != "gitlab" || f.Forge.Project != "acme/widgets" {
		t.Fatalf("plugin snapshot = %+v, %t, %v", f.Forge, plugin, err)
	}
	// The snapshot's plugin forge is selected, though the active Formula is GitHub.
	changes, meta, ok, err := MoleculeForge(lab, "mol-plugin", func(string) (string, bool) { return "", false })
	if !ok || err != nil || changes == nil || meta.Name != "gitlab" || meta.Project != "acme/widgets" || meta.Calls == nil {
		t.Fatalf("plugin snapshot forge: %+v, %t, %v", meta, ok, err)
	}
	// GitHub, an absent ledger and an unsafe id are GitHub, with no forge.
	for _, id := range []string{"mol-github", "mol-absent", "../ledgers/mol-plugin"} {
		changes, meta, ok, err := MoleculeForge(lab, id, nil)
		if ok || err != nil || changes != nil || meta != (delivery.PluginForge{}) {
			t.Fatalf("%s: %t, %v", id, ok, err)
		}
	}
	// A Lab without any active Formula still selects by the snapshot alone.
	other := t.TempDir()
	frSnapshotLedger(t, other, "mol-github", frFormulaYAML)
	if _, _, ok, err := MoleculeForge(other, "mol-github", nil); ok || err != nil {
		t.Fatalf("GitHub snapshot: %t, %v", ok, err)
	}
}

func TestMoleculeForgeNeverFailsOpenToGitHub(t *testing.T) {
	plugin := t.TempDir()
	frSnapshotLedger(t, plugin, "mol-plugin", frFormulaYAML+frForgeYAML)
	symlinked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(symlinked, "ledgers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(plugin, "ledgers", "mol-plugin.json"), filepath.Join(symlinked, "ledgers", "mol-bad.json")); err != nil {
		t.Fatal(err)
	}
	corrupt := t.TempDir()
	frWriteLedger(t, corrupt, "mol-bad", `{"formulaSnapshot":{"yaml":`)
	unparsable := t.TempDir()
	frSnapshotLedger(t, unparsable, "mol-bad", "schemaVersion: v0\nkind: formula\nforge: [not, a, block\n")
	oversized := t.TempDir()
	frWriteLedger(t, oversized, "mol-bad", `{"pad":"`+strings.Repeat("x", MaxForgeLedgerBytes)+`"}`)
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "ledgers", "mol-bad.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"symlinked":  {symlinked, "cannot be read"},
		"corrupt":    {corrupt, "is malformed"},
		"unparsable": {unparsable, "does not parse"},
		"oversized":  {oversized, "exceeds its size bound"},
		"directory":  {directory, "cannot be read"},
	} {
		_, _, ok, err := MoleculeForge(c[0], "mol-bad", nil)
		if ok || err == nil || !strings.Contains(err.Error(), c[1]) || strings.Contains(err.Error(), c[0]) {
			t.Fatalf("%s: %t, %v", name, ok, err)
		}
		// A push with an approval refuses before any process or write.
		if _, err := Push(context.Background(), c[0], "mol-bad", frRevision, "", false); err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Fatalf("%s push: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(c[0], "deliveries")); !os.IsNotExist(err) {
			t.Fatalf("%s: deliveries written: %v", name, err)
		}
	}
}

func TestCollectWithoutReviewers(t *testing.T) {
	lab := t.TempDir()
	if _, err := Collect(context.Background(), lab, frID, formula.Formula{}); !errors.Is(err, delivery.ErrNoReviewers) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(lab, "deliveries")); !os.IsNotExist(err) {
		t.Fatalf("a refused collection wrote: %v", err)
	}
}

func TestCollectRefusesADemoLabFirst(t *testing.T) {
	lab := t.TempDir()
	if err := os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"activeFormulaId":"forgecell-demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The demo guard runs before anything else, even the reviewers check, so
	// no reviewer or forge plugin can start.
	if _, err := Collect(context.Background(), lab, frID, formula.Formula{}); !errors.Is(err, delivery.ErrDemoLab) {
		t.Fatalf("err = %v", err)
	}
	// A lab.json that cannot be checked fails closed.
	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, "lab.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(context.Background(), unreadable, frID, formula.Formula{}); err == nil || errors.Is(err, delivery.ErrNoReviewers) || strings.Contains(err.Error(), unreadable) {
		t.Fatalf("unreadable lab.json: %v", err)
	}
	for _, dir := range []string{lab, unreadable} {
		if _, err := os.Lstat(filepath.Join(dir, "deliveries")); !os.IsNotExist(err) {
			t.Fatalf("a refused collection wrote: %v", err)
		}
	}
}
