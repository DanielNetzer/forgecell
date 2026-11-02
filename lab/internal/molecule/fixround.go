package molecule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// FixRoundProposed is the event kind AmendFixRound appends to the fix-round
// revision it records.
const FixRoundProposed = "fix-round-proposed"

// errFixRoundIssueClosed refuses a fix round, or its approval, once the Issue is
// closed.
var errFixRoundIssueClosed = errors.New("the Issue is closed, so fix rounds stop")

// FixRoundRequest describes one fix-round revision of a delivered Molecule. Head
// and HeadTree are the delivered head the caller read from the receipt; Findings
// are the findings the round answers; each Acceptance item is one line that cites
// finding:<fingerprint> (or issue); Scope proposes repository paths, each with a
// reason. ExpectParent is the latest plan digest the caller reviewed.
type FixRoundRequest struct {
	// ID names the Molecule.
	ID           string
	Head         string
	HeadTree     string
	Round        int
	Findings     []readiness.FixRoundFinding
	Acceptance   []readiness.Criterion
	Scope        []ScopeAddition
	ExpectParent string
	Note         string
}

var (
	fixRoundMoleculeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	fixRoundObjectID   = regexp.MustCompile(`^[a-f0-9]{40}$`)
	fixRoundDigest     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Bounds of one fix-round request beyond the amend bounds.
const (
	maxFixRoundEvidence    = 64
	maxFixRoundEvidenceID  = 128
	maxFixRoundScopePath   = 1024
	maxFixRoundListedPaths = 16
)

func (q FixRoundRequest) validate() error {
	if !fixRoundMoleculeID.MatchString(q.ID) {
		return fmt.Errorf("fix round needs an exact Molecule id")
	}
	if !fixRoundDigest.MatchString(q.ExpectParent) {
		return fmt.Errorf("fix round needs the reviewed latest plan digest")
	}
	if !fixRoundObjectID.MatchString(q.Head) || !fixRoundObjectID.MatchString(q.HeadTree) {
		return fmt.Errorf("fix round head and head tree must be 40 hex characters")
	}
	if q.Round < 1 || q.Round > readiness.MaxFixRounds {
		return fmt.Errorf("a delivered Molecule takes at most %d fix rounds", readiness.MaxFixRounds)
	}
	if len(q.Findings) > readiness.MaxFixRoundFindings {
		return fmt.Errorf("a fix round carries at most %d findings", readiness.MaxFixRoundFindings)
	}
	if len(q.Acceptance) == 0 || len(q.Acceptance) > maxAmendAdditions || len(q.Scope) > maxAmendAdditions {
		return fmt.Errorf("a fix round needs 1 to %d acceptance items and at most %d scope paths", maxAmendAdditions, maxAmendAdditions)
	}
	for _, a := range q.Acceptance {
		if err := readiness.ValidateOperatorText(a.Description, maxAmendTextBytes); err != nil {
			return fmt.Errorf("fix round acceptance: %w", err)
		}
		if len(a.Evidence) == 0 || len(a.Evidence) > maxFixRoundEvidence {
			return fmt.Errorf("fix round acceptance needs 1 to %d evidence citations", maxFixRoundEvidence)
		}
		for _, id := range a.Evidence {
			if len(id) > maxFixRoundEvidenceID || readiness.ValidateOperatorText(id, maxFixRoundEvidenceID) != nil {
				return fmt.Errorf("fix round acceptance cites a malformed evidence id")
			}
		}
	}
	for _, s := range q.Scope {
		if err := readiness.ValidateOperatorText(s.Reason, maxAmendTextBytes); err != nil {
			return fmt.Errorf("fix round scope reason: %w", err)
		}
	}
	if q.Note != "" {
		if err := readiness.ValidateOperatorText(q.Note, readiness.MaxOperatorNoteBytes); err != nil {
			return fmt.Errorf("fix round note: %w", err)
		}
	}
	return nil
}

// AmendFixRound records a fix-round revision of a delivered Molecule: a successor
// of its latest plan whose capture base is the delivered head req.Head, with the
// proposed scope and acceptance added. It takes the same Molecule flock and
// approval lock directory as AmendFromLatest and refuses, writing nothing, unless
// the Molecule has a passing verification, req.Head is not the intake base and
// req.HeadTree is the verified tree, its workspace HEAD is req.Head with tree
// req.HeadTree and no other change, req.Round is the previous fix round plus
// 1 and at most readiness.MaxFixRounds, review attempts remain below the cap, and
// req.ExpectParent is still the latest plan. The Issue is read again through the
// Molecule's own reader: a closed Issue refuses, and an edited body is recorded on
// the revision while the intake acceptance stays as approved. Finding paths
// outside the repository or on a protected path are left out of the scope with a
// note. The revision then waits for exact scope approval like any amendment.
func AmendFixRound(ctx context.Context, o Options, req FixRoundRequest) (Record, error) {
	var empty Record
	if err := req.validate(); err != nil {
		return empty, err
	}
	lab, err := filepath.Abs(o.LabDir)
	if err != nil {
		return empty, err
	}
	id := req.ID
	unlockLoop, err := lockMolecule(lab, id)
	if err != nil {
		return empty, lockedByLoop(err)
	}
	defer unlockLoop()
	lock := filepath.Join(lab, "ledgers", id+".lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return empty, fmt.Errorf("Molecule is locked: another approval, amend or recover holds ledgers/%s.lock", id)
	}
	defer os.Remove(lock)
	r, err := readMolecule(lab, id)
	if err != nil {
		// A path error would name the local Lab directory; report only its cause.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return empty, fmt.Errorf("Molecule %s ledger cannot be read: %v", id, err)
	}
	if err = refuseRetired(r); err != nil {
		return r, err
	}
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return r, fmt.Errorf("Molecule has no readiness plan to amend")
	}
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	if req.ExpectParent != latest.Digest {
		return r, ErrStalePlan
	}
	if r.Workspace.Path == "" || (r.CheckoutStatus != "" && r.CheckoutStatus != "available") {
		return r, fmt.Errorf("coding checkout unavailable; refresh intake")
	}
	if !passingVerification(r, latest.Digest) {
		return r, fmt.Errorf("the Molecule has no passing verification, so it cannot take a fix round")
	}
	// The head must be a delivered commit of the verified tree: never the intake base
	// (nothing was delivered) and never a commit of any other tree.
	if err = fixRoundHeadDelivered(latest.Plan, req.Head, req.HeadTree, r.Verification.SourceTree); err != nil {
		return r, err
	}
	previousRound := 0
	if latest.Plan.FixRound != nil {
		previousRound = latest.Plan.FixRound.Round
	}
	if req.Round != previousRound+1 {
		return r, fmt.Errorf("fix round %d must be round %d: the previous fix round plus 1", req.Round, previousRound+1)
	}
	if len(r.ReviewAttempts) >= maxReviewAttempts {
		return r, fmt.Errorf("the review cap of %d attempts is reached, so a fix round could not be reviewed", maxReviewAttempts)
	}
	if err = fixRoundWorkspaceAt(ctx, r, latest.Plan, req.Head, req.HeadTree); err != nil {
		return r, err
	}
	issue, err := fixRoundReadIssue(ctx, o, &r)
	if err != nil {
		return r, err
	}
	frozen := latest.Plan.Inputs.Issue
	if issue.Title != frozen.Title {
		return r, fmt.Errorf("the Issue title changed since intake, so a fix round cannot keep the approved intake; refresh intake")
	}
	bodySHA := readiness.IssueBodySHA256(issue.Body)
	ref := readiness.FixRoundRef{Round: req.Round, Head: req.Head, HeadTree: req.HeadTree, Findings: slices.Clone(req.Findings), IssueChanged: bodySHA != readiness.IssueBodySHA256(frozen.Body), IssueBodySHA256: bodySHA}
	if ref.Findings == nil {
		ref.Findings = []readiness.FixRoundFinding{}
	}
	if err = ref.Validate(); err != nil {
		return r, err
	}
	scope, notes := fixRoundScope(r, latest.Plan, req.Scope)
	next, err := buildRevision(r, latest.Plan, AmendRequest{AddScope: scope, Note: req.Note})
	if err != nil {
		return r, err
	}
	next.FixRound = &ref
	// Declared protected deltas were justified by the change from the intake base; the
	// protected reference is now the delivered head, so none carries into the round.
	if len(next.ExpectedProtectedDeltas) > 0 {
		next.ExpectedProtectedDeltas = nil
		notes = append(notes, "Fix round: the expected protected-test failures of the delivered plan were not carried over; the protected reference is now the delivered head.")
	}
	known := map[string]bool{}
	for _, c := range next.Analysis.Acceptance {
		known[c.Description] = true
	}
	for _, a := range req.Acceptance {
		if known[a.Description] {
			return r, fmt.Errorf("acceptance item already present: %s", oneLine(a.Description))
		}
		known[a.Description] = true
		next.Analysis.Acceptance = append(next.Analysis.Acceptance, readiness.Criterion{Description: a.Description, Evidence: slices.Clone(a.Evidence)})
	}
	detail := fmt.Sprintf("Fix round %d proposed from delivered head %s (tree %s): %d finding(s), %d scope addition(s), %d acceptance item(s).", ref.Round, ref.Head, ref.HeadTree, len(ref.Findings), len(scope), len(req.Acceptance))
	if ref.IssueChanged {
		detail += fmt.Sprintf(" The Issue body changed since intake (SHA-256 %s); the intake acceptance stays as approved.", ref.IssueBodySHA256)
	}
	return amendLockedThen(ctx, r, latest.Digest, next, "", func(rec *Record) {
		digest := rec.Readiness.Plans[len(rec.Readiness.Plans)-1].Digest
		rec.Readiness.Events = append(rec.Readiness.Events, readiness.Event{Kind: FixRoundProposed, Digest: digest, At: stamp(), Detail: detail})
		rec.Notes = append(rec.Notes, detail)
		rec.Notes = append(rec.Notes, notes...)
	})
}

// passingVerification reports a Molecule whose latest plan passed its required
// checks and still waits at the human review gate.
func passingVerification(r Record, digest string) bool {
	if r.Verification == nil || !r.Verification.RequiredChecksPassed || r.Readiness.Phase != "review-waiting" {
		return false
	}
	for _, e := range r.Readiness.Events {
		if e.Digest == digest && e.Kind == readiness.VerificationReviewWaiting {
			return true
		}
	}
	return false
}

// fixRoundWorkspaceAt requires the workspace HEAD to be head with tree headTree,
// head to descend from the intake base in the source repository, and the
// workspace to hold no change beyond head.
func fixRoundWorkspaceAt(ctx context.Context, r Record, p readiness.Plan, head, headTree string) error {
	got, err := git(ctx, r.Workspace.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || got != head {
		return fmt.Errorf("the workspace HEAD is not the delivered head %s", head)
	}
	tree, err := git(ctx, r.Workspace.Path, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil || tree != headTree {
		return fmt.Errorf("the workspace HEAD tree is not the delivered tree %s", headTree)
	}
	if err = fixRoundHeadReachable(ctx, r, p.Inputs.BaseCommit, head); err != nil {
		return err
	}
	clean, err := verification.Capture(ctx, r.Workspace.Path, head, nil, p.Artifacts)
	if err != nil || len(clean.Changes) > 0 || clean.Tree != headTree {
		return fmt.Errorf("the workspace has changes beyond the delivered head; restore it before a fix round")
	}
	return nil
}

// fixRoundHeadReachable requires head to be a commit of the source repository
// that descends from the intake base, so the branch stays rooted there.
func fixRoundHeadReachable(ctx context.Context, r Record, base, head string) error {
	if (!fixRoundObjectID.MatchString(base) && !fixRoundDigest.MatchString(base)) || !fixRoundObjectID.MatchString(head) {
		return fmt.Errorf("the fix round's head is not an exact commit id")
	}
	if _, err := git(ctx, r.Workspace.SourceRoot, "merge-base", "--is-ancestor", base, head); err != nil {
		return fmt.Errorf("the fix round's head %s is not reachable from the intake base in the source repository", head)
	}
	return nil
}

// fixRoundHeadDelivered refuses a head that is the intake base commit, or a head
// tree that is not the verified tree.
func fixRoundHeadDelivered(p readiness.Plan, head, headTree, verifiedTree string) error {
	if head == p.Inputs.BaseCommit {
		return fmt.Errorf("the fix round's head is the intake base commit, so nothing was delivered to fix")
	}
	if verifiedTree == "" || headTree != verifiedTree {
		return fmt.Errorf("the fix round's head tree %s is not the verified tree, so it is not the delivered head", headTree)
	}
	return nil
}

// fixRoundVerifiedTree is the tree the latest verification of the plan a fix round
// amends ran on, and whether that verification passed. The amended plan is the
// parent of the first plan that carries the round.
func fixRoundVerifiedTree(r Record, round int) (string, bool) {
	plans := r.Readiness.Plans
	parent := ""
	for i := 1; i < len(plans); i++ {
		if f := plans[i].Plan.FixRound; f != nil && f.Round == round {
			parent = plans[i-1].Digest
			break
		}
	}
	if parent == "" {
		return "", false
	}
	for i := len(r.VerificationAttempts) - 1; i >= 0; i-- {
		if a := r.VerificationAttempts[i]; a.PlanDigest == parent {
			return a.SourceTree, a.Status == "passed"
		}
	}
	return "", false
}

// fixRoundHeadInPlace is the approval's freshness rule for a fix-round plan: the
// round's head is a delivered commit of the verified tree of the plan it amends,
// the workspace HEAD is that head with that tree, and the head is reachable from
// the intake base in the source repository.
func fixRoundHeadInPlace(ctx context.Context, r Record, p readiness.Plan) error {
	verified, passed := fixRoundVerifiedTree(r, p.FixRound.Round)
	if !passed {
		verified = ""
	}
	if err := fixRoundHeadDelivered(p, p.FixRound.Head, p.FixRound.HeadTree, verified); err != nil {
		return fmt.Errorf("%w; no coding was started", err)
	}
	got, err := git(ctx, r.Workspace.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || got != p.FixRound.Head {
		return fmt.Errorf("the workspace HEAD is not the fix round's delivered head %s; no coding was started", p.FixRound.Head)
	}
	tree, err := git(ctx, r.Workspace.Path, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil || tree != p.FixRound.HeadTree {
		return fmt.Errorf("the workspace HEAD tree is not the fix round's delivered tree %s; no coding was started", p.FixRound.HeadTree)
	}
	return fixRoundHeadReachable(ctx, r, p.Inputs.BaseCommit, p.FixRound.Head)
}

// fixRoundReadIssue re-reads the Molecule's Issue through its own reader: the
// keyed reader for a keyed Issue, else the GitHub reader. A closed Issue or one
// that no longer matches the Molecule is refused. Plugin calls join the Record,
// which is written only when the fix round is recorded.
func fixRoundReadIssue(ctx context.Context, o Options, r *Record) (Issue, error) {
	var issue Issue
	var err error
	if r.Issue.Key != "" {
		if o.ReadIssueKey == nil {
			return issue, fmt.Errorf("a keyed Molecule needs an Issue key reader for a fix round")
		}
		issue, err = o.ReadIssueKey(ctx, r.Issue.Repo, r.Issue.Key, r.Workspace.Path)
		absorbPluginCalls(r, o.PluginCalls)
	} else {
		reader := o.ReadIssue
		if reader == nil {
			reader = readIssue
		}
		issue, err = reader(ctx, r.Issue.Repo, r.Issue.Number, r.Workspace.Path)
	}
	if err != nil {
		return issue, err
	}
	if issue.State != "OPEN" {
		return issue, errFixRoundIssueClosed
	}
	matches := issue.Key == "" && issue.Number == r.Issue.Number && issue.Repo == r.Issue.Repo && issue.URL == fmt.Sprintf("https://github.com/%s/issues/%d", r.Issue.Repo, r.Issue.Number)
	if r.Issue.Key != "" {
		matches = readKeyedIssueMatches(issue, r.Issue.Repo, r.Issue.Key)
	}
	if !matches {
		return issue, fmt.Errorf("the re-read Issue does not match the Molecule's Issue")
	}
	return issue, nil
}

// fixRoundApprovalIssue is the Issue a fix-round approval compares with the frozen
// inputs. The Issue must be open. When its body is the one the round recorded and
// its title is unchanged, the frozen Issue stands in for it, so the intake
// acceptance and evidence stay as approved; any other edit is compared as usual.
func fixRoundApprovalIssue(p readiness.Plan, issue Issue) (Issue, error) {
	if issue.State != "OPEN" {
		return issue, errFixRoundIssueClosed
	}
	if readiness.IssueBodySHA256(issue.Body) != p.FixRound.IssueBodySHA256 {
		return issue, fmt.Errorf("the Issue body changed after fix round %d was proposed; propose the fix round again", p.FixRound.Round)
	}
	if issue.Title == p.Inputs.Issue.Title {
		issue.Body = p.Inputs.Issue.Body
	}
	return issue, nil
}

// fixRoundScope keeps the proposed paths a fix round may add: clean repository
// paths that are not already in scope, not on the sensitive-path list or a frozen
// check definition or reviewed check input, not refused by the in-scope definition
// rule, and valid against the frozen tree.
// Every other path is left out with a note that names it only when it is safe to
// print.
func fixRoundScope(r Record, p readiness.Plan, proposed []ScopeAddition) ([]ScopeAddition, []string) {
	inScope, folded := map[string]bool{}, map[string]bool{}
	for _, s := range p.Analysis.Scope {
		inScope[s.Path] = true
		folded[strings.ToLower(s.Path)] = true
	}
	evidencePath := map[string]string{}
	for _, e := range p.Evidence {
		evidencePath[e.ID] = e.Path
	}
	protected := map[string]bool{}
	for _, c := range append(append([]readiness.Check{}, p.Analysis.Checks...), p.Setup...) {
		for _, id := range c.Definitions {
			if file, ok := evidencePath[id]; ok {
				protected[file] = true
			}
		}
	}
	for _, input := range p.CheckInputs {
		protected[input.Path] = true
	}
	var kept []ScopeAddition
	var protectedPaths, outsidePaths []string
	unprintable := 0
	for _, s := range proposed {
		if len(s.Path) > maxFixRoundScopePath || readiness.ValidatePath(s.Path) != nil || !printablePath(s.Path) {
			unprintable++
			continue
		}
		if inScope[s.Path] {
			continue
		}
		if sensitiveScopePath(s.Path) || protected[s.Path] {
			protectedPaths = append(protectedPaths, s.Path)
			continue
		}
		var added readiness.ScopedPath
		outside := folded[strings.ToLower(s.Path)] || r.RepositoryEvidence == nil
		if !outside {
			evidence, err := scopeEvidence(s.Path, p.Evidence, *r.RepositoryEvidence)
			added = readiness.ScopedPath{Path: s.Path, Reason: s.Reason, Evidence: []string{evidence}}
			outside = err != nil || r.RepositoryEvidence.ValidateScope([]readiness.ScopedPath{added}) != nil
		}
		if outside {
			outsidePaths = append(outsidePaths, s.Path)
			continue
		}
		// The in-scope definition rule buildRevision applies to added paths, probed one
		// path at a time: the rule is per path, so a path a check's definitions or argv
		// name is dropped alone instead of refusing the whole round.
		probe := readiness.Analysis{Scope: []readiness.ScopedPath{added}, Checks: p.Analysis.Checks}
		if readiness.ValidateInScopeDefinitionsForNewFiles(probe, r.RepositoryEvidence.References(), readiness.NewScopeFiles(probe.Scope, r.RepositoryEvidence.Files)) != nil {
			protectedPaths = append(protectedPaths, s.Path)
			continue
		}
		inScope[s.Path] = true
		folded[strings.ToLower(s.Path)] = true
		kept = append(kept, s)
	}
	var notes []string
	if len(protectedPaths) > 0 {
		notes = append(notes, fmt.Sprintf("Fix round: %d finding path(s) match a protected path and were left out of the proposed scope: %s. A human may add them with a reviewed amendment.", len(protectedPaths), boundedPaths(protectedPaths, maxFixRoundListedPaths)))
	}
	if len(outsidePaths) > 0 {
		notes = append(notes, fmt.Sprintf("Fix round: %d finding path(s) lie outside the repository's frozen tree (no parent component, a case clash, or a file, symlink, submodule or directory on the way) and were left out of the proposed scope: %s.", len(outsidePaths), boundedPaths(outsidePaths, maxFixRoundListedPaths)))
	}
	if unprintable > 0 {
		notes = append(notes, fmt.Sprintf("Fix round: %d finding path(s) are not clean repository paths and were left out of the proposed scope.", unprintable))
	}
	return kept, notes
}

// printablePath refuses format, bidi, line and paragraph separator characters,
// which ValidatePath allows but a note must never echo.
func printablePath(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return false
		}
	}
	return true
}
