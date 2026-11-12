package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	findingsTestID     = "mol-350-abc123"
	findingsTestCommit = "1111111111111111111111111111111111111111"
	findingsTestTree   = "2222222222222222222222222222222222222222"
	findingsTestFixed  = "3333333333333333333333333333333333333333"
	findingsTestFixTr  = "4444444444444444444444444444444444444444"
	findingsTestVerTr  = "5555555555555555555555555555555555555555"
	findingsTestSecret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	findingsTestPlan    = strings.Repeat("d1", 32)
	findingsTestFixPlan = strings.Repeat("f2", 32)
	findingsTestDigest  = strings.Repeat("9a", 32)
)

// findingsTestFP is a stable fingerprint for a test finding name.
func findingsTestFP(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// findingsTestYAML is a Formula snapshot declaring one reviewer plugin, or none.
func findingsTestYAML(reviewers bool) string {
	yaml := "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  repo: o/r\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n"
	if reviewers {
		yaml += "plugins:\n  - name: critic\n    manifest: plugins/critic.json\n    sha256: " + strings.Repeat("a", 64) + "\nreviewers:\n  - plugin: critic\n    autoFix: high\n"
	}
	return yaml
}

// findingsTestRecord is a published Molecule whose snapshot declares a
// reviewer (or none), waiting at the review gate of its first plan.
func findingsTestRecord(lab, id string, reviewers bool) molecule.Record {
	yaml := findingsTestYAML(reviewers)
	sum := sha256.Sum256([]byte(yaml))
	return molecule.Record{
		SchemaVersion: "v0", Kind: "molecule", ID: id, Status: "waiting", FormulaID: "sample", Mode: "ticket", StartedAt: "2026-11-01T10:00:00Z", FinishedAt: "2026-11-01T11:00:00Z", LabDir: lab,
		Issue:           molecule.Issue{Repo: "o/r", Number: 350, Title: "Fix it", URL: "https://github.com/o/r/issues/350", State: "OPEN"},
		Workspace:       molecule.Workspace{SourceRoot: "/Users/someone/src/r", Path: filepath.Join(lab, "workspaces", "issue-350"), Branch: "forgecell/issue-350", BaseCommit: strings.Repeat("0", 40), Repo: "o/r"},
		Readiness:       &readiness.State{SchemaVersion: "v1", Phase: "review-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{Revision: 1}, Digest: findingsTestPlan}}},
		FormulaApproved: true,
		FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hex.EncodeToString(sum[:])},
	}
}

// findingsTestPending makes the latest plan the receipt's next fix round on
// head, verified, and needing confirmation when confirm is set.
func findingsTestPending(r *molecule.Record, round int, head, headTree string, verified, confirm bool, fps ...string) {
	ref := &readiness.FixRoundRef{Round: round, Head: head, HeadTree: headTree}
	for _, fp := range fps {
		ref.Findings = append(ref.Findings, readiness.FixRoundFinding{Fingerprint: fp, Reviewer: "0:critic", Severity: "high", Title: "t"})
	}
	r.Readiness.Plans = append(r.Readiness.Plans, readiness.Proposal{Plan: readiness.Plan{Revision: 2, ParentDigest: findingsTestPlan, FixRound: ref}, Digest: findingsTestFixPlan})
	if !verified {
		return
	}
	r.Readiness.Events = append(r.Readiness.Events, readiness.Event{Kind: readiness.VerificationReviewWaiting, Digest: findingsTestFixPlan, At: "2026-11-01T12:00:00Z"})
	r.Verification = &verification.Result{RequiredChecksPassed: true, SourceTree: findingsTestVerTr, IndependentAcceptanceVerified: !confirm}
	r.VerificationAttempts = []molecule.VerificationAttempt{{PlanDigest: findingsTestFixPlan, SourceTree: findingsTestVerTr, Status: "passed"}}
	if !confirm {
		r.Atoms = append(r.Atoms, molecule.Atom{ID: "review", Type: "review", Status: "completed"})
		r.ReviewAttempts = []molecule.ReviewAttempt{{PlanDigest: findingsTestFixPlan, Tree: findingsTestVerTr, Status: molecule.ReviewNoHighFindings}}
	}
}

// findingsTestReceipt writes a published receipt with the given fix rounds.
func findingsTestReceipt(t *testing.T, lab, id string, rounds ...delivery.FixRound) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"state": "published", "title": "t", "acceptanceReviewAcknowledged": false, "preview": map[string]any{"moleculeId": id, "tree": findingsTestTree, "repo": "o/r", "branch": "forgecell/issue-350", "baseBranch": "main"}, "commit": findingsTestCommit, "url": "https://github.com/o/r/pull/5", "fixRounds": rounds})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// findingsTestFixRound is the receipt's first fix round, addressing fps.
func findingsTestFixRound(fps ...string) delivery.FixRound {
	return delivery.FixRound{Round: 1, Parent: findingsTestCommit, Commit: findingsTestFixed, Tree: findingsTestFixTr, PlanDigest: strings.Repeat("e3", 32), VerificationTree: findingsTestFixTr, Findings: fps, PushedAt: "2026-11-01T12:30:00Z", ObservedHead: findingsTestFixed}
}

// findingsTestFinding is one recorded finding of reviewer 0:critic.
func findingsTestFinding(name, severity, path, title string) delivery.Finding {
	return delivery.Finding{Fingerprint: findingsTestFP(name), Severity: severity, Path: path, Line: 3, Title: title, PluginID: name}
}

// findingsTestRound is one complete round on commit with reviewer 0:critic.
func findingsTestRound(commit, status string, complete bool, findings ...delivery.Finding) delivery.FindingsRound {
	if findings == nil {
		findings = []delivery.Finding{}
	}
	return delivery.FindingsRound{Commit: commit, Ref: "5", CollectedAt: "2026-11-01T11:30:00Z", Complete: complete, Reviewers: []delivery.ReviewerRound{{Reviewer: "0:critic", Plugin: "critic", Status: status, Findings: findings}}}
}

// findingsTestWrite writes the findings record.
func findingsTestWrite(t *testing.T, lab, id string, dismissed []delivery.Dismissal, rounds ...delivery.FindingsRound) {
	t.Helper()
	if rounds == nil {
		rounds = []delivery.FindingsRound{}
	}
	raw, err := json.Marshal(delivery.FindingsRecord{SchemaVersion: "v1", MoleculeID: id, Rounds: rounds, Dismissed: dismissed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", id+".findings.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// findingsFake replaces the fixround seams and counts every call; no plugin,
// git or forge process starts.
type findingsFake struct {
	mu                                   sync.Mutex
	collects, proposes, previews, pushes []string
	pushArgs                             []fixRoundPushRequest
	collectErr, proposeErr, previewErr   error
	pushErr                              error
	proposal                             fixround.Proposal
	preview                              delivery.FixRoundPreview
}

func (f *findingsFake) counts() (collects, proposes, previews, pushes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.collects), len(f.proposes), len(f.previews), len(f.pushes)
}

// findingsTestHandler serves records (by id) through the records seam, with
// fakes for every fixround call. Dismissals use the real engine, which only
// writes the findings record in the test's Lab.
func findingsTestHandler(t *testing.T, lab, source string, records ...molecule.Record) (http.Handler, *findingsFake) {
	t.Helper()
	h := testHandler(t, Options{LabDir: lab, SourceRoot: source})
	hh := h.(*handler)
	byID := map[string]molecule.Record{}
	for _, r := range records {
		byID[r.ID] = r
		writeRecord(t, lab, r)
	}
	var mu sync.Mutex
	hh.records = func(_, id string) (molecule.Record, error) {
		mu.Lock()
		defer mu.Unlock()
		r, ok := byID[id]
		if !ok {
			return molecule.Record{}, os.ErrNotExist
		}
		return r, nil
	}
	f := &findingsFake{preview: delivery.FixRoundPreview{MoleculeID: findingsTestID, Branch: "forgecell/issue-350", Parent: findingsTestCommit, Tree: findingsTestVerTr, Files: []string{"src/a.go"}, Round: 1, Title: "fix: address reviewer findings, round 1 (#350)", AcceptanceUnverified: true, Digest: findingsTestDigest}}
	hh.findingsCollect = func(ctx context.Context, _, id string, _ formula.Formula) (delivery.FindingsRecord, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.collects = append(f.collects, id)
		return delivery.FindingsRecord{}, f.collectErr
	}
	hh.fixRoundPropose = func(_ context.Context, _, id string, _ []string) (fixround.Proposal, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.proposes = append(f.proposes, id)
		return f.proposal, f.proposeErr
	}
	hh.fixRoundPreview = func(_ context.Context, _, id string) (delivery.FixRoundPreview, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.previews = append(f.previews, id)
		return f.preview, f.previewErr
	}
	hh.fixRoundPush = func(_ context.Context, _, id, approve string, confirm bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pushes = append(f.pushes, id)
		f.pushArgs = append(f.pushArgs, fixRoundPushRequest{Approve: approve, Confirm: confirm})
		return f.pushErr
	}
	return h, f
}

func findingsGetView(t *testing.T, h http.Handler, id string) (ReviewerFindingsView, string) {
	t.Helper()
	w := do(t, h, authed("/api/molecules/"+id+"/findings"))
	var v ReviewerFindingsView
	decode(t, w, &v)
	noLeak(t, w.Body.String(), findingsTestSecret)
	return v, w.Body.String()
}

// findingsRefusalOf checks a refusal's status, category and a non-empty
// bounded reason, with nothing local or secret in the body.
func findingsRefusalOf(t *testing.T, w *httptest.ResponseRecorder, code int, category string) gateRefusal {
	t.Helper()
	var v gateRefusal
	if w.Code != code || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Category != category || v.Error == "" || !boundedReason(v.Reason) {
		t.Fatalf("status %d, want %d %s: %s", w.Code, code, category, w.Body)
	}
	noLeak(t, w.Body.String(), findingsTestSecret)
	return v
}

func TestFindingsViewWithoutReviewersServesReviewersFalse(t *testing.T) {
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, false)
	findingsTestReceipt(t, lab, r.ID)
	h, fake := findingsTestHandler(t, lab, "/src", r)
	w := do(t, h, authed("/api/molecules/"+r.ID+"/findings"))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"reviewers":false}` {
		t.Fatalf("no reviewers: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, authed("/api/molecules/mol-none/findings")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown Molecule: %d", w.Code)
	}
	if w := do(t, h, authed("/api/molecules/bad..id/findings")); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d", w.Code)
	}
	if c, p, v, u := fake.counts(); c+p+v+u != 0 {
		t.Fatalf("a read called the engine: %d %d %d %d", c, p, v, u)
	}
}

func TestFindingsViewServesOpenResolvedDismissedRedacted(t *testing.T) {
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, r.ID)
	hostile := findingsTestFinding("a", "high", "src/a.go", "Nil deref in "+r.Workspace.Path+"/src/a.go with "+findingsTestSecret+" <script>")
	low := findingsTestFinding("b", "low", "docs/x.md", "Typo")
	dismissed := findingsTestFinding("c", "medium", "src/c.go", "Not a bug")
	gone := findingsTestFinding("old", "medium", "src/old.go", "Fixed already")
	earlier := findingsTestRound(strings.Repeat("6", 40), delivery.FindingsComplete, true, gone)
	latest := findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, hostile, low, dismissed)
	latest.Reviewers[0].Error = "secret " + findingsTestSecret + " at /Users/someone/x"
	findingsTestWrite(t, lab, r.ID, []delivery.Dismissal{{Fingerprint: dismissed.Fingerprint, Reviewer: "0:critic", Reason: "not a real issue " + findingsTestSecret, At: "2026-11-01T11:40:00Z"}}, earlier, latest)
	h, fake := findingsTestHandler(t, lab, "/src", r)
	v, body := findingsGetView(t, h, r.ID)
	if !v.Reviewers || !v.Published || v.Head != findingsTestCommit || !v.OnHead || v.Round != 2 || v.Commit != findingsTestCommit {
		t.Fatalf("view header %+v", v)
	}
	if len(v.Rounds) != 1 || v.Rounds[0].Reviewer != "0:critic" || v.Rounds[0].Commit != findingsTestCommit || !v.Rounds[0].Complete || v.Rounds[0].Status != delivery.FindingsComplete || v.Rounds[0].CollectedAt != "2026-11-01T11:30:00Z" {
		t.Fatalf("rounds %+v", v.Rounds)
	}
	if len(v.Open) != 2 || v.OpenCount != 2 || v.Open[0].Fingerprint != hostile.Fingerprint || !v.Open[0].AutoFix || v.Open[0].Path != "src/a.go" || v.Open[0].Line != 3 || v.Open[1].Fingerprint != low.Fingerprint || v.Open[1].AutoFix || v.Open[0].Repeated {
		t.Fatalf("open %+v", v.Open)
	}
	if v.Resolved != 1 || v.Dismissed != 1 || len(v.Dismissals) != 1 || v.Dismissals[0].Fingerprint != dismissed.Fingerprint || !strings.HasPrefix(v.Dismissals[0].Reason, "not a real issue") {
		t.Fatalf("resolved and dismissed %+v", v)
	}
	if strings.Contains(v.Open[0].Title, r.Workspace.Path) || strings.Contains(body, lab) || strings.Contains(body, "<script>") {
		t.Fatalf("served unredacted text: %s", body)
	}
	if v.Pending != nil || len(v.FixRounds) != 0 {
		t.Fatalf("no fix round: %+v %+v", v.Pending, v.FixRounds)
	}
	if c, p, pv, u := fake.counts(); c+p+pv+u != 0 {
		t.Fatalf("a read called the engine: %d %d %d %d", c, p, pv, u)
	}
	// An unreadable findings record is a fixed text, never its error.
	if err := os.WriteFile(filepath.Join(lab, "deliveries", r.ID+".findings.json"), []byte(`{"schemaVersion":"v1","moleculeId":"`+lab+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	v, _ = findingsGetView(t, h, r.ID)
	if !v.Unreadable || v.Error != findingsUnreadableTxt || len(v.Open) != 0 {
		t.Fatalf("unreadable record %+v", v)
	}
}

func TestFindingsViewServesRepeatedFindingsAndThePendingFixRound(t *testing.T) {
	lab := t.TempDir()
	back := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	other := findingsTestFinding("b", "high", "src/b.go", "Race")
	findingsTestReceipt(t, lab, findingsTestID, findingsTestFixRound(back.Fingerprint))
	findingsTestWrite(t, lab, findingsTestID, nil, findingsTestRound(findingsTestFixed, delivery.FindingsComplete, true, back, other))

	// Waiting for approval: no preview is read.
	waiting := findingsTestRecord(lab, findingsTestID, true)
	findingsTestPending(&waiting, 2, findingsTestFixed, findingsTestFixTr, false, true, other.Fingerprint)
	waiting.Readiness.Phase = "scope-waiting"
	h, fake := findingsTestHandler(t, lab, "/src", waiting)
	v, _ := findingsGetView(t, h, waiting.ID)
	if len(v.FixRounds) != 1 || v.FixRounds[0].Round != 1 || v.FixRounds[0].Commit != findingsTestFixed || v.FixRounds[0].PushedAt != "2026-11-01T12:30:00Z" {
		t.Fatalf("fix rounds %+v", v.FixRounds)
	}
	if len(v.Open) != 2 || !v.Open[0].Repeated || v.Open[1].Repeated {
		t.Fatalf("repeated %+v", v.Open)
	}
	if v.Pending == nil || v.Pending.State != fixRoundWaitingApproval || v.Pending.Round != 2 || v.Pending.Findings != 1 || v.Pending.Preview != nil {
		t.Fatalf("pending waiting %+v", v.Pending)
	}
	if _, _, previews, _ := fake.counts(); previews != 0 {
		t.Fatal("a waiting fix round read a preview")
	}

	// Running: coding or checks, still no preview.
	running := findingsTestRecord(lab, findingsTestID, true)
	findingsTestPending(&running, 2, findingsTestFixed, findingsTestFixTr, false, true, other.Fingerprint)
	running.Readiness.Phase = "coding"
	h, _ = findingsTestHandler(t, lab, "/src", running)
	if v, _ := findingsGetView(t, h, running.ID); v.Pending == nil || v.Pending.State != fixRoundRunning || v.Pending.Preview != nil {
		t.Fatalf("pending running %+v", v.Pending)
	}

	// Verified: the preview is served, with its confirmation.
	verified := findingsTestRecord(lab, findingsTestID, true)
	findingsTestPending(&verified, 2, findingsTestFixed, findingsTestFixTr, true, true, other.Fingerprint)
	h, fake = findingsTestHandler(t, lab, "/src", verified)
	fake.preview.Round = 2
	v, _ = findingsGetView(t, h, verified.ID)
	p := v.Pending
	if p == nil || p.State != fixRoundReady || p.Preview == nil || p.Preview.Digest != findingsTestDigest || p.Preview.Round != 2 || !p.Preview.AcceptanceUnverified || p.Preview.ReviewGreen || !p.Preview.NeedsConfirm || len(p.Preview.Files) != 1 || p.Preview.Files[0] != "src/a.go" {
		t.Fatalf("pending verified %+v", p)
	}
	// A refused preview is a fixed text.
	fake.previewErr = errors.New("git failed in " + lab)
	v, body := findingsGetView(t, h, verified.ID)
	if v.Pending == nil || v.Pending.State != fixRoundVerified || v.Pending.Preview != nil || v.Pending.PreviewError != fixRoundPreviewText || strings.Contains(body, lab) {
		t.Fatalf("refused preview %+v %s", v.Pending, body)
	}
	if _, _, _, pushes := fake.counts(); pushes != 0 {
		t.Fatal("a read pushed")
	}

	// A fix round proposed on another head is not pending.
	stale := findingsTestRecord(lab, findingsTestID, true)
	findingsTestPending(&stale, 2, findingsTestCommit, findingsTestTree, true, true, other.Fingerprint)
	h, _ = findingsTestHandler(t, lab, "/src", stale)
	if v, _ := findingsGetView(t, h, stale.ID); v.Pending != nil {
		t.Fatalf("a fix round on another head is pending: %+v", v.Pending)
	}
}

// findingsTestLab is one published Molecule with open findings on its head
// and a verified fix round of finding b that needs confirmation.
func findingsTestLab(t *testing.T, source string) (string, molecule.Record, http.Handler, *findingsFake) {
	t.Helper()
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, r.ID)
	a, b := findingsTestFinding("a", "high", "src/a.go", "Nil deref"), findingsTestFinding("b", "low", "docs/x.md", "Typo")
	findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a, b))
	findingsTestPending(&r, 1, findingsTestCommit, findingsTestTree, true, true, b.Fingerprint)
	h, fake := findingsTestHandler(t, lab, source, r)
	return lab, r, h, fake
}

func TestFindingsWritesRefuseWithoutTheProtectionOrASourceCheckout(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	bodies := map[string]string{
		"findings/collect":   `{}`,
		"findings/propose":   `{"include":[]}`,
		"findings/dismiss":   `{"fingerprint":"` + findingsTestFP("a") + `","reason":"noise"}`,
		"findings/undismiss": `{"fingerprint":"` + findingsTestFP("a") + `"}`,
		"fixround/push":      `{"approve":"` + findingsTestDigest + `","confirm":true}`,
	}
	for action, body := range bodies {
		path := "/api/molecules/" + r.ID + "/" + action
		for name, forged := range forgedWrites(path, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", name, action, w.Code, w.Body)
			}
		}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req := gatePost(path, body)
			req.method = method
			if w := do(t, h, req); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s: %d", method, action, w.Code)
			}
		}
		// An unknown field and trailing data are refused before the ledger is read.
		unknown := strings.TrimSuffix(body, "}")
		if unknown != "{" {
			unknown += ","
		}
		refusalOf(t, do(t, h, gatePost(path, unknown+`"extra":1}`)), http.StatusBadRequest, "invalid")
		refusalOf(t, do(t, h, gatePost(path, body+`{}`)), http.StatusBadRequest, "invalid")
		refusalOf(t, do(t, h, gatePost(path, strings.Repeat(" ", maxGateBody+1)+body)), http.StatusBadRequest, "invalid")
		refusalOf(t, do(t, h, gatePost("/api/molecules/bad..id/"+action, body)), http.StatusBadRequest, "invalid")
	}
	// A Lab UI without a source checkout writes nothing.
	_, ro, roh, rofake := findingsTestLab(t, "")
	for action, body := range bodies {
		findingsRefusalOf(t, do(t, roh, gatePost("/api/molecules/"+ro.ID+"/"+action, body)), http.StatusConflict, "read-only")
	}
	if c, p, v, u := rofake.counts(); c+p+v+u != 0 {
		t.Fatalf("a read-only Lab called the engine: %d %d %d %d", c, p, v, u)
	}
	if c, p, v, u := fake.counts(); c+p+v+u != 0 {
		t.Fatalf("a refused write called the engine: %d %d %d %d", c, p, v, u)
	}
}

func TestFindingsWritesRefuseInvalidValues(t *testing.T) {
	lab, r, h, fake := findingsTestLab(t, "/src")
	path := func(action string) string { return "/api/molecules/" + r.ID + "/" + action }
	many := make([]string, fixround.MaxIncludes+1)
	for i := range many {
		many[i] = `"` + findingsTestFP(string(rune('a'+i%26))+strings.Repeat("x", i)) + `"`
	}
	for name, c := range map[string]struct{ action, body string }{
		"too many includes":       {"findings/propose", `{"include":[` + strings.Join(many, ",") + `]}`},
		"bad include":             {"findings/propose", `{"include":["ABC"]}`},
		"bad dismiss fingerprint": {"findings/dismiss", `{"fingerprint":"` + strings.Repeat("G", 64) + `","reason":"noise"}`},
		"empty reason":            {"findings/dismiss", `{"fingerprint":"` + findingsTestFP("a") + `","reason":"  "}`},
		"no reason":               {"findings/dismiss", `{"fingerprint":"` + findingsTestFP("a") + `"}`},
		"control in reason":       {"findings/dismiss", `{"fingerprint":"` + findingsTestFP("a") + `","reason":"a\u0007b"}`},
		"long reason":             {"findings/dismiss", `{"fingerprint":"` + findingsTestFP("a") + `","reason":"` + strings.Repeat("r", delivery.MaxDismissalReasonRunes+1) + `"}`},
		"bad undismiss":           {"findings/undismiss", `{"fingerprint":""}`},
		"bad digest":              {"fixround/push", `{"approve":"abc","confirm":true}`},
	} {
		w := do(t, h, gatePost(path(c.action), c.body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
			continue
		}
		refusalOf(t, w, http.StatusBadRequest, "invalid")
	}
	// A stale digest and a missing confirmation are refused before any push.
	stale := findingsRefusalOf(t, do(t, h, gatePost(path("fixround/push"), `{"approve":"`+strings.Repeat("0", 64)+`","confirm":true}`)), http.StatusConflict, "stale")
	if stale.Error != fixRoundStaleText {
		t.Fatalf("stale text %q", stale.Error)
	}
	findingsRefusalOf(t, do(t, h, gatePost(path("fixround/push"), `{"approve":"`+findingsTestDigest+`"}`)), http.StatusConflict, "confirm")
	findingsRefusalOf(t, do(t, h, gatePost(path("fixround/push"), `{"approve":"`+findingsTestDigest+`","confirm":false}`)), http.StatusConflict, "confirm")
	if _, _, previews, pushes := fake.counts(); pushes != 0 || previews != 3 {
		t.Fatalf("refused pushes: %d previews, %d pushes", previews, pushes)
	}
	// Without reviewers or a published receipt nothing is written.
	none := findingsTestRecord(lab, "mol-351-def456", false)
	unpublished := findingsTestRecord(lab, "mol-352-fed654", true)
	h, fake = findingsTestHandler(t, lab, "/src", none, unpublished)
	findingsTestReceipt(t, lab, none.ID)
	findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+none.ID+"/findings/collect", `{}`)), http.StatusConflict, "no-reviewers")
	findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+unpublished.ID+"/findings/collect", `{}`)), http.StatusConflict, "not-published")
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-9-aaaaaa/findings/collect", `{}`)), http.StatusNotFound, "missing")
	if c, p, v, u := fake.counts(); c+p+v+u != 0 {
		t.Fatalf("a refused write called the engine: %d %d %d %d", c, p, v, u)
	}
}

func TestFindingsCollectStartsOneJob(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/collect", `{}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("collect: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, findingsJobKey(r.ID))
	if collects, _, _, pushes := fake.counts(); collects != 1 || pushes != 0 {
		t.Fatalf("collect: %d collections, %d pushes", collects, pushes)
	}
	// A failed collection is a fixed text on the view.
	fake.collectErr = errors.New("plugin said " + findingsTestSecret + " in /Users/someone")
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/collect", `{}`)); w.Code != http.StatusAccepted {
		t.Fatalf("second collect: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, findingsJobKey(r.ID))
	v, _ := findingsGetView(t, h, r.ID)
	if v.Collect == nil || v.Collect.Kind != "findings" || v.Collect.State != "refused" || v.Collect.Error == "" {
		t.Fatalf("failed collection %+v", v.Collect)
	}
	// A running push makes the collection busy.
	block := make(chan struct{})
	hh := h.(*handler)
	if _, err := hh.jobs.start(fixRoundJobKey(r.ID), "fixround", "", func(ctx context.Context) error { <-block; return nil }); err != nil {
		t.Fatal(err)
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/collect", `{}`)), http.StatusConflict, "busy")
	close(block)
	waitJob(t, h, fixRoundJobKey(r.ID))
}

func TestFindingsProposeServesTheProposalSummary(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	fake.proposal = fixround.Proposal{Round: 1, Picked: []delivery.SummaryFinding{{Plugin: "critic", Reviewer: "0:critic", Finding: a, AutoFix: true}}, Skipped: []fixround.Skip{{Fingerprint: findingsTestFP("b"), Reason: fixround.SkipRepeated}, {Fingerprint: findingsTestFP("c"), Reason: fixround.SkipDismissed}}}
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":["`+findingsTestFP("a")+`"]}`))
	var p FindingsProposalView
	decode(t, w, &p)
	if p.Round != 1 || p.None || len(p.Picked) != 1 || p.Picked[0].Fingerprint != a.Fingerprint || len(p.Skipped) != 2 || p.Skipped[0].Reason != "came back after a fix" || p.Skipped[1].Reason != "dismissed" {
		t.Fatalf("proposal %+v", p)
	}
	fake.proposal = fixround.Proposal{Round: 1, None: true}
	w = do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":[]}`))
	decode(t, w, &p)
	if !p.None || len(p.Picked) != 0 {
		t.Fatalf("no proposal %+v", p)
	}
	// An engine refusal is a fixed text: its error never reaches the page.
	fake.proposeErr = errors.New("collect findings for the current head first: forgecell findings " + r.ID + " --collect in /Users/someone " + findingsTestSecret)
	findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{}`)), http.StatusConflict, "not-collected")
	fake.proposeErr = errors.New("something unexpected in /Users/someone")
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{}`)), http.StatusUnprocessableEntity, "refused")
	if _, proposes, _, pushes := fake.counts(); proposes != 4 || pushes != 0 {
		t.Fatalf("propose: %d proposals, %d pushes", proposes, pushes)
	}
}

func TestFindingsDismissAndUndismissThroughTheEngine(t *testing.T) {
	lab, r, h, _ := findingsTestLab(t, "/src")
	fp := findingsTestFP("a")
	path := "/api/molecules/" + r.ID + "/findings/"
	var v FindingsDismissView
	decode(t, do(t, h, gatePost(path+"dismiss", `{"fingerprint":"`+fp+`","reason":"a false positive"}`)), &v)
	if v.Fingerprint != fp || !v.Dismissed {
		t.Fatalf("dismiss %+v", v)
	}
	record, err := delivery.ReadFindings(lab, r.ID)
	if err != nil || len(record.Dismissed) != 1 || record.Dismissed[0].Reason != "a false positive" {
		t.Fatalf("record %+v %v", record.Dismissed, err)
	}
	view, _ := findingsGetView(t, h, r.ID)
	if len(view.Open) != 1 || view.Dismissed != 1 || len(view.Dismissals) != 1 {
		t.Fatalf("view after dismissal %+v", view)
	}
	findingsRefusalOf(t, do(t, h, gatePost(path+"dismiss", `{"fingerprint":"`+fp+`","reason":"again"}`)), http.StatusConflict, "dismissed")
	findingsRefusalOf(t, do(t, h, gatePost(path+"dismiss", `{"fingerprint":"`+findingsTestFP("zzz")+`","reason":"unknown"}`)), http.StatusConflict, "unknown-finding")
	decode(t, do(t, h, gatePost(path+"undismiss", `{"fingerprint":"`+fp+`"}`)), &v)
	if v.Dismissed {
		t.Fatalf("undismiss %+v", v)
	}
	findingsRefusalOf(t, do(t, h, gatePost(path+"undismiss", `{"fingerprint":"`+fp+`"}`)), http.StatusConflict, "not-dismissed")
	// The delivery lock refuses a dismissal while another delivery operation runs.
	if err := os.Mkdir(filepath.Join(lab, ".delivery-lock"), 0700); err != nil {
		t.Fatal(err)
	}
	findingsRefusalOf(t, do(t, h, gatePost(path+"dismiss", `{"fingerprint":"`+fp+`","reason":"locked"}`)), http.StatusConflict, "locked")
}

func TestFixRoundPushRunsOnlyFromAnExplicitPost(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	// Reads, listings and the sync pass never push.
	findingsGetView(t, h, r.ID)
	for _, path := range []string{"/api/molecules", "/api/sync", "/api/molecules/" + r.ID} {
		if w := do(t, h, authed(path)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	findingsSyncWait(h.(*handler))
	if _, _, _, pushes := fake.counts(); pushes != 0 {
		t.Fatalf("a read pushed %d times", pushes)
	}
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/fixround/push", `{"approve":"`+findingsTestDigest+`","confirm":true}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, fixRoundJobKey(r.ID))
	fake.mu.Lock()
	args := fake.pushArgs
	fake.mu.Unlock()
	if len(args) != 1 || args[0].Approve != findingsTestDigest || !args[0].Confirm {
		t.Fatalf("push args %+v", args)
	}
	// A push the engine refuses ends refused, with a fixed text only.
	fake.pushErr = errors.New("approval is stale: the fix round push changed; review the new preview " + findingsTestSecret)
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/fixround/push", `{"approve":"`+findingsTestDigest+`","confirm":true}`)); w.Code != http.StatusAccepted {
		t.Fatalf("second push: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, fixRoundJobKey(r.ID))
	v, _ := findingsGetView(t, h, r.ID)
	if v.Push == nil || v.Push.Kind != "fixround" || v.Push.State != "refused" || v.Push.Category != "stale" {
		t.Fatalf("refused push %+v", v.Push)
	}
	// A fix round that is not verified, or a latest plan that is no fix round,
	// is never previewed or pushed.
	lab := t.TempDir()
	unverified := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, unverified.ID)
	findingsTestPending(&unverified, 1, findingsTestCommit, findingsTestTree, false, true)
	h, fake = findingsTestHandler(t, lab, "/src", unverified)
	findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+unverified.ID+"/fixround/push", `{"approve":"`+findingsTestDigest+`","confirm":true}`)), http.StatusConflict, "not-verified")
	plain := findingsTestRecord(lab, findingsTestID, true)
	h, fake2 := findingsTestHandler(t, lab, "/src", plain)
	findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+plain.ID+"/fixround/push", `{"approve":"`+findingsTestDigest+`","confirm":true}`)), http.StatusConflict, "no-fix-round")
	for _, f := range []*findingsFake{fake, fake2} {
		if _, _, previews, pushes := f.counts(); previews+pushes != 0 {
			t.Fatalf("a refused push called the engine: %d previews, %d pushes", previews, pushes)
		}
	}
}

func TestFixRoundPreviewServesDisplayNamesUnderTheRawDigest(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	fake.preview.Files = []string{"src/a.go\nsrc/evil‮gp.go", "docs/" + findingsTestSecret + ".md", strings.Repeat("p", 3*findingsMaxPathBytes)}
	fake.preview.Branch = "forgecell/issue-350\r\n‮x"
	v, body := findingsGetView(t, h, r.ID)
	p := v.Pending
	if p == nil || p.Preview == nil || p.Preview.Digest != findingsTestDigest || len(p.Preview.Files) != 3 {
		t.Fatalf("preview %+v", p)
	}
	for _, name := range append([]string{p.Preview.Branch}, p.Preview.Files...) {
		if strings.ContainsAny(name, "\n\r‮") || len([]rune(name)) > findingsMaxPathBytes {
			t.Fatalf("unsafe served name %q", name)
		}
	}
	if p.Preview.Files[0] != "src/a.go src/evil gp.go" || p.Preview.Branch != "forgecell/issue-350 x" || strings.ContainsRune(body, '‮') {
		t.Fatalf("served names %q %q", p.Preview.Files, p.Preview.Branch)
	}
	// The digest shown still pushes: it binds the engine's raw values.
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/fixround/push", `{"approve":"`+p.Preview.Digest+`","confirm":true}`)); w.Code != http.StatusAccepted {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, fixRoundJobKey(r.ID))
}

func TestFindingsProposeServesALineOnlyWithItsPath(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	noPath := findingsTestFinding("a", "high", "", "No path")
	badLine := findingsTestFinding("b", "high", "src/b.go", "Bad line")
	badLine.Line = -4
	hostilePath := findingsTestFinding("c", "high", "src/c.go\n", "Hostile path")
	good := findingsTestFinding("d", "high", "src/d.go", "Good")
	for _, f := range []delivery.Finding{noPath, badLine, hostilePath, good} {
		fake.proposal.Picked = append(fake.proposal.Picked, delivery.SummaryFinding{Plugin: "critic", Reviewer: "0:critic", Finding: f, AutoFix: true})
	}
	fake.proposal.Round = 1
	var p FindingsProposalView
	decode(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":[]}`)), &p)
	if len(p.Picked) != 4 {
		t.Fatalf("picked %+v", p.Picked)
	}
	for i, want := range []struct {
		path string
		line int64
	}{{"", 0}, {"src/b.go", 0}, {"", 0}, {"src/d.go", 3}} {
		if got := p.Picked[i]; got.Path != want.path || got.Line != want.line {
			t.Errorf("picked %d: %q:%d, want %q:%d", i, got.Path, got.Line, want.path, want.line)
		}
	}
}

// findingsTestLifecycle records the change request's observed state on the
// Molecule's receipt.
func findingsTestLifecycle(t *testing.T, lab, id, state string) {
	t.Helper()
	path := filepath.Join(lab, "deliveries", id+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observed := strings.Replace(string(raw), `"state":"published"`, `"state":"published","lifecycle":{"state":"`+state+`","observedAt":"2026-11-02T10:00:00Z"}`, 1)
	if err := os.WriteFile(path, []byte(observed), 0600); err != nil {
		t.Fatal(err)
	}
}

// Item 3 of #386: each open finding serves its body, redacted, control-free
// with its lines kept, and bounded; the snapshot's reviewer plugins are named.
func TestFindingsViewServesFindingBodies(t *testing.T) {
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, r.ID)
	explained := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	explained.Body = "The pointer is nil with " + findingsTestSecret + ".\r\n\n\n\x1b[31mSee " + r.Workspace.Path + "/src/a.go\x07 too.\nLast line."
	long := findingsTestFinding("b", "medium", "src/b.go", "Long")
	long.Body = strings.Repeat("word ", 2000)
	bare := findingsTestFinding("c", "low", "src/c.go", "No body")
	findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, explained, long, bare))
	h, _ := findingsTestHandler(t, lab, "/src", r)
	v, body := findingsGetView(t, h, r.ID)
	if len(v.Open) != 3 || len(v.Plugins) != 1 || v.Plugins[0] != "critic" || v.Lifecycle != "" {
		t.Fatalf("view %+v", v)
	}
	got := v.Open[0]
	if got.BodyTruncated || !strings.HasPrefix(got.Body, "The pointer is nil with ") || !strings.HasSuffix(got.Body, "\nLast line.") || strings.Count(got.Body, "\n") != 3 || strings.Contains(got.Body, "\n\n\n") {
		t.Fatalf("body %q", got.Body)
	}
	if strings.ContainsAny(got.Body, "\r\x1b\x07") || strings.Contains(got.Body, findingsTestSecret) || strings.Contains(body, r.Workspace.Path) {
		t.Fatalf("body served unredacted: %q", got.Body)
	}
	if n := len([]rune(v.Open[1].Body)); !v.Open[1].BodyTruncated || n == 0 || n > findingsMaxBodyRunes {
		t.Fatalf("long body: %d runes, truncated %v", n, v.Open[1].BodyTruncated)
	}
	if v.Open[2].Body != "" || v.Open[2].BodyTruncated || strings.Count(body, `"body":`) != 2 {
		t.Fatalf("an empty body was served: %+v", v.Open[2])
	}
}

// Item 5 of #386: the engine's "this Lab's Formula declares no reviewers" is
// its own refusal, whose next step names the Formula mismatch.
func TestFindingsProposeNoActiveReviewersReason(t *testing.T) {
	_, r, h, fake := findingsTestLab(t, "/src")
	fake.proposeErr = delivery.ErrNoReviewers
	got := findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":[]}`)), http.StatusConflict, "no-active-reviewers")
	if got.Error != "this Lab's Formula declares no reviewers; nothing was proposed" || got.Reason != findingsReasons["no-active-reviewers"] {
		t.Fatalf("refusal %+v", got)
	}
	if !strings.Contains(got.Reason, "This Molecule's Formula snapshot declares reviewers, but the Lab's active approved Formula does not") {
		t.Fatalf("reason %q", got.Reason)
	}
}

// Item 4 of #386: once the change request is merged or closed, the view says
// so and a proposal is refused without calling the engine.
func TestFindingsProposeRefusedAfterMergeOrClose(t *testing.T) {
	for state, want := range map[string]string{"MERGED": "merged", "CLOSED": "closed"} {
		t.Run(state, func(t *testing.T) {
			lab, r, h, fake := findingsTestLab(t, "/src")
			findingsTestLifecycle(t, lab, r.ID, state)
			if v, _ := findingsGetView(t, h, r.ID); v.Lifecycle != want || !v.Published {
				t.Fatalf("view lifecycle %q, want %q", v.Lifecycle, want)
			}
			got := findingsRefusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":[]}`)), http.StatusConflict, "not-live")
			if got.Reason != findingsReasons["not-live"] {
				t.Fatalf("refusal %+v", got)
			}
			if _, proposes, _, pushes := fake.counts(); proposes != 0 || pushes != 0 {
				t.Fatalf("a refused proposal called the engine: %d proposals, %d pushes", proposes, pushes)
			}
		})
	}
	// An open change request still proposes.
	lab, r, h, fake := findingsTestLab(t, "/src")
	findingsTestLifecycle(t, lab, r.ID, "OPEN")
	fake.proposal = fixround.Proposal{Round: 1, None: true}
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/findings/propose", `{"include":[]}`)); w.Code != http.StatusOK {
		t.Fatalf("open change request: %d %s", w.Code, w.Body)
	}
}

func TestFindingsSampleDataServesNoFindings(t *testing.T) {
	h := testHandler(t, Options{Demo: true})
	if w := do(t, h, authed("/api/molecules/"+findingsTestID+"/findings")); w.Code != http.StatusNotFound {
		t.Fatalf("sample findings: %d", w.Code)
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+findingsTestID+"/findings/collect", `{}`)), http.StatusNotFound, "sample")
}
