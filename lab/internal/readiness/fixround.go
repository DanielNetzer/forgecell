package readiness

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxFixRounds bounds the fix rounds of one delivered Molecule.
const MaxFixRounds = 3

// MaxFixRoundFindings bounds the findings one fix round carries.
const MaxFixRoundFindings = 500

// Bounds of one fix-round finding's text fields.
const (
	maxFindingReviewerBytes = 200
	maxFindingTitleBytes    = 1000
	maxFindingPathBytes     = 1024
	maxFindingLine          = 1 << 31
)

// FindingEvidencePrefix starts the evidence id an acceptance criterion cites for a
// finding of the plan's fix round: finding:<fingerprint>.
const FindingEvidencePrefix = "finding:"

// FixRoundFinding is one finding a fix round answers, as the findings evidence
// recorded it. It is untrusted reviewer output, bounded and validated as text.
type FixRoundFinding struct {
	Fingerprint string `json:"fingerprint"`
	Reviewer    string `json:"reviewer"`
	Severity    string `json:"severity"`
	Path        string `json:"path"`
	Line        int64  `json:"line"`
	Title       string `json:"title"`
}

// FixRoundRef marks a plan revision of a delivered Molecule whose capture base is
// the delivered head instead of the intake base commit. Round counts from 1 to
// MaxFixRounds. IssueBodySHA256 is the SHA-256 of the Issue body re-read when the
// round was proposed, and IssueChanged says it differs from the approved intake.
type FixRoundRef struct {
	Round           int               `json:"round"`
	Head            string            `json:"head"`
	HeadTree        string            `json:"headTree"`
	Findings        []FixRoundFinding `json:"findings"`
	IssueChanged    bool              `json:"issueChanged"`
	IssueBodySHA256 string            `json:"issueBodySha256"`
}

const errFixRoundShape = "malformed fix round"

var (
	fixRoundObject  = regexp.MustCompile(`^[a-f0-9]{40}$`)
	findingSeverity = map[string]bool{"info": true, "low": true, "medium": true, "high": true}
)

// CaptureBase is the commit every capture, diff, worktree and protected view of
// the plan compares against: the fix round's delivered head when set, otherwise
// the intake base commit.
func (p Plan) CaptureBase() string {
	if p.FixRound != nil {
		return p.FixRound.Head
	}
	return p.Inputs.BaseCommit
}

// IssueBodySHA256 is the hex SHA-256 a fix round records for an Issue body.
func IssueBodySHA256(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
}

// FindingEvidenceID is the evidence id that cites the finding fingerprint.
func FindingEvidenceID(fingerprint string) string { return FindingEvidencePrefix + fingerprint }

// Validate refuses a malformed fix round: a round outside 1..MaxFixRounds, a head
// or head tree that is not 40 lowercase hex characters, a missing body hash, more
// than MaxFixRoundFindings findings, or a malformed or duplicate finding.
func (f FixRoundRef) Validate() error {
	if f.Round < 1 || f.Round > MaxFixRounds {
		return fmt.Errorf("%s: round must be from 1 to %d", errFixRoundShape, MaxFixRounds)
	}
	if !fixRoundObject.MatchString(f.Head) || !fixRoundObject.MatchString(f.HeadTree) {
		return fmt.Errorf("%s: head and headTree must be 40 hex characters", errFixRoundShape)
	}
	if !hex256.MatchString(f.IssueBodySHA256) {
		return fmt.Errorf("%s: issueBodySha256 must be 64 hex characters", errFixRoundShape)
	}
	if len(f.Findings) > MaxFixRoundFindings {
		return fmt.Errorf("%s: at most %d findings", errFixRoundShape, MaxFixRoundFindings)
	}
	seen := map[string]bool{}
	for i, finding := range f.Findings {
		if err := finding.validate(); err != nil {
			return fmt.Errorf("%s: finding %d: %w", errFixRoundShape, i+1, err)
		}
		if seen[finding.Fingerprint] {
			return fmt.Errorf("%s: duplicate finding fingerprint", errFixRoundShape)
		}
		seen[finding.Fingerprint] = true
	}
	return nil
}

func (f FixRoundFinding) validate() error {
	if !hex256.MatchString(f.Fingerprint) {
		return fmt.Errorf("fingerprint must be 64 hex characters")
	}
	if !findingText(f.Reviewer, maxFindingReviewerBytes) {
		return fmt.Errorf("reviewer must be non-blank one-line text of at most %d bytes", maxFindingReviewerBytes)
	}
	if !findingSeverity[f.Severity] {
		return fmt.Errorf("severity must be info, low, medium or high")
	}
	// ValidatePath allows format, bidi and separator characters; a stored finding path
	// is shown to a human, so it must render as the path it is.
	if f.Path != "" && (len(f.Path) > maxFindingPathBytes || ValidatePath(f.Path) != nil || !inertRunes(f.Path)) {
		return fmt.Errorf("path must be empty or a clean, printable repository path of at most %d bytes", maxFindingPathBytes)
	}
	if f.Line < 0 || f.Line > maxFindingLine {
		return fmt.Errorf("line must be from 0 to %d", int64(maxFindingLine))
	}
	if !findingText(f.Title, maxFindingTitleBytes) {
		return fmt.Errorf("title must be non-blank one-line text of at most %d bytes", maxFindingTitleBytes)
	}
	return nil
}

// findingText accepts bounded, non-blank valid UTF-8 without control, format or
// bidi characters, so it renders as one inert line.
func findingText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s) && inertRunes(s)
}

// inertRunes refuses control, format, bidi, line and paragraph separator
// characters.
func inertRunes(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return false
		}
	}
	return true
}

// findingRefs adds the plan's finding:<fingerprint> evidence ids to refs, for the
// acceptance criteria only. A plan without a fix round adds none, so a finding id
// stays an unknown citation there.
func (p Plan) findingRefs(refs map[string]bool) map[string]bool {
	out := make(map[string]bool, len(refs))
	for id := range refs {
		out[id] = true
	}
	if p.FixRound != nil {
		for _, f := range p.FixRound.Findings {
			out[FindingEvidenceID(f.Fingerprint)] = true
		}
	}
	return out
}

// validateFixRound is the single-plan rule: a fix round is a well-formed ref on an
// amendment, never on the initial plan.
func (p Plan) validateFixRound() error {
	if p.FixRound == nil {
		return nil
	}
	if p.Revision < 2 {
		return fmt.Errorf("a fix round must be an amendment of a delivered plan")
	}
	return p.FixRound.Validate()
}

// sameFixRound compares two refs field by field; nil and empty findings are the
// same list, as they are after a JSON round trip.
func sameFixRound(a, b FixRoundRef) bool {
	return a.Round == b.Round && a.Head == b.Head && a.HeadTree == b.HeadTree && a.IssueChanged == b.IssueChanged &&
		a.IssueBodySHA256 == b.IssueBodySHA256 && slices.Equal(a.Findings, b.Findings)
}

// VerificationReviewWaiting is the event kind of a plan whose checks passed: the
// verified state a fix round must start from.
const VerificationReviewWaiting = "verification-review-waiting"

// ValidateFixRounds checks the fix-round chain of the whole history. A plan that
// starts a new fix round must number it its parent plan's round plus 1 (or 1 when
// the parent has none), and the parent must have reached a verified state. A plan
// that keeps its parent's fix round must carry it unchanged, and no plan may drop
// a fix round its parent had, since its capture base would move back.
func (s State) ValidateFixRounds() error {
	verified := map[string]bool{}
	for _, e := range s.Events {
		if e.Kind == VerificationReviewWaiting {
			verified[e.Digest] = true
		}
	}
	for i, proposal := range s.Plans {
		next := proposal.Plan.FixRound
		if i == 0 {
			if next != nil {
				return fmt.Errorf("a fix round must be an amendment of a delivered plan")
			}
			continue
		}
		previous := s.Plans[i-1].Plan.FixRound
		if next == nil {
			if previous != nil {
				return fmt.Errorf("revision %d drops fix round %d; a fix round's capture base cannot move back", proposal.Plan.Revision, previous.Round)
			}
			continue
		}
		if previous != nil && next.Round == previous.Round {
			if !sameFixRound(*previous, *next) {
				return fmt.Errorf("revision %d changes fix round %d; propose the next round instead", proposal.Plan.Revision, next.Round)
			}
			continue
		}
		want := 1
		if previous != nil {
			want = previous.Round + 1
		}
		if next.Round != want {
			return fmt.Errorf("fix round %d must be round %d: the round is the parent plan's round plus 1", next.Round, want)
		}
		if !verified[s.Plans[i-1].Digest] {
			return fmt.Errorf("fix round %d needs a parent plan that was verified and delivered", next.Round)
		}
	}
	return nil
}
