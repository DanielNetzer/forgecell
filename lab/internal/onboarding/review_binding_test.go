package onboarding

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

const (
	reviewClaude = "[/l/forgecell, __adapter, claude-code, /bin/claude]"
	reviewCodex  = "[/l/forgecell, __adapter, codex, /bin/codex]"
)

func reviewFormula(t *testing.T, harnesses string, extraAtoms ...string) formula.Formula {
	t.Helper()
	atoms := "atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
	for _, a := range extraAtoms {
		atoms += "  - " + a + "\n"
	}
	f, err := formula.Parse([]byte("kind: formula\nid: review-binding\nharness:\n  binding: codex\n  command: " + reviewCodex + "\n  timeoutMs: 1000\n" + harnesses + atoms))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const (
	reviewAtom       = "{id: review, type: review}"
	learnWithCommand = "{id: learn, type: learn, binding: claude-code, command: " + reviewClaude + ", timeoutMs: 2000}"
	learnBare        = "{id: learn, type: learn}"
	reviewRole       = "harnesses:\n  review: {binding: claude-code, command: " + reviewClaude + ", timeoutMs: 3000}\n"
	metaRole         = "harnesses:\n  meta: {binding: claude-code, command: " + reviewClaude + "}\n"
)

func TestReviewAtomBindingResolutionOrder(t *testing.T) {
	cases := []struct {
		name      string
		harnesses string
		atoms     []string
		source    string
		binding   string
		timeout   int
		failure   string
	}{
		{"declared review role wins over the meta binding", reviewRole + "  meta: {binding: codex, command: " + reviewCodex + "}\n", []string{reviewAtom, learnBare}, "review-role", "claude-code", 3000, ""},
		{"review role needs no learn Atom", reviewRole, []string{reviewAtom}, "review-role", "claude-code", 3000, ""},
		{"meta role with a learn Atom", metaRole, []string{reviewAtom, learnBare}, "meta-override", "claude-code", 1000, ""},
		{"learn Atom command", "", []string{reviewAtom, learnWithCommand}, "learn-atom", "claude-code", 2000, ""},
		{"neither review nor meta binding", "", []string{reviewAtom}, "", "", 0, "configure harnesses.review"},
		{"default coding binding only is never used", "", []string{reviewAtom, learnBare}, "", "", 0, "never used to review its own change"},
		{"meta role without a learn Atom is not a meta binding", metaRole, []string{reviewAtom}, "", "", 0, "configure harnesses.review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := reviewFormula(t, c.harnesses, c.atoms...)
			res, configured, err := ResolveReviewBinding(f)
			if c.failure != "" {
				if err == nil || !strings.Contains(err.Error(), c.failure) || !configured {
					t.Fatalf("configured=%v err=%v, want failure containing %q", configured, err, c.failure)
				}
				if res.Binding.Binding == "codex" || len(res.Binding.Command) != 0 {
					t.Fatalf("the default coding binding was returned: %+v", res)
				}
				return
			}
			if err != nil || !configured || res.Source != c.source || res.Binding.Binding != c.binding || res.Binding.TimeoutMS != c.timeout || res.SameCommandAsCoding {
				t.Fatalf("%+v configured=%v err=%v", res, configured, err)
			}
		})
	}
}

func TestReviewAtomSameCommandAsCodingIsRecordedNotRefused(t *testing.T) {
	same := "harnesses:\n  review: {binding: codex, command: " + reviewCodex + "}\n"
	f := reviewFormula(t, same, reviewAtom)
	res, configured, err := ResolveReviewBinding(f)
	if err != nil || !configured || !res.SameCommandAsCoding || res.Source != "review-role" {
		t.Fatalf("%+v %v %v", res, configured, err)
	}
	if err = validateReviewBinding(f); err != nil {
		t.Fatalf("a shared command must be recorded, not refused: %v", err)
	}
	coding := "harnesses:\n  coding: {binding: claude-code, command: " + reviewClaude + "}\n  review: {binding: claude-code, command: " + reviewClaude + "}\n"
	if res, _, err = ResolveReviewBinding(reviewFormula(t, coding, reviewAtom)); err != nil || !res.SameCommandAsCoding {
		t.Fatalf("a coding role override with the same command must be detected: %+v %v", res, err)
	}
}

func TestReviewAtomProposalFailsClosedWithoutUsableBinding(t *testing.T) {
	custom := "harnesses:\n  review: {binding: custom, command: [/bin/sh, -c, review]}\n"
	cursor := "harnesses:\n  review: {binding: cursor, command: [/l/forgecell, __adapter, cursor, /bin/cursor]}\n"
	customLearn := "{id: learn, type: learn, binding: custom, command: [/bin/true]}"
	cases := []struct {
		name      string
		harnesses string
		atoms     []string
		want      string
	}{
		{"no binding at all", "", []string{reviewAtom}, "configure harnesses.review"},
		{"default coding binding only", "", []string{reviewAtom, learnBare}, "configure harnesses.review"},
		{"custom review role", custom, []string{reviewAtom}, "no verified read-only review protocol"},
		{"cursor review role", cursor, []string{reviewAtom}, "Cursor review is unavailable"},
		{"custom learn command", "", []string{reviewAtom, customLearn}, "no verified read-only review protocol"},
		{"unknown adapter id is not assumed supported", "harnesses:\n  review: {binding: mystery, command: [/l/forgecell, __adapter, mystery, /bin/x]}\n", []string{reviewAtom}, "(unknown)"},
		{"role without a review Atom", reviewRole, nil, "no review Atom"},
		{"review Atom with a command", reviewRole, []string{"{id: review, type: review, command: [/bin/x]}"}, "cannot declare a command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := reviewFormula(t, c.harnesses, c.atoms...)
			err := ValidateRoleBindings(f)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidateRoleBindings = %v, want %q", err, c.want)
			}
			dir := t.TempDir()
			if _, err = SaveProposal(dir, f.YAML); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("SaveProposal = %v, want %q", err, c.want)
			}
		})
	}
	ok := reviewFormula(t, "", reviewAtom, learnWithCommand)
	if err := ValidateRoleBindings(ok); err != nil {
		t.Fatalf("a learn Atom with a native command is a valid review binding: %v", err)
	}
}

func TestReviewAtomRoleEvidenceOnlyAppearsWithReview(t *testing.T) {
	plain := reviewFormula(t, "")
	for _, r := range RoleCapabilities(plain) {
		if r.Role == string(formula.ReviewHarness) {
			t.Fatalf("a Formula without a review Atom reports a review role: %+v", r)
		}
	}
	if res, configured, err := ResolveReviewBinding(plain); configured || err != nil || res.Binding.Binding != "" {
		t.Fatalf("%+v %v %v", res, configured, err)
	}
	f := reviewFormula(t, reviewRole, reviewAtom)
	roles := RoleCapabilities(f)
	last := roles[len(roles)-1]
	if last.Role != "review" || last.Source != "review-role" || last.Binding != "claude-code" || last.Capability.State != "supported" {
		t.Fatalf("%+v", roles)
	}
	if len(roles) != len(RoleCapabilities(plain))+1 {
		t.Fatalf("only the review entry may be added: %d vs %d", len(roles), len(RoleCapabilities(plain)))
	}
	if err := validateRoleEvidence(f, roles); err != nil {
		t.Fatal(err)
	}
	if err := validateRoleEvidence(f, roles[:len(roles)-1]); err == nil {
		t.Fatal("saved evidence without the review role must not match a Formula that has one")
	}
	unbound := reviewFormula(t, "", reviewAtom)
	for _, r := range RoleCapabilities(unbound) {
		if r.Role == "review" {
			t.Fatalf("an unresolvable review must not be reported as bound: %+v", r)
		}
	}
}
