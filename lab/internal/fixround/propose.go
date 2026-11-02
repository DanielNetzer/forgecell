package fixround

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Bounds of a fix-round proposal.
const (
	// MaxIncludes bounds the included fingerprints of one proposal.
	MaxIncludes = 50
	// MaxProposed is the most findings one fix round answers: each adds one
	// acceptance item, and an amendment adds at most 64.
	MaxProposed = 64
	// maxAcceptanceRunes bounds one acceptance line.
	maxAcceptanceRunes = 1000
	// Bounds of a finding's fields as a fix round records them.
	maxFindingTitleBytes = 1000
	maxFindingPathBytes  = 1024
	maxFindingLine       = 1 << 31
)

// The reasons a finding is not proposed.
const (
	// SkipRepeated is a finding an earlier fix round addressed that is still
	// open.
	SkipRepeated = "came back after a fix"
	// SkipDismissed is a finding the operator dismissed.
	SkipDismissed = "dismissed"
)

// Fingerprint matches a finding fingerprint.
var Fingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Skip is an open finding the proposal left out, and why.
type Skip struct {
	Fingerprint string
	Reason      string
}

// Proposal is the outcome of Propose. Round is the fix round's number; Picked
// the findings it answers, in summary order; Skipped the open findings left
// out, in the same order. None is set when nothing was picked, so nothing was
// amended. Record is the amended Molecule record and Request the revision it
// was amended with.
type Proposal struct {
	Record  molecule.Record
	Request molecule.FixRoundRequest
	Round   int
	Picked  []delivery.SummaryFinding
	Skipped []Skip
	None    bool
}

// Propose proposes the next fix round of a published delivery from the latest
// complete findings round on its head: the open findings at or above each
// reviewer identity's autoFix level, and those named by includes. A finding an
// earlier fix round addressed that is still open is not proposed again unless
// included; a dismissed finding is never proposed, even when included. An
// include that is not an open finding of that round is refused. Skipped is
// returned with any refusal that comes after selection. Findings are untrusted
// evidence: their text reaches the revision only flattened and bounded, and
// nothing here approves or pushes.
func Propose(ctx context.Context, lab, id string, includes []string, opts ...Options) (Proposal, error) {
	o := options(opts)
	if len(includes) > MaxIncludes {
		return Proposal{}, fmt.Errorf("at most %d findings may be included", MaxIncludes)
	}
	for _, fp := range includes {
		if !Fingerprint.MatchString(fp) {
			return Proposal{}, fmt.Errorf("an included finding is a fingerprint: 64 lower-case hex characters")
		}
	}
	if err := delivery.RefuseDemoLab(lab); err != nil {
		return Proposal{}, err
	}
	receipt, err := delivery.ReadFixRoundReceipt(lab, id)
	if err != nil {
		return Proposal{}, err
	}
	head, tree := delivery.ReceiptHead(receipt)
	round := len(receipt.FixRounds) + 1
	if round > delivery.MaxFixRounds {
		return Proposal{}, fmt.Errorf("the change request already has %d fix rounds, so fix rounds stop", delivery.MaxFixRounds)
	}
	f, err := activeFormula(lab, o)
	if err != nil {
		return Proposal{}, err
	}
	record, err := delivery.ReadFindings(lab, id)
	if err != nil {
		return Proposal{}, err
	}
	at := -1
	for i := len(record.Rounds) - 1; i >= 0; i-- {
		if record.Rounds[i].Complete && record.Rounds[i].Commit == head {
			at = i
			break
		}
	}
	if at < 0 {
		return Proposal{}, fmt.Errorf("collect findings for the current head first: forgecell findings %s --collect", id)
	}
	autoFix := map[string]string{}
	for i, r := range f.Reviewers() {
		autoFix[delivery.ReviewerKey(i, r.Plugin)] = r.AutoFix
	}
	// Dismissals are applied here, so a dismissed finding is reported as
	// skipped rather than as not open.
	dismissed := record.DismissedSet()
	onHead := record
	onHead.Rounds = record.Rounds[:at+1]
	onHead.Dismissed = nil
	summary := delivery.Summarize(onHead, autoFix)
	if summary.Round != at {
		return Proposal{}, fmt.Errorf("collect findings for the current head first: forgecell findings %s --collect", id)
	}
	included := map[string]bool{}
	for _, fp := range includes {
		included[fp] = true
	}
	addressed := map[string]bool{}
	for _, fr := range receipt.FixRounds {
		for _, fp := range fr.Findings {
			addressed[fp] = true
		}
	}
	p := Proposal{Round: round}
	seen, found := map[string]bool{}, map[string]bool{}
	for _, g := range summary.Open {
		for _, sf := range g.Findings {
			fp := sf.Fingerprint
			if !Fingerprint.MatchString(fp) || seen[fp] {
				continue
			}
			seen[fp] = true
			found[fp] = true
			if dismissed[fp] {
				if sf.AutoFix || included[fp] {
					p.Skipped = append(p.Skipped, Skip{Fingerprint: fp, Reason: SkipDismissed})
				}
				continue
			}
			if addressed[fp] && !included[fp] {
				p.Skipped = append(p.Skipped, Skip{Fingerprint: fp, Reason: SkipRepeated})
				continue
			}
			if sf.AutoFix || included[fp] {
				p.Picked = append(p.Picked, sf)
			}
		}
	}
	for _, fp := range includes {
		if !found[fp] {
			return p, fmt.Errorf("finding %s is not an open finding of the latest complete findings round on the head", fp)
		}
	}
	if len(p.Picked) == 0 {
		p.None = true
		return p, nil
	}
	if len(p.Picked) > MaxProposed {
		return p, fmt.Errorf("%d findings are more than one fix round answers (%d); raise the reviewers' autoFix level or include fewer", len(p.Picked), MaxProposed)
	}
	rec, err := o.ReadRecord(lab, id)
	if err != nil {
		return p, fmt.Errorf("the Molecule ledger cannot be read")
	}
	if rec.Readiness == nil || len(rec.Readiness.Plans) == 0 {
		return p, fmt.Errorf("the Molecule has no readiness plan to amend")
	}
	parent := rec.Readiness.Plans[len(rec.Readiness.Plans)-1].Digest
	p.Request = Request(id, head, tree, round, parent, at, len(record.Rounds), p.Picked)
	amended, err := o.Amend(ctx, lab, rec, p.Request)
	if err != nil {
		return p, err
	}
	p.Record = amended
	return p, nil
}

// activeFormula is the caller's Formula, or the Lab's active approved one.
func activeFormula(lab string, o Options) (formula.Formula, error) {
	if o.Formula != nil {
		return *o.Formula, nil
	}
	loaded, err := formula.Load(lab, "")
	if err != nil || !loaded.Approved {
		return formula.Formula{}, fmt.Errorf("the Lab has no approved active Formula, so its reviewers are unknown")
	}
	if len(loaded.Formula.Reviewers()) == 0 {
		return formula.Formula{}, delivery.ErrNoReviewers
	}
	return loaded.Formula, nil
}

// Request builds the revision for the picked findings: one acceptance line and
// one scope path for each, and a note naming the reviewers, the findings round
// and the counts.
func Request(id, head, tree string, round int, parent string, at, rounds int, picked []delivery.SummaryFinding) molecule.FixRoundRequest {
	req := molecule.FixRoundRequest{ID: id, Head: head, HeadTree: tree, Round: round, ExpectParent: parent}
	scoped := map[string]bool{}
	perReviewer := map[string]int{}
	var reviewers []string
	for _, sf := range picked {
		finding := recordedFinding(sf)
		req.Findings = append(req.Findings, finding)
		location := "-"
		if finding.Path != "" {
			location = finding.Path
			if finding.Line > 0 {
				location += fmt.Sprintf(":%d", finding.Line)
			}
		}
		line := fmt.Sprintf("Fix the reviewer finding [%s] %s: %s (%s)", finding.Severity, location, finding.Title, readiness.FindingEvidenceID(finding.Fingerprint))
		req.Acceptance = append(req.Acceptance, readiness.Criterion{Description: fitRunes(safetext.Text(line, maxAcceptanceRunes), maxAcceptanceRunes, 4000), Evidence: []string{readiness.FindingEvidenceID(finding.Fingerprint)}})
		if finding.Path != "" && !scoped[finding.Path] {
			scoped[finding.Path] = true
			req.Scope = append(req.Scope, molecule.ScopeAddition{Path: finding.Path, Reason: "reviewer finding " + finding.Fingerprint})
		}
		if perReviewer[finding.Reviewer] == 0 {
			reviewers = append(reviewers, finding.Reviewer)
		}
		perReviewer[finding.Reviewer]++
	}
	counts := make([]string, 0, len(reviewers))
	for _, r := range reviewers {
		counts = append(counts, fmt.Sprintf("%s (%d)", r, perReviewer[r]))
	}
	note := fmt.Sprintf("Fix round %d: %d reviewer finding(s) from findings round %d of %d on commit %s, by reviewer %s; %d scope path(s).", round, len(picked), at+1, rounds, head, strings.Join(counts, ", "), len(req.Scope))
	req.Note = fitRunes(safetext.Text(note, readiness.MaxOperatorNoteBytes), readiness.MaxOperatorNoteBytes, readiness.MaxOperatorNoteBytes)
	return req
}

// recordedFinding is a summary finding as a fix round records it: its text
// flattened and cut to the round's bounds, and a path the round cannot hold
// cleared, so the finding still answers but adds no scope.
func recordedFinding(sf delivery.SummaryFinding) readiness.FixRoundFinding {
	title := fitRunes(flatten(sf.Title), maxFindingTitleBytes, maxFindingTitleBytes)
	if strings.TrimSpace(title) == "" {
		title = "(untitled)"
	}
	path := sf.Path
	if path != "" && (len(path) > maxFindingPathBytes || readiness.ValidatePath(path) != nil || safetext.Text(path, maxFindingPathBytes) != path) {
		path = ""
	}
	line := sf.Line
	if line < 0 || line > maxFindingLine {
		line = 0
	}
	return readiness.FixRoundFinding{Fingerprint: sf.Fingerprint, Reviewer: fitRunes(flatten(sf.Reviewer), 200, 200), Severity: sf.Severity, Path: path, Line: line, Title: title}
}

// flatten is one line of display text, as the CLI shows findings.
func flatten(s string) string { return learning.DisplayText(s, 300) }

// fitRunes cuts s to at most runes runes and bytes bytes, on a rune boundary.
func fitRunes(s string, runes, bytes int) string {
	if utf8.RuneCountInString(s) > runes {
		s = string([]rune(s)[:runes])
	}
	for len(s) > bytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}
