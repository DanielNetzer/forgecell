package readiness

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixRoundFinding(c string) FixRoundFinding {
	return FixRoundFinding{Fingerprint: strings.Repeat(c, 64), Reviewer: "0:forgecomments", Severity: "medium", Path: "lab/parser.go", Line: 12, Title: "Trailing whitespace is accepted"}
}

func fixRoundFixture(round int) FixRoundRef {
	return FixRoundRef{Round: round, Head: strings.Repeat("1", 40), HeadTree: strings.Repeat("2", 40), Findings: []FixRoundFinding{fixRoundFinding("f")}, IssueBodySHA256: IssueBodySHA256("Reject trailing tokens")}
}

// successor is fixture as the amendment of parent, with fix round fr (or none).
func successor(parent Plan, parentDigest string, fr *FixRoundRef) Plan {
	p := fixture()
	p.Revision = parent.Revision + 1
	p.ParentDigest = parentDigest
	p.PausedTree = strings.Repeat("3", 40)
	p.FixRound = fr
	return p
}

func TestCaptureBaseFollowsTheFixRound(t *testing.T) {
	p := fixture()
	if got := p.CaptureBase(); got != p.Inputs.BaseCommit {
		t.Fatalf("CaptureBase without a fix round = %q, want the intake base", got)
	}
	fr := fixRoundFixture(1)
	p.FixRound = &fr
	if got := p.CaptureBase(); got != fr.Head {
		t.Fatalf("CaptureBase with a fix round = %q, want the delivered head", got)
	}
}

func TestPlanWithoutFixRoundKeepsBytesAndDigest(t *testing.T) {
	p := fixture()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fixRound") {
		t.Fatalf("a plan without a fix round marshals the field: %s", raw)
	}
	// The digest of the exact pre-change bytes: unmarshalling them gives a plan whose
	// digest is the SHA-256 of the same bytes.
	var round Plan
	if err = json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(round)
	if string(again) != string(raw) {
		t.Fatalf("round trip changed the bytes:\n%s\n%s", raw, again)
	}
	before, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	after, err := round.Digest()
	if err != nil || after != before {
		t.Fatalf("digest changed: %s %s %v", before, after, err)
	}
	// A fix round joins the digest.
	amended := successor(p, before, nil)
	plain, err := amended.Digest()
	if err != nil {
		t.Fatal(err)
	}
	fr := fixRoundFixture(1)
	amended.FixRound = &fr
	withRound, err := amended.Digest()
	if err != nil || withRound == plain {
		t.Fatalf("the fix round is not bound to the digest: %v", err)
	}
}

func TestFixRoundRefValidationRefusesMalformedRefs(t *testing.T) {
	for name, edit := range map[string]func(*FixRoundRef){
		"round 0":              func(f *FixRoundRef) { f.Round = 0 },
		"round 4":              func(f *FixRoundRef) { f.Round = 4 },
		"short head":           func(f *FixRoundRef) { f.Head = strings.Repeat("1", 39) },
		"uppercase head":       func(f *FixRoundRef) { f.Head = strings.Repeat("A", 40) },
		"option head":          func(f *FixRoundRef) { f.Head = "--output=" + strings.Repeat("1", 31) },
		"sha256 tree":          func(f *FixRoundRef) { f.HeadTree = strings.Repeat("2", 64) },
		"missing body hash":    func(f *FixRoundRef) { f.IssueBodySHA256 = "" },
		"bad fingerprint":      func(f *FixRoundRef) { f.Findings[0].Fingerprint = "abc" },
		"blank reviewer":       func(f *FixRoundRef) { f.Findings[0].Reviewer = " " },
		"unknown severity":     func(f *FixRoundRef) { f.Findings[0].Severity = "critical" },
		"traversing path":      func(f *FixRoundRef) { f.Findings[0].Path = "../etc/passwd" },
		"absolute path":        func(f *FixRoundRef) { f.Findings[0].Path = "/etc/passwd" },
		"bidi path":            func(f *FixRoundRef) { f.Findings[0].Path = "lab/‮og.resrap" },
		"format path":          func(f *FixRoundRef) { f.Findings[0].Path = "lab/par​ser.go" },
		"separator path":       func(f *FixRoundRef) { f.Findings[0].Path = "lab/par ser.go" },
		"negative line":        func(f *FixRoundRef) { f.Findings[0].Line = -1 },
		"control title":        func(f *FixRoundRef) { f.Findings[0].Title = "a\x1b[31mb" },
		"bidi title":           func(f *FixRoundRef) { f.Findings[0].Title = "a‮b" },
		"oversize title":       func(f *FixRoundRef) { f.Findings[0].Title = strings.Repeat("t", maxFindingTitleBytes+1) },
		"duplicate finding":    func(f *FixRoundRef) { f.Findings = append(f.Findings, f.Findings[0]) },
		"too many findings":    func(f *FixRoundRef) { f.Findings = make([]FixRoundFinding, MaxFixRoundFindings+1) },
		"invalid utf-8 title":  func(f *FixRoundRef) { f.Findings[0].Title = "\xff" },
		"invalid utf-8 review": func(f *FixRoundRef) { f.Findings[0].Reviewer = "\xff" },
	} {
		fr := fixRoundFixture(1)
		fr.Findings = append([]FixRoundFinding(nil), fr.Findings...)
		edit(&fr)
		if err := fr.Validate(); err == nil || !strings.Contains(err.Error(), "malformed fix round") {
			t.Errorf("%s: %v", name, err)
		}
		parent := fixture()
		d, _ := parent.Digest()
		p := successor(parent, d, &fr)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: the plan accepted a malformed fix round", name)
		}
	}
	// The bounds themselves are accepted.
	fr := fixRoundFixture(3)
	fr.Findings = nil
	for i := 0; i < MaxFixRoundFindings; i++ {
		f := fixRoundFinding("a")
		f.Fingerprint = IssueBodySHA256(strings.Repeat("x", i))
		fr.Findings = append(fr.Findings, f)
	}
	if err := fr.Validate(); err != nil {
		t.Fatalf("round 3 with %d findings: %v", MaxFixRoundFindings, err)
	}
}

func TestFixRoundIsNeverTheInitialPlan(t *testing.T) {
	p := fixture()
	fr := fixRoundFixture(1)
	p.FixRound = &fr
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "amendment of a delivered plan") {
		t.Fatalf("initial plan with a fix round: %v", err)
	}
}

func TestFindingCitationsNeedTheRoundsFindings(t *testing.T) {
	parent := fixture()
	d, _ := parent.Digest()
	fr := fixRoundFixture(1)
	p := successor(parent, d, &fr)
	cite := func(id string) *Plan {
		q := p
		q.Analysis.Acceptance = append(append([]Criterion(nil), p.Analysis.Acceptance...), Criterion{Description: "Address the finding", Evidence: []string{id}})
		return &q
	}
	if err := cite(FindingEvidenceID(fr.Findings[0].Fingerprint)).Validate(); err != nil {
		t.Fatalf("a finding of the round was refused: %v", err)
	}
	absent := FindingEvidenceID(strings.Repeat("0", 64))
	if err := cite(absent).Validate(); err == nil || !strings.Contains(err.Error(), "unknown or duplicate evidence citation: "+absent) {
		t.Fatalf("a finding absent from the round was accepted: %v", err)
	}
	// Without a fix round, no finding id is citable.
	plain := successor(parent, d, nil)
	plain.Analysis.Acceptance = append(plain.Analysis.Acceptance, Criterion{Description: "Address the finding", Evidence: []string{FindingEvidenceID(fr.Findings[0].Fingerprint)}})
	if err := plain.Validate(); err == nil {
		t.Fatal("a plan without a fix round accepted a finding citation")
	}
	// Only acceptance may cite a finding: not an impact or a scope path.
	impact := p
	impact.Analysis.Impacts = []Assessment{{Description: "Impact", Evidence: []string{FindingEvidenceID(fr.Findings[0].Fingerprint)}}}
	if err := impact.Validate(); err == nil {
		t.Fatal("an impact cited a finding")
	}
	scope := p
	scope.Analysis.Scope = []ScopedPath{{Path: "lab/parser.go", Reason: "Parser", Evidence: []string{FindingEvidenceID(fr.Findings[0].Fingerprint)}}}
	if err := scope.Validate(); err == nil {
		t.Fatal("a scope path cited a finding")
	}
}

// verifiedState is the history of fixture as revision 1, verified and waiting at
// the human review gate.
func verifiedState(t *testing.T, verified bool) (State, Plan, string) {
	t.Helper()
	p := fixture()
	s, err := NewState(p, "t0")
	if err != nil {
		t.Fatal(err)
	}
	d := s.Plans[0].Digest
	s.Phase = "review-waiting"
	if verified {
		s.Events = append(s.Events, Event{Kind: VerificationReviewWaiting, Digest: d, At: "t1"})
	}
	return s, p, d
}

func TestFixRoundChainRules(t *testing.T) {
	// Round 1 after a verified plan.
	s, p, d := verifiedState(t, true)
	fr1 := fixRoundFixture(1)
	s, err := s.Amend(successor(p, d, &fr1), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateFixRounds(); err != nil {
		t.Fatalf("round 1 after a verified plan: %v", err)
	}
	// A further amendment that keeps round 1 unchanged continues the round.
	p2, d2 := s.Plans[1].Plan, s.Plans[1].Digest
	s.Phase = "failed"
	carried, err := s.Amend(successor(p2, d2, &fr1), "t3")
	if err != nil {
		t.Fatal(err)
	}
	if err = carried.ValidateFixRounds(); err != nil {
		t.Fatalf("carried round 1: %v", err)
	}
	// Changing the round within it, dropping it, or skipping a round is refused.
	changed := fr1
	changed.Head = strings.Repeat("4", 40)
	for name, tc := range map[string]struct {
		fr   *FixRoundRef
		want string
	}{
		"changed": {&changed, "changes fix round 1"},
		"dropped": {nil, "drops fix round 1"},
		"skipped": {ptr(fixRoundFixture(3)), "must be round 2"},
	} {
		next, err := s.Amend(successor(p2, d2, tc.fr), "t3")
		if err != nil {
			t.Fatal(err)
		}
		if err = next.ValidateFixRounds(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Round 2 needs round 1's plan verified first.
	fr2 := fixRoundFixture(2)
	unverified, err := s.Amend(successor(p2, d2, &fr2), "t3")
	if err != nil {
		t.Fatal(err)
	}
	if err = unverified.ValidateFixRounds(); err == nil || !strings.Contains(err.Error(), "verified and delivered") {
		t.Fatalf("round 2 after an unverified round 1: %v", err)
	}
	s.Events = append(s.Events, Event{Kind: VerificationReviewWaiting, Digest: d2, At: "t3"})
	s.Phase = "review-waiting"
	second, err := s.Amend(successor(p2, d2, &fr2), "t4")
	if err != nil {
		t.Fatal(err)
	}
	if err = second.ValidateFixRounds(); err != nil {
		t.Fatalf("round 2 after a verified round 1: %v", err)
	}
}

func TestFixRoundChainRefusesWrongRoundAndUnverifiedParent(t *testing.T) {
	s, p, d := verifiedState(t, true)
	fr2 := fixRoundFixture(2)
	next, err := s.Amend(successor(p, d, &fr2), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if err = next.ValidateFixRounds(); err == nil || !strings.Contains(err.Error(), "must be round 1") {
		t.Fatalf("round 2 with no earlier round: %v", err)
	}
	s, p, d = verifiedState(t, false)
	fr1 := fixRoundFixture(1)
	next, err = s.Amend(successor(p, d, &fr1), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if err = next.ValidateFixRounds(); err == nil || !strings.Contains(err.Error(), "verified and delivered") {
		t.Fatalf("round 1 on an unverified plan: %v", err)
	}
	// A history without fix rounds is unaffected.
	plain, err := s.Amend(successor(p, d, nil), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if err = plain.ValidateFixRounds(); err != nil {
		t.Fatalf("history without fix rounds: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
