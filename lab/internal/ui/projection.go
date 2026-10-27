package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Summary is one Molecule in GET /api/molecules.
type Summary struct {
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	Number      int64  `json:"number"`
	Title       string `json:"title"`
	FormulaID   string `json:"formulaId"`
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	CurrentAtom string `json:"currentAtom"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt"`
	Lane        string `json:"lane"`
	Activity    string `json:"activity,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
	// Inbox is the Molecule's Inbox entry, or absent for one the Inbox does not list.
	Inbox InboxView `json:"inbox,omitzero"`
}

// MoleculeList is GET /api/molecules. Counts are taken from Molecules, so they
// always reconcile with it, and the Lanes' counts sum to All. Molecules also
// lists retired ones, counted only under "retired" and never in All.
type MoleculeList struct {
	Sample      bool           `json:"sample"`
	ETag        string         `json:"etag"`
	Complete    bool           `json:"complete"`
	Truncated   bool           `json:"truncated"`
	Diagnostics []string       `json:"diagnostics"`
	Counts      map[string]int `json:"counts"`
	Molecules   []Summary      `json:"molecules"`
}

// Detail is GET /api/molecules/{id}: a projection of molecule.Record. It never
// carries the Lab directory, workspace or source paths, credentials, environment
// or HTML; absolute local paths inside free text are replaced by labels such as
// <lab> or ~, or by "[local path]".
type Detail struct {
	Sample      bool           `json:"sample"`
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Phase       string         `json:"phase"`
	Lane        string         `json:"lane"`
	Activity    string         `json:"activity,omitempty"`
	Mode        string         `json:"mode"`
	FormulaID   string         `json:"formulaId"`
	StartedAt   string         `json:"startedAt"`
	FinishedAt  string         `json:"finishedAt"`
	CurrentAtom string         `json:"currentAtom"`
	Issue       IssueView      `json:"issue"`
	Workspace   WorkspaceView  `json:"workspace"`
	Formula     FormulaView    `json:"formula"`
	Readiness   *ReadinessView `json:"readiness"`
	Atoms       []AtomView     `json:"atoms"`
	Attempts    []AttemptView  `json:"attempts"`
	Delivery    *DeliveryView  `json:"delivery"`
	Gate        GateView       `json:"gate"`
	Next        NextView       `json:"next"`
	Retired     *RetiredView   `json:"retired"`
	Diagnostics []string       `json:"diagnostics"`
	// View is the plain-words Molecule view (#251): absent for sample data.
	View *MoleculeView `json:"view,omitempty"`
}

// RetiredView is a retired Molecule's retired event: its time, the status it
// replaced and its reason, redacted and bounded to one line.
type RetiredView struct {
	At          string `json:"at"`
	PriorStatus string `json:"priorStatus"`
	Reason      string `json:"reason"`
}

// Gate states. GATES AND DECISIONS and NEXT ACTION both render the one GateView
// of a Detail, so they cannot disagree about whether a gate is open.
const (
	// GateScope: the exact plan Digest waits for scope approval.
	GateScope = "scope"
	// GateDelivery: the finished Molecule may be previewed and published as a draft.
	GateDelivery = "delivery"
	// GatePublished: the review gate was resolved by draft publication.
	GatePublished = "published"
	// GateNone: no human gate this UI performs is open.
	GateNone = "none"
)

// GateView is the selected Molecule's gate state. Receipt, Lifecycle and URL are
// set for GatePublished; URL only when it is the Issue repository's pull request.
type GateView struct {
	State     string `json:"state"`
	Digest    string `json:"digest,omitempty"`
	Receipt   string `json:"receipt,omitempty"`
	Lifecycle string `json:"lifecycle,omitempty"`
	URL       string `json:"url,omitempty"`
}

// NextView is the NEXT ACTION title and copy, derived from the lane and the
// GateView: it names Gates and decisions only while a gate there is open.
type NextView struct {
	Title string `json:"title"`
	Copy  string `json:"copy"`
}

// gateState is the one gate state of a record in lane: a pending scope approval
// of a Molecule waiting for a decision first, then a published receipt, then an
// unpublished finished Molecule. A running or blocked Molecule has no open gate.
func gateState(r molecule.Record, lane string, d *DeliveryState, view *DeliveryView) GateView {
	if lane == LaneRetired {
		return GateView{State: GateNone}
	}
	if digest, ok := pendingDigest(r); ok && lane == LaneDecision && sha256Hex.MatchString(digest) {
		return GateView{State: GateScope, Digest: digest}
	}
	if d.Published() {
		g := GateView{State: GatePublished, Receipt: stateText(d.State), Lifecycle: stateText(d.Lifecycle)}
		if view != nil {
			g.URL = view.URL
		}
		return g
	}
	if (lane == LaneDecision || lane == LaneOther) && r.FinishedAt != "" && (r.Status == "waiting" || r.Status == "recorded") {
		return GateView{State: GateDelivery}
	}
	return GateView{State: GateNone}
}

// NextPublished is NEXT ACTION for a published draft.
const NextPublished = "Draft published · merge on GitHub; merge and production are not verified by Forgecell."

// nextAction is NEXT ACTION for a Molecule in lane with gate g.
func nextAction(lane, activity, status, phase, current string, g GateView, delivery *DeliveryView) NextView {
	at := ""
	if current = stateText(current); current != "" {
		at = " · " + current
	}
	recorded := func(s string) string {
		if s = stateText(s); s != "" {
			return s
		}
		return "Not recorded"
	}
	label := ""
	if delivery != nil {
		label = delivery.Label + "."
	}
	switch lane {
	case LaneDecision:
		if g.State == GateScope || g.State == GateDelivery {
			return NextView{"Needs your decision" + at, "This Molecule waits at a human gate (phase " + recorded(phase) + "). Review it and decide under Gates and decisions, or with the forgecell CLI. The engine checks every decision again."}
		}
		return NextView{"Waiting at a human gate" + at, "This Molecule waits at a human gate (phase " + recorded(phase) + ") that this Lab UI does not perform. Inspect it with forgecell ledger and decide with the forgecell CLI. The engine checks every decision again."}
	case LaneRunning:
		if activity == ActivityQueued {
			return NextView{"Queued for checks" + at, "Another Molecule in this Lab holds the verification slot. Checks start, and their time budgets begin, when the slot is free. This view updates from the ledger."}
		}
		return NextView{"Running" + at, "The Lab is working within the approved scope. This view updates from the ledger."}
	case LaneBlocked:
		return NextView{"Blocked" + at, strings.TrimSpace("Status " + recorded(status) + ". Failure evidence stays in the ledger; a failed or blocked run is not a delivery. " + label)}
	case LaneFinished:
		return NextView{"Draft published", NextPublished}
	case LaneRetired:
		return NextView{"Retired", "This Molecule was retired: it is finished history and every record is kept. The loop never resumes it and may start a fresh Molecule for its Issue."}
	}
	if g.State == GateDelivery {
		return NextView{"Ready for a draft PR" + at, strings.TrimSpace("This Molecule finished with status " + recorded(status) + ". Preview and publish a draft PR under Gates and decisions, or with the forgecell CLI. " + label)}
	}
	if label == "" {
		label = "No published delivery receipt. This Molecule is in no lane; it appears under All and Other."
	}
	return NextView{"Recorded · " + recorded(status), label}
}

type IssueView struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	// Body is the recorded Issue text, served as plain text and never rendered.
	Body      string `json:"body"`
	State     string `json:"state"`
	UpdatedAt string `json:"updatedAt"`
	// URL is set only when the recorded URL names this exact Issue on GitHub.
	URL string `json:"url,omitempty"`
}

type WorkspaceView struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
}

type FormulaView struct {
	SHA256   string `json:"sha256"`
	Approved bool   `json:"approved"`
	// YAML is the saved snapshot, included only with ?yaml=1.
	YAML string `json:"yaml,omitempty"`
}

type ReadinessView struct {
	Phase  string      `json:"phase"`
	Plans  []PlanView  `json:"plans"`
	Events []EventView `json:"events"`
}

type PlanView struct {
	Revision     int    `json:"revision"`
	Digest       string `json:"digest"`
	ParentDigest string `json:"parentDigest"`
}

type EventView struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	At     string `json:"at"`
	Detail string `json:"detail"`
	Tree   string `json:"tree,omitempty"`
}

type AtomView struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Detail     string          `json:"detail"`
	StartedAt  string          `json:"startedAt,omitempty"`
	FinishedAt string          `json:"finishedAt,omitempty"`
	ElapsedMS  int64           `json:"elapsedMs,omitempty"`
	ExitCode   *int            `json:"exitCode,omitempty"`
	Provenance *ProvenanceView `json:"provenance,omitempty"`
}

type ProvenanceView struct {
	DeclaredSource    string       `json:"declaredSource,omitempty"`
	DeclaredBinding   string       `json:"declaredBinding,omitempty"`
	DeclaredWorkflows []string     `json:"declaredWorkflows,omitempty"`
	DeclaredPurpose   string       `json:"declaredPurpose,omitempty"`
	PlannedAction     string       `json:"plannedAction,omitempty"`
	Execution         string       `json:"execution,omitempty"`
	SkipReason        string       `json:"skipReason,omitempty"`
	Actions           []ActionView `json:"actions"`
}

type ActionView struct {
	AttemptID       string           `json:"attemptId"`
	PlanDigest      string           `json:"planDigest,omitempty"`
	ResolvedSource  string           `json:"resolvedSource,omitempty"`
	PlannedAction   string           `json:"plannedAction"`
	StartedAt       string           `json:"startedAt"`
	ExecutedBinding string           `json:"executedBinding,omitempty"`
	Observed        *ObservationView `json:"observed,omitempty"`
}

type ObservationView struct {
	Action         string         `json:"action"`
	Result         string         `json:"result"`
	FinishedAt     string         `json:"finishedAt"`
	ExternalAction string         `json:"externalAction,omitempty"`
	ExternalResult string         `json:"externalResult,omitempty"`
	Evidence       []EvidenceView `json:"evidence"`
}

// EvidenceView names evidence by a Lab-relative path. A path outside the Lab is
// withheld and says so. Viewable says the evidence endpoint may serve the path,
// subject to its SHA-256 and file checks when it is read.
type EvidenceView struct {
	Pointer  string `json:"pointer,omitempty"`
	Path     string `json:"path,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Note     string `json:"note,omitempty"`
	Viewable bool   `json:"viewable,omitempty"`
}

// AttemptView is one analysis, coding or review harness attempt with its model
// and usage exactly as recorded; a missing usage means unavailable, not zero.
type AttemptView struct {
	Kind       string         `json:"kind"`
	ID         string         `json:"id"`
	Status     string         `json:"status,omitempty"`
	StartedAt  string         `json:"startedAt,omitempty"`
	FinishedAt string         `json:"finishedAt,omitempty"`
	Model      string         `json:"model,omitempty"`
	Binding    string         `json:"binding,omitempty"`
	Usage      *harness.Usage `json:"usage,omitempty"`
	// FailureClass is the attempt's typed failure class (harness-not-authenticated),
	// shown only when it is that known class.
	FailureClass string `json:"failureClass,omitempty"`
}

// knownFailureClass keeps a recorded attempt failure class only when it is one the
// engine writes, so ledger text never reaches the view through it.
func knownFailureClass(class string) string {
	if class == harness.HarnessNotAuthenticated {
		return class
	}
	return ""
}

// DeliveryView is the receipt state, reported exactly. Label is "Draft
// published" only for a published receipt, and never says merged.
type DeliveryView struct {
	State     string `json:"state"`
	Lifecycle string `json:"lifecycle,omitempty"`
	Published bool   `json:"published"`
	Label     string `json:"label"`
	// URL is set only for a GitHub pull request URL of the Issue's repository.
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// receiptReader reads the displayed part of one Molecule's delivery receipt;
// tests replace it.
type receiptReader func(lab, id string) (delivery.ReceiptStatus, bool, error)

// loadDelivery reads a receipt for display. An unreadable receipt is reported,
// never treated as published.
func loadDelivery(read receiptReader, lab string, r molecule.Record) (*DeliveryState, *DeliveryView) {
	receipt, found, err := read(lab, r.ID)
	if !found {
		return nil, nil
	}
	if err != nil {
		return &DeliveryState{State: "unreadable"}, &DeliveryView{State: "unreadable", Label: "Delivery receipt unreadable", Error: "the receipt could not be read safely"}
	}
	d := &DeliveryState{State: receipt.State, Lifecycle: receipt.Lifecycle}
	view := &DeliveryView{State: d.State, Lifecycle: d.Lifecycle, Published: d.Published(), Label: DeliveryLabel(d)}
	if pullURL(receipt.URL, r.Issue.Repo) {
		view.URL = receipt.URL
	}
	return d, view
}

var pullPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/[1-9][0-9]{0,15}$`)

func pullURL(raw, repo string) bool {
	m := pullPattern.FindStringSubmatch(raw)
	return m != nil && strings.EqualFold(m[1], repo)
}

// issueURL returns the canonical URL of the recorded Issue when the recorded URL
// names exactly that Issue, and "" otherwise.
func issueURL(issue molecule.Issue) string {
	if !strings.HasPrefix(strings.ToLower(issue.URL), "https://github.com/") {
		return ""
	}
	ref, err := intake.ParseIssueRef(issue.URL)
	if err == nil {
		ref, err = intake.BindRepository(ref, issue.Repo)
	}
	if err != nil || ref.Number != issue.Number {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number)
}

// redactor replaces the record's local directories, and the user's home, in free
// text with labels, then replaces any other absolute path with "[local path]",
// so a projection never carries an absolute local path.
type redactor struct{ entries []redaction }

type redaction struct{ path, label string }

func newRedactor(lab string, r molecule.Record) redactor {
	var entries []redaction
	add := func(path, label string) {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return
		}
		entries = append(entries, redaction{filepath.Clean(path), label})
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != filepath.Clean(path) {
			entries = append(entries, redaction{resolved, label})
		}
	}
	add(r.Workspace.Path, "<workspace>")
	add(r.Workspace.SourceRoot, "<source>")
	add(r.LabDir, "<lab>")
	if abs, err := filepath.Abs(lab); err == nil {
		add(abs, "<lab>")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	// Longest first, so a workspace inside the Lab keeps its own label.
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].path) > len(entries[j].path) })
	return redactor{entries: entries}
}

// pathByte reports a byte that continues a path name, so a directory matches
// only at a name boundary: home /Users/dan never rewrites /Users/danielle.
func pathByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("._-~@+%", b) >= 0
}

// localPath is an absolute path, or a file:// URL, under a real local root, in
// free text: it starts the text or follows a separator, quote or |, >, so URLs,
// a/b pairs and repo-relative text such as /api/molecules are not paths.
var localPath = regexp.MustCompile("(^|[\\s\"'`(\\[=:,|>“”‘’])((?:file://)?/(?:Users|home|root|private|var|tmp|Volumes|opt|etc|mnt|srv|System|Library)(?:/[^\\s\"'`<>()\\[\\]{},;|“”‘’]*)?)")

// pathEnd reports a character after which a root name such as /var has ended.
func pathEnd(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-~@+%", r))
}

func (x redactor) text(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		matched := false
		for _, e := range x.entries {
			if strings.HasPrefix(s[i:], e.path) && (i+len(e.path) == len(s) || !pathByte(s[i+len(e.path)])) {
				b.WriteString(e.label)
				i += len(e.path)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(s[i])
			i++
		}
	}
	return scrubPaths(b.String())
}

// scrubPaths replaces each remaining local path with "[local path]", keeping
// sentence punctuation that ends it.
func scrubPaths(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range localPath.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[4], m[5]
		// A bare root must end there: /variable is not /var.
		if r, _ := utf8.DecodeRuneInString(s[end:]); end < len(s) && !pathEnd(r) {
			continue
		}
		path := s[start:end]
		trimmed := strings.TrimRight(path, ".:")
		b.WriteString(s[last:start])
		b.WriteString("[local path]")
		b.WriteString(path[len(trimmed):])
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func (x redactor) texts(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = x.text(v)
	}
	return out
}

// evidence keeps a Lab-relative evidence path; an absolute path inside the Lab is
// made relative and any other path is withheld.
func evidence(lab string, r molecule.Record, e molecule.ProvenanceEvidence) EvidenceView {
	v := EvidenceView{Pointer: e.Pointer, SHA256: e.SHA256}
	switch path := e.Path; {
	case path == "":
	case !filepath.IsAbs(path):
		if filepath.IsLocal(path) {
			v.Path = filepath.ToSlash(filepath.Clean(path))
		} else {
			v.Note = "path outside the Lab withheld"
		}
	default:
		for _, root := range []string{r.LabDir, lab} {
			abs, err := filepath.Abs(root)
			if root == "" || err != nil {
				continue
			}
			if rel, err := filepath.Rel(abs, path); err == nil && filepath.IsLocal(rel) {
				v.Path = filepath.ToSlash(rel)
				break
			}
		}
		if v.Path == "" {
			v.Note = "path outside the Lab withheld"
		}
	}
	return v
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// evidenceAllowlist maps each Lab-relative file the record lists as evidence
// with a SHA-256 to its recorded digests: Atom observation evidence, harness
// result and capture files, and verification observations and results. Paths
// outside the Lab, pointer-only references and malformed digests are left out,
// so the evidence endpoint serves nothing else.
func evidenceAllowlist(lab string, r molecule.Record) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(path, sum string) {
		if !sha256Hex.MatchString(sum) {
			return
		}
		v := evidence(lab, r, molecule.ProvenanceEvidence{Path: path, SHA256: sum})
		if v.Path == "" {
			return
		}
		if out[v.Path] == nil {
			out[v.Path] = map[string]bool{}
		}
		out[v.Path][sum] = true
	}
	files := func(refs ...molecule.VerificationFile) {
		for _, f := range refs {
			add(f.Path, f.SHA256)
		}
	}
	for _, a := range r.Atoms {
		if a.Provenance == nil {
			continue
		}
		for _, act := range a.Provenance.Actions {
			if act.Observed == nil {
				continue
			}
			for _, e := range act.Observed.Evidence {
				add(e.Path, e.SHA256)
			}
		}
	}
	for _, a := range r.HarnessAttempts {
		if a.Evidence != nil {
			files(*a.Evidence)
		}
		files(a.CaptureEvidence...)
	}
	for _, a := range r.VerificationAttempts {
		files(a.Observations...)
		if a.Result != nil {
			files(*a.Result)
		}
	}
	return out
}

// currentAtom is the first Atom still active or waiting, or "" when none is.
func currentAtom(r molecule.Record) string {
	for _, a := range r.Atoms {
		if a.Status == "active" || a.Status == "running" || a.Status == "waiting" {
			return a.ID
		}
	}
	return ""
}

func phase(r molecule.Record) string {
	if r.Readiness == nil {
		return ""
	}
	return r.Readiness.Phase
}

func summarize(lab string, r molecule.Record, read receiptReader) Summary {
	x := newRedactor(lab, r)
	d, view := loadDelivery(read, lab, r)
	s := Summary{ID: r.ID, Repository: r.Issue.Repo, Number: r.Issue.Number, Title: x.text(r.Issue.Title), FormulaID: r.FormulaID, Status: r.Status, Phase: phase(r), CurrentAtom: currentAtom(r), StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Lane: Lane(r.Status, phase(r), d)}
	s.Activity = Activity(s.Lane, molecule.QueuedForChecks(r))
	if view != nil {
		s.Delivery = view.Label
	}
	s.Inbox = inboxView(x, r, s.Lane, s.Activity, d)
	return s
}

func project(lab string, r molecule.Record, read receiptReader, withYAML bool) Detail {
	x := newRedactor(lab, r)
	allowed := evidenceAllowlist(lab, r)
	d, view := loadDelivery(read, lab, r)
	out := Detail{ID: r.ID, Status: r.Status, Phase: phase(r), Lane: Lane(r.Status, phase(r), d), Mode: r.Mode, FormulaID: r.FormulaID, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CurrentAtom: currentAtom(r), Delivery: view, Diagnostics: []string{}, Atoms: []AtomView{}, Attempts: []AttemptView{}}
	out.Activity = Activity(out.Lane, molecule.QueuedForChecks(r))
	out.Gate = gateState(r, out.Lane, d, view)
	out.Next = nextAction(out.Lane, out.Activity, r.Status, out.Phase, out.CurrentAtom, out.Gate, view)
	if hint, ok := molecule.NotAuthenticated(r); ok && out.Lane == LaneBlocked {
		// The blocked reason is the fixed login hint, never the provider's text (#244).
		out.Next = NextView{"Blocked · harness not authenticated", hint + ", then start this Issue again. Failure evidence stays in the ledger."}
	}
	if rt := r.Retired; rt != nil {
		out.Retired = &RetiredView{At: stateText(rt.At), PriorStatus: stateText(rt.PriorStatus), Reason: harness.ReviewDisplayText(x.text(rt.Reason), molecule.MaxRetireReason)}
	}
	out.Issue = IssueView{Repository: r.Issue.Repo, Number: r.Issue.Number, Title: x.text(r.Issue.Title), Body: x.text(r.Issue.Body), State: r.Issue.State, UpdatedAt: r.Issue.UpdatedAt, URL: issueURL(r.Issue)}
	if r.Issue.URL != "" && out.Issue.URL == "" {
		out.Diagnostics = append(out.Diagnostics, "recorded Issue URL does not name this Issue on GitHub; no link is shown")
	}
	out.Workspace = WorkspaceView{Repository: r.Workspace.Repo, Branch: r.Workspace.Branch, BaseCommit: r.Workspace.BaseCommit}
	out.Formula = FormulaView{SHA256: r.FormulaSnapshot.SHA256, Approved: r.FormulaApproved}
	if withYAML {
		out.Formula.YAML = r.FormulaSnapshot.YAML
		if sum := sha256.Sum256([]byte(r.FormulaSnapshot.YAML)); hex.EncodeToString(sum[:]) != r.FormulaSnapshot.SHA256 {
			out.Diagnostics = append(out.Diagnostics, "saved Formula YAML does not match its recorded SHA-256")
		}
	}
	if s := r.Readiness; s != nil {
		rv := &ReadinessView{Phase: s.Phase, Plans: []PlanView{}, Events: []EventView{}}
		for _, p := range s.Plans {
			rv.Plans = append(rv.Plans, PlanView{Revision: p.Plan.Revision, Digest: p.Digest, ParentDigest: p.Plan.ParentDigest})
		}
		for _, e := range s.Events {
			rv.Events = append(rv.Events, EventView{Kind: e.Kind, Digest: e.Digest, At: e.At, Detail: x.text(e.Detail), Tree: e.Tree})
		}
		out.Readiness = rv
	}
	for _, a := range r.Atoms {
		av := AtomView{ID: a.ID, Type: a.Type, Status: a.Status, Detail: x.text(a.Detail), StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, ElapsedMS: a.ElapsedMS, ExitCode: a.ExitCode}
		if p := a.Provenance; p != nil {
			pv := &ProvenanceView{DeclaredSource: x.text(p.DeclaredSource), DeclaredBinding: p.DeclaredBinding, DeclaredWorkflows: x.texts(p.DeclaredWorkflows), DeclaredPurpose: x.text(p.DeclaredPurpose), PlannedAction: x.text(p.PlannedAction), Execution: p.Execution, SkipReason: x.text(p.SkipReason), Actions: []ActionView{}}
			for _, act := range p.Actions {
				ac := ActionView{AttemptID: act.AttemptID, PlanDigest: act.PlanDigest, ResolvedSource: x.text(act.ResolvedSource), PlannedAction: x.text(act.PlannedAction), StartedAt: act.StartedAt, ExecutedBinding: act.ExecutedBinding}
				if o := act.Observed; o != nil {
					ov := &ObservationView{Action: x.text(o.Action), Result: x.text(o.Result), FinishedAt: o.FinishedAt, ExternalAction: x.text(o.ExternalAction), ExternalResult: x.text(o.ExternalResult), Evidence: []EvidenceView{}}
					for _, e := range o.Evidence {
						ev := evidence(lab, r, e)
						ev.Viewable = ev.Path != "" && allowed[ev.Path][ev.SHA256]
						ov.Evidence = append(ov.Evidence, ev)
					}
					ac.Observed = ov
				}
				pv.Actions = append(pv.Actions, ac)
			}
			av.Provenance = pv
		}
		out.Atoms = append(out.Atoms, av)
	}
	for _, a := range r.AnalysisAttempts {
		out.Attempts = append(out.Attempts, AttemptView{Kind: "analysis", ID: fmt.Sprintf("analysis-%d", a.Ordinal), Status: a.Status, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Usage: a.Usage, FailureClass: knownFailureClass(a.FailureClass)})
	}
	for _, a := range r.HarnessAttempts {
		status := ""
		if a.Coding != nil {
			status = a.Coding.Outcome
		}
		out.Attempts = append(out.Attempts, AttemptView{Kind: "coding", ID: a.ID, Status: status, Model: a.Model, Usage: a.Usage, FailureClass: knownFailureClass(a.FailureClass)})
	}
	for _, a := range r.ReviewAttempts {
		out.Attempts = append(out.Attempts, AttemptView{Kind: "review", ID: fmt.Sprintf("review-%d", a.Ordinal), Status: a.Status, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Binding: a.Binding, Usage: a.Usage, FailureClass: knownFailureClass(a.FailureClass)})
	}
	out.View = moleculeView(x, r, out.Lane, out.Activity, out.Gate, d)
	return out
}

// MoleculeView is the Molecule view in plain words: the state pill, the seven
// steps, the latest plan, the one decision this page offers, the roles and the
// planning cost. Free text is redacted, bounded to one line and stripped of raw
// ids, digests and timestamps; Plan.Digest and PlannedAt are data for the
// browser (the bound digest, shown only short inside Plan details, and a time it
// formats), never prose.
type MoleculeView struct {
	State        StateView         `json:"state"`
	Steps        []StepView        `json:"steps"`
	Current      int               `json:"current"`
	Plan         *PlanBrief        `json:"plan,omitempty"`
	Decision     DecisionView      `json:"decision"`
	Roles        RolesView         `json:"roles"`
	PlanningCost *PlanningCostView `json:"planningCost,omitempty"`
	// Control is set by the handler for a running Molecule: whether this Lab UI
	// can stop it and, when not, who runs it.
	Control *ControlView `json:"control,omitempty"`
}

// State tones: what the pill's dot and colour say, always with its text.
const (
	ToneNeeds   = "needs"
	ToneRunning = "running"
	ToneBlocked = "blocked"
	ToneMerged  = "merged"
	ToneNeutral = "neutral"
)

// StateView is the state pill: "Waiting for your approval", "Coding" (the
// browser appends the minutes since Since, the current attempt's start, then
// Note, a fixed rerun note), "Ready to publish", "Blocked · …".
type StateView struct {
	Label string `json:"label"`
	Tone  string `json:"tone"`
	Since string `json:"since,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Step states.
const (
	StepDone    = "done"
	StepCurrent = "current"
	StepFailed  = "blocked"
	StepTodo    = "todo"
)

// StepView is one of Plan · Your approval · Coding · Checks · Review · Draft PR
// · Merged, derived from the Atoms, the readiness phase and the receipt.
type StepView struct {
	Label string `json:"label"`
	State string `json:"state"`
}

// File changes, from the frozen base tree: absent from it is new, present is an
// edit. Unknown means the ledger has no frozen tree, so no badge is shown.
const (
	ChangeNew  = "new"
	ChangeEdit = "edit"
)

// PlanBrief is the latest plan in plain words. Lists are bounded; their Total
// says how many the plan has, so a cut list is never read as the whole.
type PlanBrief struct {
	Revision        int                `json:"revision"`
	Digest          string             `json:"digest"`
	PlannedAt       string             `json:"plannedAt,omitempty"`
	Summary         string             `json:"summary"`
	Questions       int                `json:"questions"`
	Components      []string           `json:"components"`
	ComponentsTotal int                `json:"componentsTotal"`
	BudgetMS        int                `json:"budgetMs,omitempty"`
	Note            string             `json:"note,omitempty"`
	Files           []PlanFileView     `json:"files"`
	FilesTotal      int                `json:"filesTotal"`
	CheckGroups     []CheckGroupView   `json:"checkGroups"`
	ChecksTotal     int                `json:"checksTotal"`
	Acceptance      []string           `json:"acceptance"`
	AcceptanceTotal int                `json:"acceptanceTotal"`
	Evidence        []PlanEvidenceView `json:"evidence"`
	EvidenceTotal   int                `json:"evidenceTotal"`
}

// PlanFileView is one scope path with its reason and its change from the base tree.
type PlanFileView struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Change string `json:"change,omitempty"`
}

// CheckGroupView is checks under one human name. Baseline marks the Formula's
// own checks, which every plan carries.
type CheckGroupView struct {
	Name     string          `json:"name"`
	Baseline bool            `json:"baseline"`
	Checks   []PlanCheckView `json:"checks"`
}

// PlanCheckView is one check's exact command, quoted as a shell would read it,
// and its directory.
type PlanCheckView struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Dir     string `json:"dir,omitempty"`
}

// PlanEvidenceView is one frozen evidence file the plan cites.
type PlanEvidenceView struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Decision kinds: the scope gate, the review gate (preview, then publish), a
// published draft, or nothing for this page to decide.
const (
	DecisionScope     = "scope"
	DecisionDelivery  = "delivery"
	DecisionPublished = "published"
	DecisionNone      = "none"
)

// DecisionView is the sticky decision card. It has at most one Primary, and a
// scope Primary carries the exact digest the Plan shows.
type DecisionView struct {
	Kind           string       `json:"kind"`
	Title          string       `json:"title"`
	Copy           string       `json:"copy"`
	Primary        *PrimaryView `json:"primary,omitempty"`
	RequestChanges bool         `json:"requestChanges,omitempty"`
	PullURL        string       `json:"pullUrl,omitempty"`
}

// Primary actions.
const (
	PrimaryApprove = "approve"
	PrimaryPreview = "preview"
)

// PrimaryView is the card's one primary action and the digest it is bound to.
type PrimaryView struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Digest string `json:"digest,omitempty"`
}

// RolesView names who works on the Molecule, from its Formula snapshot and its
// recorded attempts; an absent value is not recorded.
type RolesView struct {
	Coding   string `json:"coding,omitempty"`
	Model    string `json:"model,omitempty"`
	Reviewer string `json:"reviewer,omitempty"`
}

// PlanningCostView is the reported cost of the analysis attempts. USD is null
// when none reported one; Partial when only some did.
type PlanningCostView struct {
	USD      *float64 `json:"usd"`
	Reported int      `json:"reported"`
	Attempts int      `json:"attempts"`
	Partial  bool     `json:"partial"`
}

// Bounds of the plan in plain words.
const (
	maxPlanSummaryRunes = 1200
	maxPlanReasonRunes  = 400
	maxPlanPathRunes    = 300
	maxPlanCommandRunes = 400
	maxPlanFiles        = 200
	maxPlanChecks       = 64
	maxPlanAcceptance   = 64
	maxPlanEvidence     = 64
	maxPlanComponents   = 6
	maxRoleRunes        = 80
)

// planText is untrusted plan text as one bounded plain line: redacted, controls
// removed, ids replaced, digests shortened and timestamps cut to their date.
func planText(x redactor, s string, limit int) string {
	s = harness.ReviewDisplayText(redactSecrets(x.text(s)), 4*limit)
	s = rawID.ReplaceAllStringFunc(s, func(id string) string {
		if id[0] == 'a' {
			return "an assay"
		}
		return "a Molecule"
	})
	s = longHex.ReplaceAllStringFunc(s, func(h string) string { return h[:7] + "…" })
	s = isoTime.ReplaceAllString(s, "$1")
	return harness.ReviewDisplayText(s, limit)
}

func moleculeView(x redactor, r molecule.Record, lane, activity string, g GateView, d *DeliveryState) *MoleculeView {
	v := &MoleculeView{}
	step, label, since, note := stepProgress(r, lane, activity, d)
	merged := d.Published() && d.Lifecycle == "MERGED"
	v.Current = step
	for i, l := range StepLabels {
		s := StepTodo
		switch {
		case i < step || merged:
			s = StepDone
		case i == step && lane == LaneBlocked:
			s = StepFailed
		case i == step:
			s = StepCurrent
		}
		v.Steps = append(v.Steps, StepView{Label: l, State: s})
	}
	v.State = stateView(r, lane, activity, label, since, note, g, d)
	if r.Readiness != nil && len(r.Readiness.Plans) > 0 {
		v.Plan = planBrief(x, r)
	}
	v.Decision = decisionView(r, lane, activity, g, v.Plan)
	f, err := formula.Parse([]byte(r.FormulaSnapshot.YAML))
	if err == nil {
		v.Roles = roles(x, r, f)
		if v.Plan != nil && v.Plan.BudgetMS == 0 {
			b, _ := f.RoleBinding(formula.CodingHarness)
			v.Plan.BudgetMS = b.EffectiveTimeoutMS()
		}
	} else {
		v.Roles = roles(x, r, formula.Formula{})
	}
	v.PlanningCost = planningCost(r)
	return v
}

// stateView is the pill: a gate first, then the lane.
func stateView(r molecule.Record, lane, activity, label, since, note string, g GateView, d *DeliveryState) StateView {
	switch {
	case lane == LaneRetired:
		return StateView{Label: "Retired", Tone: ToneNeutral}
	case g.State == GateScope:
		return StateView{Label: "Waiting for your approval", Tone: ToneNeeds}
	case g.State == GateDelivery:
		return StateView{Label: "Ready to publish", Tone: ToneNeeds}
	case g.State == GatePublished && d != nil && d.Lifecycle == "MERGED":
		return StateView{Label: "Merged", Tone: ToneMerged}
	case g.State == GatePublished:
		return StateView{Label: "Draft published", Tone: ToneMerged}
	case lane == LaneBlocked:
		return StateView{Label: "Blocked · " + strings.TrimSuffix(blockedReason(r), "."), Tone: ToneBlocked}
	case lane == LaneRunning && activity == ActivityQueued:
		return StateView{Label: "Queued for checks", Tone: ToneRunning}
	case lane == LaneRunning:
		return StateView{Label: label, Tone: ToneRunning, Since: since, Note: note}
	case lane == LaneDecision:
		return StateView{Label: "Waiting at a gate", Tone: ToneNeeds}
	}
	return StateView{Label: "Recorded", Tone: ToneNeutral}
}

// plannedAt is when the latest plan was proposed or amended, re-formatted, or ""
// when no valid time is recorded for it.
func plannedAt(s *readiness.State, digest string) string {
	at := ""
	for _, e := range s.Events {
		if e.Digest == digest && e.Kind != "approved" {
			at = e.At
		}
	}
	return stepSince(at)
}

func planBrief(x redactor, r molecule.Record) *PlanBrief {
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	p := latest.Plan
	a := p.Analysis
	b := &PlanBrief{Revision: p.Revision, PlannedAt: plannedAt(r.Readiness, latest.Digest), Summary: planText(x, a.Summary, maxPlanSummaryRunes), Questions: len(a.Questions), BudgetMS: max(p.HarnessTimeoutMS, 0),
		Files: []PlanFileView{}, FilesTotal: len(a.Scope), CheckGroups: []CheckGroupView{}, ChecksTotal: len(a.Checks), Acceptance: []string{}, AcceptanceTotal: len(a.Acceptance), Evidence: []PlanEvidenceView{}, EvidenceTotal: len(p.Evidence), Components: []string{}}
	if sha256Hex.MatchString(latest.Digest) {
		b.Digest = latest.Digest
	}
	if p.OperatorNote != "" {
		b.Note = planText(x, p.OperatorNote, maxPlanReasonRunes)
	}
	// Newness comes from the frozen base tree only, never from plan text.
	var tree map[string]bool
	if r.RepositoryEvidence != nil {
		tree = map[string]bool{}
		for _, f := range r.RepositoryEvidence.Files {
			tree[f.Path] = true
		}
	}
	components := map[string]bool{}
	var order []string
	for i, s := range a.Scope {
		dir := path.Dir(s.Path)
		if dir == "." {
			dir = "repository root"
		}
		if !components[dir] {
			components[dir] = true
			order = append(order, dir)
		}
		if i >= maxPlanFiles {
			continue
		}
		f := PlanFileView{Path: planText(x, s.Path, maxPlanPathRunes), Reason: planText(x, s.Reason, maxPlanReasonRunes)}
		if tree != nil {
			f.Change = ChangeEdit
			if !tree[s.Path] {
				f.Change = ChangeNew
			}
		}
		b.Files = append(b.Files, f)
	}
	sort.Strings(order)
	b.ComponentsTotal = len(order)
	for _, c := range order[:min(len(order), maxPlanComponents)] {
		b.Components = append(b.Components, planText(x, c, maxPlanPathRunes))
	}
	b.CheckGroups = checkGroups(x, p)
	for i, c := range a.Acceptance {
		if i == maxPlanAcceptance {
			break
		}
		b.Acceptance = append(b.Acceptance, planText(x, c.Description, maxPlanReasonRunes))
	}
	for i, e := range p.Evidence {
		if i == maxPlanEvidence {
			break
		}
		b.Evidence = append(b.Evidence, PlanEvidenceView{ID: planText(x, e.ID, maxRoleRunes), Path: planText(x, e.Path, maxPlanPathRunes)})
	}
	return b
}

// The human names of check groups, in display order: the plan's own checks
// first, then the Formula baseline.
const (
	GroupCandidate  = "New tests for this change"
	GroupRegression = "Regression tests"
	GroupSetup      = "Setup"
	GroupFullSuite  = "Full test suite"
	GroupVet        = "Go vet · gofmt"
	GroupHygiene    = "Repository hygiene"
	GroupBaseline   = "Other Formula checks"
	GroupOther      = "Other checks"
)

var groupOrder = []string{GroupCandidate, GroupRegression, GroupSetup, GroupOther, GroupFullSuite, GroupVet, GroupHygiene, GroupBaseline}

// checkGroupName names a check by what it runs: a Formula baseline check by its
// command (go test, go vet or gofmt, the repository hygiene script), any other by
// its category.
func checkGroupName(c readiness.Check, baseline bool) string {
	if !baseline {
		switch c.Category {
		case "candidate":
			return GroupCandidate
		case "regression":
			return GroupRegression
		case "setup":
			return GroupSetup
		}
		return GroupOther
	}
	argv := c.Argv
	has := func(part string) bool {
		return slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, part) })
	}
	switch {
	case len(argv) > 1 && path.Base(argv[0]) == "go" && argv[1] == "test":
		return GroupFullSuite
	case len(argv) > 1 && path.Base(argv[0]) == "go" && argv[1] == "vet", has("gofmt"):
		return GroupVet
	case has("check-repo-hygiene"):
		return GroupHygiene
	}
	return GroupBaseline
}

func checkGroups(x redactor, p readiness.Plan) []CheckGroupView {
	baseline := map[string]bool{}
	for _, id := range p.FormulaBaseline {
		baseline[id] = true
	}
	groups := map[string]*CheckGroupView{}
	for i, c := range p.Analysis.Checks {
		if i == maxPlanChecks {
			break
		}
		name := checkGroupName(c, baseline[c.ID])
		g := groups[name]
		if g == nil {
			g = &CheckGroupView{Name: name, Baseline: baseline[c.ID], Checks: []PlanCheckView{}}
			groups[name] = g
		}
		check := PlanCheckView{ID: planText(x, c.ID, maxRoleRunes), Command: harness.ReviewDisplayText(redactSecrets(x.text(shellJoin(c.Argv))), maxPlanCommandRunes)}
		if c.Dir != "" && c.Dir != "." {
			check.Dir = planText(x, c.Dir, maxPlanPathRunes)
		}
		g.Checks = append(g.Checks, check)
	}
	out := []CheckGroupView{}
	for _, name := range groupOrder {
		if g := groups[name]; g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// shellJoin quotes argv as a POSIX shell would read it, for display only.
func shellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
		}) < 0 {
			parts = append(parts, a)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

// decisionView is the one decision this page offers for the gate, in plain words.
func decisionView(r molecule.Record, lane, activity string, g GateView, plan *PlanBrief) DecisionView {
	switch g.State {
	case GateScope:
		if plan == nil || plan.Digest != g.Digest {
			break
		}
		return DecisionView{Kind: DecisionScope, Title: "Approve this plan?",
			Copy:    "Forgecell will code " + countWords(plan.FilesTotal, "file", "files") + ", then run " + countWords(plan.ChecksTotal, "check", "checks") + " on its own. Nothing is published until you approve the draft PR.",
			Primary: &PrimaryView{Action: PrimaryApprove, Label: "Approve plan & start coding", Digest: g.Digest}, RequestChanges: true}
	case GateDelivery:
		return DecisionView{Kind: DecisionDelivery, Title: "Ready to publish?",
			Copy:    "Preview the exact change first. Publishing creates a draft pull request only; it never merges or deploys.",
			Primary: &PrimaryView{Action: PrimaryPreview, Label: "Preview delivery"}}
	case GatePublished:
		text := "The draft PR is published. Merge it on GitHub; Forgecell does not verify merges or production."
		if g.Lifecycle == "MERGED" {
			text = "The receipt last observed the pull request merged on GitHub. Forgecell does not verify production."
		}
		return DecisionView{Kind: DecisionPublished, Title: "Nothing to do", Copy: text, PullURL: g.URL}
	}
	d := DecisionView{Kind: DecisionNone, Title: "Nothing to do", Copy: "This Molecule is recorded and has no open gate."}
	switch lane {
	case LaneRunning:
		d.Title, d.Copy = "Nothing needs you now", "Forgecell is working within the approved scope. It comes back to your Inbox when it needs you."
		if activity == ActivityQueued {
			d.Copy = "It is queued for checks: another Molecule in this Lab holds the verification slot."
		}
	case LaneBlocked:
		d.Title, d.Copy = "Blocked", blockedReason(r)+" Failure evidence stays in the timeline."
		if hint, ok := molecule.NotAuthenticated(r); ok {
			d.Copy = harness.ReviewDisplayText(hint, maxReasonRunes) + ", then start this Issue again."
		}
	case LaneDecision:
		d.Title, d.Copy = "Waiting at a gate", "This page does not perform this gate. Decide with the forgecell CLI; the engine checks every decision again."
	case LaneRetired:
		d.Title, d.Copy = "Retired", "Every record is kept. The loop never resumes it and may start a fresh Molecule for its Issue."
	}
	return d
}

// countWords is a count with its noun: "1 file", "5 files".
func countWords(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// roles names the coding agent and model and the reviewer: the latest coding
// attempt's recorded model first, else the Formula's pinned one.
func roles(x redactor, r molecule.Record, f formula.Formula) RolesView {
	text := func(s string) string { return harness.ReviewDisplayText(x.text(s), maxRoleRunes) }
	coding, _ := f.RoleBinding(formula.CodingHarness)
	v := RolesView{Coding: text(coding.Binding), Model: text(coding.Model)}
	for i := len(r.HarnessAttempts) - 1; i >= 0; i-- {
		if m := r.HarnessAttempts[i].Model; m != "" && m != "model unpinned" {
			v.Model = text(m)
			break
		}
	}
	if _, ok, err := f.ReviewAtom(); err == nil && ok {
		if review, bound := f.RoleBinding(formula.ReviewHarness); bound {
			v.Reviewer = text(review.Binding)
		}
	}
	return v
}

// planningCost sums the analysis attempts' reported costs; nothing reported is
// null, never zero.
func planningCost(r molecule.Record) *PlanningCostView {
	if len(r.AnalysisAttempts) == 0 {
		return nil
	}
	v := &PlanningCostView{Attempts: len(r.AnalysisAttempts)}
	sum := 0.0
	for _, a := range r.AnalysisAttempts {
		if a.Usage == nil || a.Usage.TotalCostUSD == nil {
			continue
		}
		c := *a.Usage.TotalCostUSD
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
			continue
		}
		v.Reported++
		sum += c
	}
	if v.Reported > 0 && !math.IsInf(sum, 0) {
		v.USD = &sum
	}
	v.Partial = v.Reported < v.Attempts
	return v
}

// ledgerETag fingerprints the names, sizes and modification times of the Lab's
// ledgers and delivery receipts, reading no file contents. It is bounded by the
// shared raw-entry cap; a directory beyond it, or an entry modified within
// racyWindow, changes the tag on every call rather than hiding a change.
func ledgerETag(lab string) string {
	h := sha256.New()
	// The Insights "this week" tiles change when the week turns, so the week is
	// part of the tag and a 304 never serves last week's totals.
	fmt.Fprintf(h, "week %d\x00", weekStart(analyticsNow().UnixMilli()))
	for _, sub := range []string{"ledgers", "deliveries"} {
		fmt.Fprintf(h, "%s\x00", sub)
		dir := filepath.Join(lab, sub)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(h, "absent\x00")
			continue
		}
		f, err := os.Open(dir)
		if err != nil {
			fmt.Fprintf(h, "unreadable\x00")
			continue
		}
		var entries []os.FileInfo
		for len(entries) <= maxETagEntries {
			batch, err := f.Readdir(512)
			entries = append(entries, batch...)
			if err == io.EOF || len(batch) == 0 {
				break
			}
			if err != nil {
				fmt.Fprintf(h, "unreadable\x00")
				break
			}
		}
		f.Close()
		if len(entries) > maxETagEntries {
			fmt.Fprintf(h, "truncated %d\x00", time.Now().UnixNano())
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", e.Name(), e.Size(), e.ModTime().UnixNano(), e.Mode())
			// A coarse clock can give a rewrite of the same size the same stamp, so
			// while any entry is that fresh the tag changes on every call.
			if time.Since(e.ModTime()) < racyWindow {
				fmt.Fprintf(h, "racy %d\x00", time.Now().UnixNano())
			}
		}
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:32] + `"`
}

const maxETagEntries = 65536
