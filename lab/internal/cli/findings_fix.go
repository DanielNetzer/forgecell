package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Bounds of a fix-round proposal.
const (
	// maxFixRoundIncludes bounds the --include fingerprints of one proposal.
	maxFixRoundIncludes = 50
	// maxFixRoundProposed is the most findings one fix round answers: each adds
	// one acceptance item, and an amendment adds at most 64.
	maxFixRoundProposed = 64
	// maxFixRoundAcceptanceRunes bounds one acceptance line.
	maxFixRoundAcceptanceRunes = 1000
	// Bounds of a finding's fields as a fix round records them.
	maxFixRoundFindingTitleBytes = 1000
	maxFixRoundFindingPathBytes  = 1024
	maxFixRoundFindingLine       = 1 << 31
)

var findingFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Seams of the fix-round commands; tests replace them.
var (
	fixRoundReadRecord = molecule.ReadRecord
	fixRoundAmend      = amendFixRoundThroughIssue
	fixRoundPreview    = delivery.PreviewFixRound
	fixRoundPush       = pushFixRoundThroughForge
)

// runFindingsPropose proposes the next fix round of a published delivery from
// the latest complete findings round on its head: the open findings at or
// above each reviewer's autoFix level, and those named by include. A finding
// an earlier fix round addressed that is still open is not proposed again
// unless included. Findings are untrusted evidence: their text reaches the
// revision only flattened and bounded, and nothing here approves or pushes.
func runFindingsPropose(ctx context.Context, lab, labArg, id string, f formula.Formula, refs []formula.ReviewerRef, includes []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if err := delivery.RefuseDemoLab(lab); err != nil {
		return fail(err)
	}
	receipt, err := delivery.ReadFixRoundReceipt(lab, id)
	if err != nil {
		return fail(err)
	}
	head, tree := delivery.ReceiptHead(receipt)
	round := len(receipt.FixRounds) + 1
	if round > delivery.MaxFixRounds {
		return fail(fmt.Errorf("the change request already has %d fix rounds, so fix rounds stop", delivery.MaxFixRounds))
	}
	record, err := delivery.ReadFindings(lab, id)
	if err != nil {
		return fail(err)
	}
	at := -1
	for i := len(record.Rounds) - 1; i >= 0; i-- {
		if record.Rounds[i].Complete && record.Rounds[i].Commit == head {
			at = i
			break
		}
	}
	if at < 0 {
		return fail(fmt.Errorf("collect findings for the current head first: forgecell findings %s --collect", id))
	}
	autoFix := map[string]string{}
	for i, r := range refs {
		autoFix[delivery.ReviewerKey(i, r.Plugin)] = r.AutoFix
	}
	onHead := record
	onHead.Rounds = record.Rounds[:at+1]
	summary := delivery.Summarize(onHead, autoFix)
	if summary.Round != at {
		return fail(fmt.Errorf("collect findings for the current head first: forgecell findings %s --collect", id))
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
	var picked []delivery.SummaryFinding
	seen, found := map[string]bool{}, map[string]bool{}
	for _, g := range summary.Open {
		for _, sf := range g.Findings {
			fp := sf.Fingerprint
			if !findingFingerprint.MatchString(fp) || seen[fp] {
				continue
			}
			seen[fp] = true
			found[fp] = true
			if addressed[fp] && !included[fp] {
				fmt.Fprintf(out, "finding %s came back after a fix; it is not proposed again\n", fp)
				continue
			}
			if sf.AutoFix || included[fp] {
				picked = append(picked, sf)
			}
		}
	}
	for _, fp := range includes {
		if !found[fp] {
			return fail(fmt.Errorf("finding %s is not an open finding of the latest complete findings round on the head", fp))
		}
	}
	if len(picked) == 0 {
		fmt.Fprintln(out, "no findings to propose")
		return 0
	}
	if len(picked) > maxFixRoundProposed {
		return fail(fmt.Errorf("%d findings are more than one fix round answers (%d); raise the reviewers' autoFix level or include fewer", len(picked), maxFixRoundProposed))
	}
	rec, err := fixRoundReadRecord(lab, id)
	if err != nil {
		return fail(fmt.Errorf("the Molecule ledger cannot be read"))
	}
	if rec.Readiness == nil || len(rec.Readiness.Plans) == 0 {
		return fail(fmt.Errorf("the Molecule has no readiness plan to amend"))
	}
	parent := rec.Readiness.Plans[len(rec.Readiness.Plans)-1].Digest
	req := fixRoundRequest(id, head, tree, round, parent, at, len(record.Rounds), picked)
	amended, err := fixRoundAmend(ctx, lab, rec, req)
	if err != nil {
		return fail(err)
	}
	presentFixRoundProposal(out, labArg, amended, round, len(picked))
	return 0
}

// fixRoundRequest builds the revision for the picked findings: one acceptance
// line and one scope path for each, and a note naming the reviewers, the
// findings round and the counts.
func fixRoundRequest(id, head, tree string, round int, parent string, at, rounds int, picked []delivery.SummaryFinding) molecule.FixRoundRequest {
	req := molecule.FixRoundRequest{ID: id, Head: head, HeadTree: tree, Round: round, ExpectParent: parent}
	scoped := map[string]bool{}
	perReviewer := map[string]int{}
	var reviewers []string
	for _, sf := range picked {
		finding := fixRoundFinding(sf)
		req.Findings = append(req.Findings, finding)
		location := "-"
		if finding.Path != "" {
			location = finding.Path
			if finding.Line > 0 {
				location += fmt.Sprintf(":%d", finding.Line)
			}
		}
		line := fmt.Sprintf("Fix the reviewer finding [%s] %s: %s (%s)", finding.Severity, location, finding.Title, readiness.FindingEvidenceID(finding.Fingerprint))
		req.Acceptance = append(req.Acceptance, readiness.Criterion{Description: fitRunes(safetext.Text(line, maxFixRoundAcceptanceRunes), maxFixRoundAcceptanceRunes, 4000), Evidence: []string{readiness.FindingEvidenceID(finding.Fingerprint)}})
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

// fixRoundFinding is a summary finding as a fix round records it: its text
// flattened and cut to the round's bounds, and a path the round cannot hold
// cleared, so the finding still answers but adds no scope.
func fixRoundFinding(sf delivery.SummaryFinding) readiness.FixRoundFinding {
	title := fitRunes(flatten(sf.Title), maxFixRoundFindingTitleBytes, maxFixRoundFindingTitleBytes)
	if strings.TrimSpace(title) == "" {
		title = "(untitled)"
	}
	path := sf.Path
	if path != "" && (len(path) > maxFixRoundFindingPathBytes || readiness.ValidatePath(path) != nil || safetext.Text(path, maxFixRoundFindingPathBytes) != path) {
		path = ""
	}
	line := sf.Line
	if line < 0 || line > maxFixRoundFindingLine {
		line = 0
	}
	return readiness.FixRoundFinding{Fingerprint: sf.Fingerprint, Reviewer: fitRunes(flatten(sf.Reviewer), 200, 200), Severity: sf.Severity, Path: path, Line: line, Title: title}
}

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

// presentFixRoundProposal prints the revision summary and the exact command
// that approves it, in the keyed form for a keyed Molecule.
func presentFixRoundProposal(out io.Writer, labArg string, r molecule.Record, round, findings int) {
	plans := r.Readiness.Plans
	latest := plans[len(plans)-1]
	p := latest.Plan
	summary := "no change"
	if len(plans) > 1 {
		summary = molecule.AmendDiffSummary(plans[len(plans)-2].Plan, p)
	}
	fmt.Fprintf(out, "Fix round %d proposed for %s: %d finding(s); revision %d: %s\n", round, r.ID, findings, p.Revision, summary)
	issueArg := fmt.Sprint(r.Issue.Number)
	if r.Issue.IsKeyed() {
		if intake.ValidateIssueKey(r.Issue.Key) != nil {
			fmt.Fprintln(out, "This Molecule's Issue key is not a valid tracker key, so no approve command is shown.")
			return
		}
		issueArg = shellWord(r.Issue.Key)
	}
	fmt.Fprintf(out, "Review the full ledger, then approve exactly this plan:\nforgecell run %s --lab %s --formula %s --base %s --target %s --approve %s\n", issueArg, shellWord(labArg), shellWord(r.FormulaID), shellWord(p.Inputs.BaseCommit), shellWord(p.Inputs.TargetBranch), latest.Digest)
}

// amendFixRoundThroughIssue records the fix round, re-reading the Issue through
// the Molecule's own reader: the tracker plugin of its approved Formula for a
// keyed Molecule, else GitHub.
func amendFixRoundThroughIssue(ctx context.Context, lab string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error) {
	o := molecule.Options{LabDir: lab}
	if r.Issue.IsKeyed() {
		loaded, err := formula.Load(lab, r.FormulaID)
		if err != nil || !loaded.Approved {
			return molecule.Record{}, fmt.Errorf("a keyed fix round needs the Molecule's approved Formula to read its Issue")
		}
		if _, ok := loaded.Formula.TrackerPlugin(); !ok {
			return molecule.Record{}, fmt.Errorf("a keyed fix round needs a Formula that reads Issues from a tracker plugin")
		}
		calls := molecule.NewPluginCallLog(lab)
		keyed, _, err := mcpadapter.TrackerFor(lab, loaded.Formula, mcpadapter.Options{LookupEnv: pluginLookupEnv, Observer: calls.Observe})
		if err != nil {
			return molecule.Record{}, err
		}
		o.ReadIssueKey, o.PluginCalls = molecule.KeyedReader(keyed, calls), calls
	}
	return molecule.AmendFixRound(ctx, o, req)
}

// pushFixRoundThroughForge pushes through the Molecule's forge, chosen as
// deliver chooses it: its Formula snapshot's forge plugin, else GitHub. A
// preview without an approval starts no plugin.
func pushFixRoundThroughForge(ctx context.Context, o delivery.FixRoundPushOptions) (delivery.Receipt, error) {
	if o.Approve != "" {
		f, plugin, err := moleculeForgeFormula(o.LabDir, o.MoleculeID)
		if err != nil {
			return delivery.Receipt{}, err
		}
		if plugin {
			changes, meta, err := newPluginForge(o.LabDir, f)
			if err != nil {
				return delivery.Receipt{}, err
			}
			o.Changes, o.Plugin = changes, meta
		}
	}
	return delivery.PushFixRound(ctx, o)
}

// runFindingsPush is findings --push: without an approval it prints the
// preview and the command that approves it; with one it pushes exactly that.
func runFindingsPush(ctx context.Context, lab, id, approve, title string, confirm bool, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	r, err := fixRoundPush(ctx, delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: id, Approve: approve, Title: title, Confirm: confirm})
	var pending *delivery.FixRoundApprovalRequired
	if errors.As(err, &pending) {
		if code := encode(out, stderr, pending.Preview); code != 0 {
			return code
		}
		fmt.Fprintln(stderr, fixRoundPushCommand(id, title, pending.Preview))
		return 0
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, fixRoundPushedLine(r))
	return 0
}

// fixRoundPushCommand is the exact command that pushes the previewed round.
func fixRoundPushCommand(id, title string, p delivery.FixRoundPreview) string {
	line := fmt.Sprintf("Review this fix round %d push, then push it with forgecell findings %s --push --approve %s", p.Round, id, p.Digest)
	if title != "" {
		line += " --title " + shellWord(flatten(title))
	}
	if p.NeedsConfirm() {
		line += " --confirm"
	}
	return line
}

// fixRoundPushedLine reports the recorded fix round.
func fixRoundPushedLine(r delivery.Receipt) string {
	if len(r.FixRounds) == 0 {
		return "No fix round was recorded."
	}
	fr := r.FixRounds[len(r.FixRounds)-1]
	return fmt.Sprintf("Fix round %d pushed to %s: commit %s is the change request's head. Nothing was merged or marked ready.", fr.Round, flatten(r.Preview.Branch), fr.Commit)
}

// fixRoundAfterRun is the run-completion hook of a fix-round plan. When the
// run's latest plan carries a fix round whose checks passed, it pushes the
// round automatically if the review is green and acceptance is verified,
// bound to the preview digest it just computed; otherwise it prints how to
// push it. A plan without a fix round prints nothing. It returns a non-zero
// exit code when the round could not be pushed.
func fixRoundAfterRun(ctx context.Context, out, stderr io.Writer, lab string, r molecule.Record) int {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return 0
	}
	ref := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.FixRound
	if ref == nil || r.Readiness.Phase != "review-waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		return 0
	}
	if !safeID.MatchString(r.ID) {
		return 0
	}
	o := delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: r.ID}
	p, err := fixRoundPreview(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "forgecell: fix round %d was not pushed: %v\n", ref.Round, err)
		return 1
	}
	if p.NeedsConfirm() {
		fmt.Fprintf(out, "fix round %d is ready; push it with forgecell findings %s --push --approve %s --confirm\n", p.Round, r.ID, p.Digest)
		return 0
	}
	// The scope approval of this exact revision is the push approval.
	o.Approve = p.Digest
	receipt, err := fixRoundPush(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "forgecell: fix round %d was not pushed: %v\n", p.Round, err)
		return 1
	}
	fmt.Fprintln(out, fixRoundPushedLine(receipt))
	return 0
}
