package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

var (
	headFirst = strings.Repeat("a", 40)
	headTree  = strings.Repeat("7", 40)
	fixTree   = strings.Repeat("8", 40)
)

func hexCommit(c string) string { return strings.Repeat(c, 40) }

// headRound is a valid fix round on parent pushing commit, bound to vtree.
func headRound(round int, parent, commit, vtree string) FixRound {
	return FixRound{Round: round, Parent: parent, Commit: commit, Tree: strings.Repeat("e", 40), PlanDigest: strings.Repeat("d", 64), VerificationTree: vtree, Findings: []string{strings.Repeat("f", 64), strings.Repeat("0", 64)}, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: commit}
}

func writeHeadLedger(t *testing.T, o Options, record molecule.Record) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(o.LabDir, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// headFixture is a GitHub delivery published at headFirst with its ledger.
func headFixture(t *testing.T) (Options, Receipt, molecule.Record) {
	t.Helper()
	o := Options{LabDir: t.TempDir(), MoleculeID: "mol-head"}
	p := PreviewResult{MoleculeID: o.MoleculeID, Repo: "owner/repo", Tree: headTree}
	p.Digest = digest(p)
	r := Receipt{Preview: p, State: "published", Commit: headFirst, URL: "https://github.com/owner/repo/pull/9"}
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	record := molecule.Record{ID: o.MoleculeID, Issue: molecule.Issue{Repo: p.Repo}, Verification: &verification.Result{SourceTree: headTree}}
	writeHeadLedger(t, o, record)
	return o, r, record
}

func receiptBytes(t *testing.T, o Options) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func headSnapshot(commit, status, at string) checks.Snapshot {
	s := checks.Snapshot{Repo: "owner/repo", PR: 9, Commit: commit, ObservedHead: commit, CheckedAt: at, Required: []string{"test"}, Status: status, Checks: []checks.Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://github.com/owner/repo/actions/runs/1"}}}
	if status == "failed" {
		s.Checks[0].Conclusion = "FAILURE"
	}
	return s
}

func TestReceiptHeadFollowsTheLastFixRound(t *testing.T) {
	r := Receipt{Commit: headFirst, Preview: PreviewResult{Tree: headTree}}
	if c, tree := ReceiptHead(r); c != headFirst || tree != headTree {
		t.Fatalf("no rounds: %s %s", c, tree)
	}
	r.FixRounds = []FixRound{headRound(1, headFirst, hexCommit("b"), fixTree)}
	r.FixRounds[0].Tree = hexCommit("1")
	if c, tree := ReceiptHead(r); c != hexCommit("b") || tree != hexCommit("1") {
		t.Fatalf("one round: %s %s", c, tree)
	}
	r.FixRounds = append(r.FixRounds, headRound(2, hexCommit("b"), hexCommit("c"), fixTree), headRound(3, hexCommit("c"), hexCommit("d"), fixTree))
	r.FixRounds[2].Tree = hexCommit("3")
	if c, tree := ReceiptHead(r); c != hexCommit("d") || tree != hexCommit("3") {
		t.Fatalf("three rounds: %s %s", c, tree)
	}
	if r.Commit != headFirst {
		t.Fatal("the first published commit moved")
	}
}

func TestAppendFixRoundAppendsOnTheCurrentHead(t *testing.T) {
	o, _, _ := headFixture(t)
	r, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := ReceiptHead(r); c != hexCommit("b") || r.Commit != headFirst || len(r.FixRounds) != 1 {
		t.Fatalf("round 1: %+v", r)
	}
	if _, err := AppendFixRound(o, headRound(2, headFirst, hexCommit("c"), fixTree)); err == nil {
		t.Fatal("round 2 on the first commit, not the head, was admitted")
	}
	if _, err := AppendFixRound(o, headRound(2, hexCommit("b"), hexCommit("c"), fixTree)); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendFixRound(o, headRound(3, hexCommit("c"), hexCommit("d"), fixTree)); err != nil {
		t.Fatal(err)
	}
	before := receiptBytes(t, o)
	if _, err := AppendFixRound(o, headRound(4, hexCommit("d"), hexCommit("1"), fixTree)); err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("a fourth round: %v", err)
	}
	if receiptBytes(t, o) != before {
		t.Fatal("a refused round rewrote the receipt")
	}
	var stored Receipt
	if err := json.Unmarshal([]byte(before), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.FixRounds) != 3 || stored.Commit != headFirst || stored.FixRounds[2].Commit != hexCommit("d") || len(stored.FixRounds[0].Findings) != 2 {
		t.Fatalf("stored receipt: %+v", stored)
	}
	s, found, err := ReadReceiptStatus(o.LabDir, o.MoleculeID)
	if err != nil || !found || s != (ReceiptStatus{State: "published", URL: "https://github.com/owner/repo/pull/9", MoleculeID: o.MoleculeID, Commit: headFirst, Head: hexCommit("d")}) {
		t.Fatalf("status %+v %t %v", s, found, err)
	}
	if _, err := os.Stat(filepath.Join(o.LabDir, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatal("the delivery lock was left behind")
	}
}

func TestAppendFixRoundRefusesAnInvalidRound(t *testing.T) {
	o, _, _ := headFixture(t)
	many := []string{}
	for i := 0; i <= MaxFixRoundFindings; i++ {
		many = append(many, fmt.Sprintf("%064x", i))
	}
	for name, mutate := range map[string]func(*FixRound){
		"round zero":         func(fr *FixRound) { fr.Round = 0 },
		"round two first":    func(fr *FixRound) { fr.Round = 2 },
		"wrong parent":       func(fr *FixRound) { fr.Parent = hexCommit("c") },
		"observed head":      func(fr *FixRound) { fr.ObservedHead = hexCommit("c") },
		"upper-case commit":  func(fr *FixRound) { fr.Commit, fr.ObservedHead = strings.Repeat("B", 40), strings.Repeat("B", 40) },
		"short commit":       func(fr *FixRound) { fr.Commit, fr.ObservedHead = hexCommit("b")[:39], hexCommit("b")[:39] },
		"long tree":          func(fr *FixRound) { fr.Tree = strings.Repeat("e", 64) },
		"bad verification":   func(fr *FixRound) { fr.VerificationTree = "" },
		"short digest":       func(fr *FixRound) { fr.PlanDigest = fr.PlanDigest[:63] },
		"non-hex digest":     func(fr *FixRound) { fr.PlanDigest = strings.Repeat("g", 64) },
		"duplicate findings": func(fr *FixRound) { fr.Findings = []string{fr.Findings[0], fr.Findings[0]} },
		"bad finding":        func(fr *FixRound) { fr.Findings = []string{"../x"} },
		"too many findings":  func(fr *FixRound) { fr.Findings = many },
		"bad push time":      func(fr *FixRound) { fr.PushedAt = "yesterday" },
		"same commit":        func(fr *FixRound) { fr.Commit, fr.ObservedHead = headFirst, headFirst },
	} {
		fr := headRound(1, headFirst, hexCommit("b"), fixTree)
		mutate(&fr)
		before := receiptBytes(t, o)
		if _, err := AppendFixRound(o, fr); err == nil {
			t.Errorf("%s: admitted", name)
		}
		if receiptBytes(t, o) != before {
			t.Fatalf("%s: receipt rewritten", name)
		}
	}
	if err := os.Mkdir(filepath.Join(o.LabDir, ".delivery-lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err == nil {
		t.Fatal("a held delivery lock was ignored")
	}
	os.Remove(filepath.Join(o.LabDir, ".delivery-lock"))
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err != nil {
		t.Fatalf("a valid round after refusals: %v", err)
	}
	if _, err := AppendFixRound(Options{LabDir: o.LabDir, MoleculeID: "../x"}, headRound(1, headFirst, hexCommit("b"), fixTree)); err == nil {
		t.Fatal("an unsafe id was admitted")
	}
	if _, err := AppendFixRound(Options{LabDir: o.LabDir, MoleculeID: "absent"}, headRound(1, headFirst, hexCommit("b"), fixTree)); err == nil {
		t.Fatal("a missing receipt was admitted")
	}
}

func TestAppendFixRoundRefusesAnUnpublishedOrSymlinkedReceipt(t *testing.T) {
	o, r, _ := headFixture(t)
	r.State = "committed"
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	before := receiptBytes(t, o)
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err == nil {
		t.Fatal("an unpublished receipt took a fix round")
	}
	if receiptBytes(t, o) != before {
		t.Fatal("an unpublished receipt was rewritten")
	}

	o, _, _ = headFixture(t)
	file := filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json")
	elsewhere := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.Rename(file, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, file); err != nil {
		t.Fatal(err)
	}
	target, _ := os.ReadFile(elsewhere)
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err == nil {
		t.Fatal("a symlinked receipt took a fix round")
	}
	if after, _ := os.ReadFile(elsewhere); string(after) != string(target) {
		t.Fatal("the symlink target was rewritten")
	}
	if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlinked receipt was replaced")
	}
}

func TestAppendFixRoundRefusesTamperedHistory(t *testing.T) {
	o, r, _ := headFixture(t)
	r.FixRounds = []FixRound{headRound(1, hexCommit("c"), hexCommit("b"), fixTree)}
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendFixRound(o, headRound(2, hexCommit("b"), hexCommit("d"), fixTree)); err == nil {
		t.Fatal("a round was appended to a broken history")
	}
}

func TestPublishRefusesAReceiptWithFixRounds(t *testing.T) {
	o, r, _ := headFixture(t)
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err != nil {
		t.Fatal(err)
	}
	before := receiptBytes(t, o)
	_, err := Publish(context.Background(), PublishOptions{Options: o, Approve: r.Preview.Digest, Title: "t"})
	if err == nil || err.Error() != "this change request was already published and has fix rounds" {
		t.Fatalf("Publish: %v", err)
	}
	if receiptBytes(t, o) != before {
		t.Fatal("Publish rewrote the receipt")
	}
	if _, err := os.Stat(filepath.Join(o.LabDir, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatal("the delivery lock was left behind")
	}
}

func TestPublishedEvidenceFollowsTheFixRoundVerificationTree(t *testing.T) {
	_, r, record := headFixture(t)
	if !MatchesPublishedEvidence(r, record) {
		t.Fatal("the published receipt does not match")
	}
	r.FixRounds = []FixRound{headRound(1, headFirst, hexCommit("b"), fixTree)}
	if MatchesPublishedEvidence(r, record) {
		t.Fatal("the first commit's tree still matched after a fix round")
	}
	moved := record
	moved.Verification = &verification.Result{SourceTree: fixTree}
	if !MatchesPublishedEvidence(r, moved) {
		t.Fatal("the fix round's verification tree does not match")
	}
	broken := r
	broken.FixRounds = []FixRound{headRound(1, hexCommit("c"), hexCommit("b"), fixTree)}
	if MatchesPublishedEvidence(broken, moved) {
		t.Fatal("a fix round on the wrong parent matched")
	}

	fx, pr := publishedPluginFixture(t)
	precord, err := molecule.ReadRecord(fx.o.LabDir, fx.o.MoleculeID)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesPluginPublishedEvidence(pr, precord) {
		t.Fatal("the published plugin receipt does not match")
	}
	pr.FixRounds = []FixRound{headRound(1, pr.Commit, hexCommit("b"), fixTree)}
	if MatchesPluginPublishedEvidence(pr, precord) {
		t.Fatal("the plugin receipt's first tree still matched after a fix round")
	}
	pmoved := precord
	v := *precord.Verification
	v.SourceTree = fixTree
	pmoved.Verification = &v
	if !MatchesPluginPublishedEvidence(pr, pmoved) {
		t.Fatal("the plugin fix round's verification tree does not match")
	}
	pr.FixRounds[0].ObservedHead = hexCommit("c")
	if MatchesPluginPublishedEvidence(pr, pmoved) {
		t.Fatal("a plugin fix round with a moved observed head matched")
	}
}

func TestCIFollowsTheHead(t *testing.T) {
	o, _, record := headFixture(t)
	if _, err := AppendCI(o, headSnapshot(headFirst, "passed", "2026-11-02T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err != nil {
		t.Fatal(err)
	}
	if _, err := ciIdentity(o); err == nil {
		t.Fatal("CI identity bound the first commit's verified tree after a fix round")
	}
	record.Verification = &verification.Result{SourceTree: fixTree}
	writeHeadLedger(t, o, record)
	id, err := ciIdentity(o)
	if err != nil || id.Commit != hexCommit("b") || id.PR != 9 {
		t.Fatalf("identity %+v %v", id, err)
	}
	if err := ValidateCITarget(o, "owner/repo", 9, headFirst); err == nil {
		t.Fatal("CI target on the first commit was admitted after a fix round")
	}
	if err := ValidateCITarget(o, "owner/repo", 9, hexCommit("b")); err != nil {
		t.Fatal(err)
	}
	h, err := ReadCI(o)
	if err != nil || len(h) != 1 || h[0].Identity.Commit != headFirst {
		t.Fatalf("older observations unreadable: %+v %v", h, err)
	}
	if CIClassification(h) != "passed" || CIClassification(HeadCI(h, hexCommit("b"))) != "unknown" {
		t.Fatal("the first commit's pass classified the new head")
	}
	if _, err := AppendCI(o, headSnapshot(headFirst, "passed", "2026-11-02T11:00:00Z")); err == nil {
		t.Fatal("CI for the first commit was appended after a fix round")
	}
	if _, err := AppendCI(o, headSnapshot(hexCommit("b"), "failed", "2026-11-02T11:00:00Z")); err != nil {
		t.Fatal(err)
	}
	h, err = ReadCI(o)
	if err != nil || len(h) != 2 || h[1].Identity.Commit != hexCommit("b") {
		t.Fatalf("history %+v %v", h, err)
	}
	if got := CIClassification(HeadCI(h, hexCommit("b"))); got != "failure" {
		t.Fatalf("head classification %s", got)
	}
	if len(HeadCI(h, headFirst)) != 1 {
		t.Fatal("HeadCI dropped the first commit's observation")
	}

	// A receipt whose recorded fix round no longer names the observed commit
	// cannot keep that observation.
	raw := receiptBytes(t, o)
	var r Receipt
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	r.FixRounds[0].Commit, r.FixRounds[0].ObservedHead = hexCommit("c"), hexCommit("c")
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCI(o); err == nil {
		t.Fatal("an observation for an unrecorded commit was admitted")
	}
}

// writeCILink writes one hash-linked CI observation as appendCI would.
func writeCILink(t *testing.T, o Options, a CIObservation) CIObservation {
	t.Helper()
	a.SHA256 = ciHash(a)
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ciDir(o, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d-%s.json", a.Sequence, a.SHA256)), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestReadCIRefusesAnEarlierCommitAfterTheHead(t *testing.T) {
	o, _, record := headFixture(t)
	if _, err := AppendFixRound(o, headRound(1, headFirst, hexCommit("b"), fixTree)); err != nil {
		t.Fatal(err)
	}
	record.Verification = &verification.Result{SourceTree: fixTree}
	writeHeadLedger(t, o, record)
	id, err := ciIdentity(o)
	if err != nil {
		t.Fatal(err)
	}
	first := writeCILink(t, o, CIObservation{SchemaVersion: "v1", Sequence: 1, Identity: id, Snapshot: headSnapshot(hexCommit("b"), "passed", "2026-11-02T10:00:00Z")})
	old := id
	old.Commit = headFirst
	writeCILink(t, o, CIObservation{SchemaVersion: "v1", Sequence: 2, Previous: first.SHA256, Identity: old, Snapshot: headSnapshot(headFirst, "passed", "2026-11-02T11:00:00Z")})
	if _, err := ReadCI(o); err == nil {
		t.Fatal("an earlier commit's observation after the head's was admitted")
	}
}

func TestPluginCIFollowsTheHead(t *testing.T) {
	fx, r := publishedPluginFixture(t)
	record, err := molecule.ReadRecord(fx.o.LabDir, fx.o.MoleculeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePluginCITarget(fx.o, pluginTestProject, r.Ref, r.Commit); err != nil {
		t.Fatal(err)
	}
	head := hexCommit("b")
	if r.Commit == head {
		head = hexCommit("c")
	}
	if _, err := AppendFixRound(fx.o, headRound(1, r.Commit, head, record.Verification.SourceTree)); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePluginCITarget(fx.o, pluginTestProject, r.Ref, r.Commit); err == nil {
		t.Fatal("plugin CI on the first commit was admitted after a fix round")
	}
	if err := ValidatePluginCITarget(fx.o, pluginTestProject, r.Ref, head); err != nil {
		t.Fatal(err)
	}
	if id, err := ciIdentity(fx.o); err != nil || id.Commit != head || id.Ref != r.Ref {
		t.Fatalf("plugin identity %+v %v", id, err)
	}
}

func TestReceiptStatusHead(t *testing.T) {
	lab := t.TempDir()
	round := func(n int, parent, commit string) string {
		raw, _ := json.Marshal(headRound(n, parent, commit, fixTree))
		return string(raw)
	}
	r1, r2, r3, r4 := round(1, headFirst, hexCommit("b")), round(2, hexCommit("b"), hexCommit("c")), round(3, hexCommit("c"), hexCommit("d")), round(4, hexCommit("d"), hexCommit("1"))
	receipt := func(state, rounds string) string {
		return `{"state":"` + state + `","commit":"` + headFirst + `","url":"https://github.com/o/r/pull/5","preview":{"moleculeId":"m1"}` + rounds + `}`
	}
	for name, rounds := range map[string]string{"absent": "", "null": `,"fixRounds":null`, "empty": `,"fixRounds":[]`} {
		writeTestReceipt(t, lab, "m1", receipt("published", rounds))
		s, _, err := ReadReceiptStatus(lab, "m1")
		if err != nil || s != (ReceiptStatus{State: "published", URL: "https://github.com/o/r/pull/5", MoleculeID: "m1", Commit: headFirst}) {
			t.Fatalf("%s: %+v %v", name, s, err)
		}
	}
	writeTestReceipt(t, lab, "m1", receipt("published", `,"fixRounds":[`+r1+`,`+r2+`,`+r3+`]`))
	s, _, err := ReadReceiptStatus(lab, "m1")
	if err != nil || s.Commit != headFirst || s.Head != hexCommit("d") {
		t.Fatalf("three rounds: %+v %v", s, err)
	}
	thousand := strings.TrimSuffix(strings.Repeat(r1+",", 1000), ",")
	for name, raw := range map[string]string{
		"four rounds":     receipt("published", `,"fixRounds":[`+r1+`,`+r2+`,`+r3+`,`+r4+`]`),
		"many rounds":     receipt("published", `,"fixRounds":[`+thousand+`]`),
		"not a list":      receipt("published", `,"fixRounds":{"round":1}`),
		"not an object":   receipt("published", `,"fixRounds":["round"]`),
		"string round":    receipt("published", `,"fixRounds":[`+strings.Replace(r1, `"round":1`, `"round":"1"`, 1)+`]`),
		"bad hex":         receipt("published", `,"fixRounds":[`+strings.ReplaceAll(r1, hexCommit("b"), strings.Repeat("B", 40))+`]`),
		"wrong parent":    receipt("published", `,"fixRounds":[`+r2+`]`),
		"skipped round":   receipt("published", `,"fixRounds":[`+r1+`,`+r3+`]`),
		"unpublished":     receipt("committed", `,"fixRounds":[`+r1+`]`),
		"repeated rounds": receipt("published", `,"fixRounds":[`+r1+`],"fixRounds":[]`),
		"truncated":       receipt("published", `,"fixRounds":[`+r1),
	} {
		writeTestReceipt(t, lab, "m1", raw)
		if s, found, err := ReadReceiptStatus(lab, "m1"); err == nil || !found || s != (ReceiptStatus{}) {
			t.Errorf("%s: accepted %+v (%t)", name, s, found)
		}
	}
}

func TestReadPublishedReceiptFollowsTheHead(t *testing.T) {
	o, r, _ := headFixture(t)
	r.FixRounds = []FixRound{headRound(1, headFirst, hexCommit("b"), fixTree)}
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	got, err := readPublishedReceipt(o.LabDir, o.MoleculeID)
	if c, _ := ReceiptHead(got); err != nil || c != hexCommit("b") {
		t.Fatalf("head %s %v", c, err)
	}
	r.FixRounds[0].Round = 2
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublishedReceipt(o.LabDir, o.MoleculeID); err == nil {
		t.Fatal("findings read a receipt with a broken fix-round history")
	}
}

func TestReceiptWithoutFixRoundsIsUnchanged(t *testing.T) {
	o, r, record := headFixture(t)
	raw := receiptBytes(t, o)
	if strings.Contains(raw, "fixRounds") {
		t.Fatalf("a receipt without fix rounds writes them: %s", raw)
	}
	empty := r
	empty.FixRounds = []FixRound{}
	a, _ := json.MarshalIndent(r, "", "  ")
	b, _ := json.MarshalIndent(empty, "", "  ")
	if string(a) != string(b) || string(a)+"\n" != raw {
		t.Fatal("an empty fix-round list changes the receipt bytes")
	}
	var back Receipt
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatal(err)
	}
	if err := writeReceipt(o, back); err != nil {
		t.Fatal(err)
	}
	if receiptBytes(t, o) != raw {
		t.Fatal("a round trip changed the receipt bytes")
	}
	if c, tree := ReceiptHead(back); c != r.Commit || tree != r.Preview.Tree {
		t.Fatalf("head %s %s", c, tree)
	}
	if !MatchesPublishedEvidence(back, record) {
		t.Fatal("the receipt no longer matches")
	}
	s, _, err := ReadReceiptStatus(o.LabDir, o.MoleculeID)
	if err != nil || s != (ReceiptStatus{State: "published", URL: r.URL, MoleculeID: o.MoleculeID, Commit: r.Commit}) {
		t.Fatalf("status %+v %v", s, err)
	}
	if id, err := ciIdentity(o); err != nil || id.Commit != r.Commit {
		t.Fatalf("identity %+v %v", id, err)
	}
	if err := ValidateCITarget(o, "owner/repo", 9, r.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendCI(o, headSnapshot(r.Commit, "passed", "2026-11-02T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	h, err := ReadCI(o)
	if err != nil || len(HeadCI(h, r.Commit)) != len(h) || CIClassification(HeadCI(h, r.Commit)) != CIClassification(h) {
		t.Fatalf("history %+v %v", h, err)
	}
	if receiptBytes(t, o) != raw {
		t.Fatal("CI rewrote the receipt")
	}
}
