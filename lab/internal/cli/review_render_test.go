package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

func reviewRecord(findings []harness.ReviewFinding, drafts []harness.ReviewFollowUp, same bool) molecule.Record {
	return molecule.Record{ID: "mol-review", ReviewAttempts: []molecule.ReviewAttempt{{
		HarnessAttemptID: "attempt-1", Binding: "claude-code", BindingSource: "review-role", SameCommandAsCoding: same,
		Status: molecule.ReviewBlocked, Reason: "1 high finding(s) block delivery.", High: 1, Medium: 1, Findings: findings, FollowUps: drafts,
	}}}
}

func TestReviewAtomGateRendersBoundedAndEscapedFindings(t *testing.T) {
	esc := "\x1b"
	hostile := harness.ReviewFinding{Severity: "high", Status: "confirmed", File: "a" + esc + "[2Jb.go", Line: 7,
		Claim:    "claim " + esc + "[31mred" + esc + "[0m " + esc + "]0;title\x07 " + strings.Repeat("c", 2000),
		Scenario: "line one\nline two\r\x00 hidden",
		Fix:      "fix\x1b[1A\x1b[2K overwritten"}
	findings := []harness.ReviewFinding{hostile}
	for i := range 14 {
		findings = append(findings, harness.ReviewFinding{Severity: "medium", Status: "plausible", File: fmt.Sprintf("f%d.go", i), Line: i, Claim: "claim", Scenario: "scenario", Fix: "fix"})
	}
	var drafts []harness.ReviewFollowUp
	for i := range 8 {
		drafts = append(drafts, harness.ReviewFollowUp{Title: fmt.Sprintf("[review medium] draft %d %s[31m", i, esc), Body: "body"})
	}
	var out bytes.Buffer
	presentReview(&out, reviewRecord(findings, drafts, false))
	shown := out.String()
	for _, r := range shown {
		if r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x202e) {
			t.Fatalf("a control character reached the gate: %q in\n%q", r, shown)
		}
	}
	for _, leaked := range []string{"[2J", "[31m", "]0;title", "overwritten\x1b", "[1A"} {
		if strings.Contains(shown, leaked) {
			t.Fatalf("escape sequence %q reached the gate:\n%s", leaked, shown)
		}
	}
	for _, want := range []string{"evidence only, never an approval", "1 high", "claude-code (review-role)", "ab.go:7", "Scenario: line one line two hidden", "… 5 more finding(s) are in the ledger", "… 3 more draft follow-up(s) are in the ledger", "Draft follow-up (not filed)"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("gate lacks %q:\n%s", want, shown)
		}
	}
	for _, line := range strings.Split(shown, "\n") {
		if n := len([]rune(line)); n > 3*presentedFieldRunes {
			t.Fatalf("an unbounded line of %d runes reached the gate", n)
		}
	}
	if strings.Contains(shown, "same command") {
		t.Fatal("the same-command notice appeared without the condition")
	}
}

func TestReviewAtomGateShowsSameCommandNoticeAndFailureReason(t *testing.T) {
	var out bytes.Buffer
	r := reviewRecord(nil, nil, true)
	r.ReviewAttempts[0].Status = molecule.ReviewFailed
	r.ReviewAttempts[0].Reason = "invalid review output: findings must be an array"
	r.ReviewAttempts[0].High = 0
	presentRun(&out, r)
	shown := out.String()
	for _, want := range []string{"Adversarial review: failed", "NOTICE: the review binding runs the same command as the coding binding", "Reason: invalid review output: findings must be an array", "Ledger:"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("gate lacks %q:\n%s", want, shown)
		}
	}
}

func TestReviewAtomGateShowsEveryEarlierPassForTheRetainedTree(t *testing.T) {
	r := molecule.Record{ID: "mol-review", ReviewAttempts: []molecule.ReviewAttempt{
		{HarnessAttemptID: "attempt-1", Tree: "tree-a", Status: molecule.ReviewBlocked, High: 2},
		{HarnessAttemptID: "attempt-1/continuation-1", Tree: "tree-a", Status: molecule.ReviewFailed},
		{HarnessAttemptID: "attempt-2", Tree: "tree-b", Status: molecule.ReviewNoHighFindings, Medium: 1},
		{HarnessAttemptID: "attempt-2/continuation-2", Tree: "tree-b", Status: molecule.ReviewNoHighFindings},
	}}
	var out bytes.Buffer
	presentReview(&out, r)
	shown := out.String()
	for _, want := range []string{
		"Earlier review pass 1 of 4: blocked · 2 high · 0 medium · 0 low · on a different tree",
		"Earlier review pass 2 of 4: failed · 0 high · 0 medium · 0 low · on a different tree",
		"Earlier review pass 3 of 4: no-high-findings · 0 high · 1 medium · 0 low · on the same retained tree",
		"Adversarial review: no-high-findings",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("gate lacks %q:\n%s", want, shown)
		}
	}
	if strings.Index(shown, "Earlier review pass 1") > strings.Index(shown, "Adversarial review:") {
		t.Fatalf("earlier passes must come first:\n%s", shown)
	}
	var continued bytes.Buffer
	presentRun(&continued, molecule.Record{ID: "mol-continued", ReviewAttempts: []molecule.ReviewAttempt{
		{HarnessAttemptID: "attempt-1", Tree: "tree-a", Status: molecule.ReviewBlocked, High: 1},
		{HarnessAttemptID: "attempt-1/continuation-1", Tree: "tree-a", Status: molecule.ReviewNoHighFindings},
	}})
	if got := continued.String(); !strings.Contains(got, "Earlier review pass 1 of 2: blocked · 1 high · 0 medium · 0 low · on the same retained tree") || !strings.Contains(got, "Adversarial review: no-high-findings") {
		t.Fatalf("a clean continuation hides the blocked pass on the same tree:\n%s", got)
	}
	var single bytes.Buffer
	presentReview(&single, reviewRecord(nil, nil, false))
	if strings.Contains(single.String(), "Earlier review pass") {
		t.Fatalf("a single pass has no earlier pass:\n%s", single.String())
	}
}

func TestReviewAtomGateShowsRedactionNoticeAndCoverage(t *testing.T) {
	r := reviewRecord(nil, nil, false)
	r.ReviewAttempts[0].Redactions = 3
	r.ReviewAttempts[0].ChangedFiles, r.ReviewAttempts[0].ReviewedFiles = 4, 2
	var out bytes.Buffer
	presentReview(&out, r)
	shown := out.String()
	for _, want := range []string{"NOTICE: 3 secret-like value(s) were redacted from what the reviewer saw", "Coverage: the reviewer listed 2 file(s) as reviewed for 4 changed path(s)"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("gate lacks %q:\n%s", want, shown)
		}
	}
	var clean bytes.Buffer
	presentReview(&clean, reviewRecord(nil, nil, false))
	if strings.Contains(clean.String(), "redacted") || strings.Contains(clean.String(), "Coverage") {
		t.Fatalf("notices appeared without their condition:\n%s", clean.String())
	}
}

func TestReviewAtomGateIsSilentWithoutReview(t *testing.T) {
	var out bytes.Buffer
	presentRun(&out, molecule.Record{ID: "mol-plain"})
	if strings.Contains(out.String(), "Adversarial review") {
		t.Fatalf("a Molecule without a review shows one:\n%s", out.String())
	}
}
