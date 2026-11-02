package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	fixCLITree     = "1111111111111111111111111111111111111111"
	fixCLIBase     = "2222222222222222222222222222222222222222"
	fixCLIRound1   = "3333333333333333333333333333333333333333"
	fixCLIParent   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixCLIRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fixCLIReviewers declares the same reviewer plugin twice with different
// autoFix levels, so each reviewer identity keeps its own threshold.
const fixCLIReviewers = `reviewers:
  - plugin: critic
    autoFix: medium
  - plugin: critic
    autoFix: high
`

// fixCLIFinding is one recorded finding of reviewer index.
type fixCLIFinding struct {
	index          int
	severity, path string
	line           int64
	title          string
}

func (f fixCLIFinding) fingerprint() string {
	return delivery.FindingFingerprint(delivery.ReviewerKey(f.index, "critic"), f.severity, f.path, f.title)
}

// The fixture's findings: reviewer 0 (autoFix medium) and reviewer 1 (autoFix high).
var (
	fixHigh0   = fixCLIFinding{0, "high", "src/app.go", 12, "Unchecked error"}
	fixLow0    = fixCLIFinding{0, "low", "src/style.go", 0, "Style nit"}
	fixMedium1 = fixCLIFinding{1, "medium", "src/db.go", 3, "Leaked handle"}
	fixHigh1   = fixCLIFinding{1, "high", "", 0, "No timeout"}
)

// fixCLILab is an approved Lab with two reviewers, a published GitHub receipt
// (with the given fix rounds) and one complete findings round on its head.
type fixCLILab struct {
	t       *testing.T
	lab     string
	receipt delivery.Receipt
	amended []molecule.FixRoundRequest
	record  molecule.Record
}

func newFixCLILab(t *testing.T, findings ...fixCLIFinding) *fixCLILab {
	t.Helper()
	server := plugintest.Server(t)
	lab := pluginListLab(t, "github-issues", fixCLIReviewers,
		pluginListPin{name: "critic", path: "plugins/critic.json", raw: plugintest.Manifest(t, server, "reviewer", "good")})
	fx := &fixCLILab{t: t, lab: lab}
	fx.receipt = delivery.Receipt{Preview: delivery.PreviewResult{MoleculeID: findingsCLIID, Repo: "acme/widgets", Tree: fixCLITree}, Title: "Fix", State: "published", Commit: findingsCLICommit, URL: "https://github.com/acme/widgets/pull/7"}
	fx.writeReceipt()
	fx.writeFindings(findingsCLICommit, true, findings...)
	fx.record = molecule.Record{ID: findingsCLIID, FormulaID: "sample", Issue: molecule.Issue{Repo: "acme/widgets", Number: 9}, Readiness: &readiness.State{Plans: []readiness.Proposal{{Digest: fixCLIParent, Plan: readiness.Plan{Revision: 1, Inputs: readiness.Inputs{BaseCommit: fixCLIBase, TargetBranch: "main"}}}}}}
	previousRead, previousAmend := fixRoundReadRecord, fixRoundAmend
	t.Cleanup(func() { fixRoundReadRecord, fixRoundAmend = previousRead, previousAmend })
	fixRoundReadRecord = func(string, string) (molecule.Record, error) { return fx.record, nil }
	fixRoundAmend = func(_ context.Context, _ string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error) {
		fx.amended = append(fx.amended, req)
		out := r
		state := *r.Readiness
		state.Plans = append(append([]readiness.Proposal{}, state.Plans...), readiness.Proposal{Digest: fixCLIRevision, Plan: readiness.Plan{Revision: 2, Inputs: state.Plans[0].Plan.Inputs, OperatorNote: req.Note}})
		out.Readiness = &state
		return out, nil
	}
	return fx
}

func (fx *fixCLILab) writeReceipt() {
	fx.t.Helper()
	raw, err := json.Marshal(fx.receipt)
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.lab, "deliveries"), 0o700); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.lab, "deliveries", findingsCLIID+".json"), raw, 0o600); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixCLILab) writeFindings(commit string, complete bool, findings ...fixCLIFinding) {
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
	raw, err := json.Marshal(delivery.FindingsRecord{SchemaVersion: "v1", MoleculeID: findingsCLIID, Rounds: []delivery.FindingsRound{round}})
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.lab, "deliveries", findingsCLIID+".findings.json"), raw, 0o600); err != nil {
		fx.t.Fatal(err)
	}
}

// addFixRound records a pushed fix round 1 answering the given findings, so
// the head moves to fixCLIRound1.
func (fx *fixCLILab) addFixRound(findings ...fixCLIFinding) {
	fps := []string{}
	for _, f := range findings {
		fps = append(fps, f.fingerprint())
	}
	fx.receipt.FixRounds = []delivery.FixRound{{Round: 1, Parent: findingsCLICommit, Commit: fixCLIRound1, Tree: fixCLITree, PlanDigest: fixCLIParent, VerificationTree: fixCLITree, Findings: fps, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: fixCLIRound1}}
	fx.writeReceipt()
}

func (fx *fixCLILab) propose(args ...string) (int, string, string) {
	return runCLI(append([]string{"findings", findingsCLIID, "--lab", fx.lab, "--propose"}, args...)...)
}

func fingerprints(req molecule.FixRoundRequest) []string {
	out := []string{}
	for _, f := range req.Findings {
		out = append(out, f.Fingerprint)
	}
	return out
}

func TestFixRoundProposePicksAtOrAboveEachReviewersAutoFix(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0, fixLow0, fixMedium1, fixHigh1)
	code, out, errOut := fx.propose()
	if code != 0 || errOut != "" || len(fx.amended) != 1 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	req := fx.amended[0]
	// Reviewer 0 takes medium and above; reviewer 1, the same plugin, only high.
	if want := []string{fixHigh0.fingerprint(), fixHigh1.fingerprint()}; !reflect.DeepEqual(fingerprints(req), want) {
		t.Fatalf("picked %q, want %q", fingerprints(req), want)
	}
	if req.ID != findingsCLIID || req.Head != findingsCLICommit || req.HeadTree != fixCLITree || req.Round != 1 || req.ExpectParent != fixCLIParent {
		t.Fatalf("request = %+v", req)
	}
	wantAcceptance := []readiness.Criterion{
		{Description: "Fix the reviewer finding [high] src/app.go:12: Unchecked error (finding:" + fixHigh0.fingerprint() + ")", Evidence: []string{"finding:" + fixHigh0.fingerprint()}},
		{Description: "Fix the reviewer finding [high] -: No timeout (finding:" + fixHigh1.fingerprint() + ")", Evidence: []string{"finding:" + fixHigh1.fingerprint()}},
	}
	if !reflect.DeepEqual(req.Acceptance, wantAcceptance) {
		t.Fatalf("acceptance = %+v", req.Acceptance)
	}
	if want := []molecule.ScopeAddition{{Path: "src/app.go", Reason: "reviewer finding " + fixHigh0.fingerprint()}}; !reflect.DeepEqual(req.Scope, want) {
		t.Fatalf("scope = %+v", req.Scope)
	}
	if req.Findings[0].Reviewer != "0:critic" || req.Findings[1].Reviewer != "1:critic" || req.Findings[0].Line != 12 {
		t.Fatalf("findings = %+v", req.Findings)
	}
	for _, want := range []string{"Fix round 1:", "2 reviewer finding(s)", "findings round 1 of 1", "0:critic (1), 1:critic (1)"} {
		if !strings.Contains(req.Note, want) {
			t.Fatalf("note %q lacks %q", req.Note, want)
		}
	}
	for _, want := range []string{
		"Fix round 1 proposed for " + findingsCLIID + ": 2 finding(s); revision 2:",
		"Review the full ledger, then approve exactly this plan:\nforgecell run 9 --lab " + shellWord(fx.lab) + " --formula 'sample' --base '" + fixCLIBase + "' --target 'main' --approve " + fixCLIRevision + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFixRoundProposeIncludesABelowThresholdFinding(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0, fixLow0, fixMedium1)
	code, out, errOut := fx.propose("--include", fixLow0.fingerprint(), "--include", fixMedium1.fingerprint())
	if code != 0 || len(fx.amended) != 1 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{fixHigh0.fingerprint(), fixMedium1.fingerprint(), fixLow0.fingerprint()}; !reflect.DeepEqual(fingerprints(fx.amended[0]), want) {
		t.Fatalf("picked %q, want %q", fingerprints(fx.amended[0]), want)
	}
	// An include that is not an open finding of the round, or not a fingerprint, is refused.
	for _, include := range []string{strings.Repeat("e", 64), "not-a-fingerprint"} {
		fx.amended = nil
		code, out, errOut = fx.propose("--include", include)
		if code == 0 || len(fx.amended) != 0 || errOut == "" {
			t.Fatalf("include %q: exit %d\n%s%s", include, code, out, errOut)
		}
	}
	args := []string{}
	for i := 0; i <= maxFixRoundIncludes; i++ {
		args = append(args, "--include", fmt.Sprintf("%064x", i))
	}
	if code, out, errOut = fx.propose(args...); code == 0 || !strings.Contains(errOut, "at most 50") {
		t.Fatalf("51 includes: exit %d\n%s%s", code, out, errOut)
	}
}

func TestFixRoundProposeLeavesARepeatedFindingOut(t *testing.T) {
	fx := newFixCLILab(t)
	fx.addFixRound(fixHigh0)
	fx.writeFindings(fixCLIRound1, true, fixHigh0, fixHigh1)
	code, out, errOut := fx.propose()
	if code != 0 || len(fx.amended) != 1 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "finding "+fixHigh0.fingerprint()+" came back after a fix; it is not proposed again\n") {
		t.Fatalf("no repeat line in:\n%s", out)
	}
	req := fx.amended[0]
	if want := []string{fixHigh1.fingerprint()}; !reflect.DeepEqual(fingerprints(req), want) || req.Round != 2 || req.Head != fixCLIRound1 {
		t.Fatalf("request = %+v", req)
	}

	// --include proposes it again explicitly.
	fx.amended = nil
	code, out, errOut = fx.propose("--include", fixHigh0.fingerprint())
	if code != 0 || len(fx.amended) != 1 || strings.Contains(out, "came back") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{fixHigh0.fingerprint(), fixHigh1.fingerprint()}; !reflect.DeepEqual(fingerprints(fx.amended[0]), want) {
		t.Fatalf("picked %q", fingerprints(fx.amended[0]))
	}

	// Only a repeated finding: nothing is proposed.
	fx.amended = nil
	fx.writeFindings(fixCLIRound1, true, fixHigh0)
	if code, out, _ = fx.propose(); code != 0 || len(fx.amended) != 0 || !strings.HasSuffix(out, "no findings to propose\n") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestFixRoundProposeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fx *fixCLILab)
		want   string
	}{
		{"no round on the head", func(fx *fixCLILab) { fx.addFixRound() }, "collect findings for the current head first"},
		{"an incomplete round on the head", func(fx *fixCLILab) { fx.writeFindings(findingsCLICommit, false, fixHigh0) }, "collect findings for the current head first"},
		{"an unpublished receipt", func(fx *fixCLILab) {
			fx.receipt.State = "committed"
			fx.writeReceipt()
		}, "not published"},
		{"no receipt", func(fx *fixCLILab) {
			os.Remove(filepath.Join(fx.lab, "deliveries", findingsCLIID+".json"))
		}, "no delivery receipt exists"},
		{"the third round already pushed", func(fx *fixCLILab) {
			parent := findingsCLICommit
			for i, c := range []string{"4", "5", "6"} {
				commit := strings.Repeat(c, 40)
				fx.receipt.FixRounds = append(fx.receipt.FixRounds, delivery.FixRound{Round: i + 1, Parent: parent, Commit: commit, Tree: fixCLITree, PlanDigest: fixCLIParent, VerificationTree: fixCLITree, Findings: []string{}, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: commit})
				parent = commit
			}
			fx.writeReceipt()
		}, "fix rounds stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixCLILab(t, fixHigh0)
			tc.mutate(fx)
			code, out, errOut := fx.propose()
			if code != 1 || !strings.Contains(errOut, tc.want) || len(fx.amended) != 0 || strings.Contains(errOut, fx.lab) {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
		})
	}
}

func TestFixRoundProposeWithNoFindingsExitsZero(t *testing.T) {
	fx := newFixCLILab(t, fixLow0, fixMedium1)
	code, out, errOut := fx.propose()
	if code != 0 || out != "no findings to propose\n" || errOut != "" || len(fx.amended) != 0 {
		t.Fatalf("exit %d\n%q%q", code, out, errOut)
	}
}

func TestFixRoundProposePrintsTheKeyedForm(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0)
	fx.record.Issue = molecule.Issue{Repo: "acme/widgets", Key: "ENG-12"}
	code, out, errOut := fx.propose()
	if code != 0 || !strings.Contains(out, "forgecell run 'ENG-12' --lab "+shellWord(fx.lab)+" --formula 'sample' --base '"+fixCLIBase+"' --target 'main' --approve "+fixCLIRevision+"\n") || strings.Contains(out, "run 0 ") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestFixRoundFlagsRefuseMixedModes(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0)
	for _, args := range [][]string{
		{"--propose", "--push"},
		{"--propose", "--collect"},
		{"--push", "--json"},
		{"--include", fixHigh0.fingerprint()},
		{"--approve", fixCLIParent},
		{"--confirm"},
		{"--propose", "--title", "x"},
	} {
		code, out, errOut := runCLI(append([]string{"findings", findingsCLIID, "--lab", fx.lab}, args...)...)
		if code == 0 || !strings.HasPrefix(errOut, "forgecell:") || len(fx.amended) != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
}

// stubFixRoundPush replaces the preview and the push, recording each push's
// options.
func stubFixRoundPush(t *testing.T, preview delivery.FixRoundPreview) *[]delivery.FixRoundPushOptions {
	t.Helper()
	previousPreview, previousPush := fixRoundPreview, fixRoundPush
	t.Cleanup(func() { fixRoundPreview, fixRoundPush = previousPreview, previousPush })
	var pushes []delivery.FixRoundPushOptions
	fixRoundPreview = func(context.Context, delivery.FixRoundPushOptions) (delivery.FixRoundPreview, error) {
		return preview, nil
	}
	fixRoundPush = func(_ context.Context, o delivery.FixRoundPushOptions) (delivery.Receipt, error) {
		pushes = append(pushes, o)
		if o.Approve == "" {
			return delivery.Receipt{}, &delivery.FixRoundApprovalRequired{Preview: preview}
		}
		return delivery.Receipt{Preview: delivery.PreviewResult{Branch: "forgecell/issue-12"}, FixRounds: []delivery.FixRound{{Round: preview.Round, Commit: fixCLIRound1}}}, nil
	}
	return &pushes
}

func TestFindingsPushIsTwoStep(t *testing.T) {
	lab := t.TempDir()
	preview := delivery.FixRoundPreview{MoleculeID: findingsCLIID, Round: 1, Digest: fixCLIRevision, AcceptanceUnverified: true}
	pushes := stubFixRoundPush(t, preview)
	code, out, errOut := runCLI("findings", findingsCLIID, "--lab", lab, "--push")
	var shown delivery.FixRoundPreview
	if code != 0 || json.Unmarshal([]byte(out), &shown) != nil || !reflect.DeepEqual(shown, preview) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := "Review this fix round 1 push, then push it with forgecell findings " + findingsCLIID + " --push --approve " + fixCLIRevision + " --confirm\n"; errOut != want {
		t.Fatalf("stderr %q, want %q", errOut, want)
	}
	code, out, errOut = runCLI("findings", findingsCLIID, "--lab", lab, "--push", "--approve", fixCLIRevision, "--confirm", "--title", "fix: tidy")
	if code != 0 || out != "Fix round 1 pushed to forgecell/issue-12: commit "+fixCLIRound1+" is the change request's head. Nothing was merged or marked ready.\n" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	last := (*pushes)[len(*pushes)-1]
	if last.Approve != fixCLIRevision || !last.Confirm || last.Title != "fix: tidy" || last.MoleculeID != findingsCLIID {
		t.Fatalf("push options = %+v", last)
	}
}

// fixRoundRunRecord is a run's record: verified, with a fix-round latest plan
// when round is set.
func fixRoundRunRecord(round int) molecule.Record {
	plan := readiness.Plan{Revision: 2, Inputs: readiness.Inputs{BaseCommit: fixCLIBase, TargetBranch: "main"}}
	if round > 0 {
		plan.FixRound = &readiness.FixRoundRef{Round: round, Head: findingsCLICommit, HeadTree: fixCLITree}
	}
	return molecule.Record{ID: findingsCLIID, Status: "waiting", Issue: molecule.Issue{Repo: "acme/widgets", Number: 12, Title: "Widget"}, Readiness: &readiness.State{Phase: "review-waiting", Plans: []readiness.Proposal{{Digest: fixCLIParent, Plan: plan}}}, Verification: &verification.Result{RequiredChecksPassed: true, IndependentAcceptanceVerified: true}}
}

func TestFixRoundAutomaticPushWhenAllGatesAreGreen(t *testing.T) {
	pushes := stubFixRoundPush(t, delivery.FixRoundPreview{Round: 1, Digest: fixCLIRevision, ReviewGreen: true})
	var out, stderr bytes.Buffer
	if code := fixRoundAfterRun(context.Background(), &out, &stderr, t.TempDir(), fixRoundRunRecord(1)); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(*pushes) != 1 || (*pushes)[0].Approve != fixCLIRevision || (*pushes)[0].Confirm || (*pushes)[0].MoleculeID != findingsCLIID {
		t.Fatalf("pushes = %+v", *pushes)
	}
	if !strings.Contains(out.String(), "Fix round 1 pushed to forgecell/issue-12") {
		t.Fatalf("output %q", out.String())
	}
}

func TestFixRoundAutomaticPushPrintsTheReadyLineOtherwise(t *testing.T) {
	for _, preview := range []delivery.FixRoundPreview{
		{Round: 1, Digest: fixCLIRevision, ReviewGreen: false},
		{Round: 1, Digest: fixCLIRevision, ReviewGreen: true, AcceptanceUnverified: true},
	} {
		pushes := stubFixRoundPush(t, preview)
		var out, stderr bytes.Buffer
		if code := fixRoundAfterRun(context.Background(), &out, &stderr, t.TempDir(), fixRoundRunRecord(1)); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if want := "fix round 1 is ready; push it with forgecell findings " + findingsCLIID + " --push --approve " + fixCLIRevision + " --confirm\n"; out.String() != want || len(*pushes) != 0 {
			t.Fatalf("output %q, pushes %+v", out.String(), *pushes)
		}
	}
	// Failed checks or a plan still waiting for approval push nothing and print nothing.
	stubFixRoundPush(t, delivery.FixRoundPreview{Round: 1, Digest: fixCLIRevision, ReviewGreen: true})
	failed := fixRoundRunRecord(1)
	failed.Verification.RequiredChecksPassed = false
	waiting := fixRoundRunRecord(1)
	waiting.Readiness.Phase = "scope-waiting"
	for _, r := range []molecule.Record{failed, waiting} {
		var out, stderr bytes.Buffer
		if code := fixRoundAfterRun(context.Background(), &out, &stderr, t.TempDir(), r); code != 0 || out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("exit %d, %q %q", code, out.String(), stderr.String())
		}
	}
}

func TestFixRoundRunCompletionHook(t *testing.T) {
	previous := runIssue
	t.Cleanup(func() { runIssue = previous })
	pushes := stubFixRoundPush(t, delivery.FixRoundPreview{Round: 1, Digest: fixCLIRevision})
	for _, round := range []int{0, 1} {
		lab := t.TempDir()
		record := fixRoundRunRecord(round)
		record.LabDir = lab
		runIssue = func(context.Context, molecule.Options) (molecule.Record, error) { return record, nil }
		var out, stderr bytes.Buffer
		code := Run(context.Background(), []string{"run", "12", "--lab", lab}, nil, &out, &stderr, "test")
		var want, wantErr bytes.Buffer
		presentRun(&want, record)
		renderLearnNotices(&want, &wantErr, lab)
		if round == 0 {
			// A plan without a fix round prints exactly what run printed before.
			if code != 0 || out.String() != want.String() {
				t.Fatalf("exit %d\n%q\nwant\n%q", code, out.String(), want.String())
			}
			continue
		}
		ready := "fix round 1 is ready; push it with forgecell findings " + findingsCLIID + " --push --approve " + fixCLIRevision + " --confirm\n"
		if code != 0 || out.String() != want.String()+ready || len(*pushes) != 0 {
			t.Fatalf("exit %d\n%q\nwant\n%q", code, out.String(), want.String()+ready)
		}
	}
}
