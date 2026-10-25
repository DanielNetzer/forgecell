package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// A learn request is the durable, deduplicated record that a recurrence rule
// fired. It never contains model output and evaluating rules never calls a model:
// only RunPending does, through the same Create path as `forgecell learn`.

const (
	requestPending    = "pending"
	requestSuggested  = "suggested"
	requestSuperseded = "superseded"
	requestStale      = "stale"

	maxRequestEntries   = 4096
	maxRequestBytes     = 64 * 1024
	maxRequestMolecules = 50
)

// ErrNoActiveFormula means the Lab has no active Formula, so nothing can recur.
var ErrNoActiveFormula = errors.New("the Lab has no active Formula")

var requestID = regexp.MustCompile(`^learn-request-[a-f0-9]{16}$`)
var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)
var runNonce = regexp.MustCompile(`^[a-f0-9]{32}$`)

// clock is a seam so tests use fixed timestamps.
var clock = func() time.Time { return time.Now().UTC() }

type Request struct {
	SchemaVersion string   `json:"schemaVersion"`
	ID            string   `json:"id"`
	Class         string   `json:"class"`
	FormulaID     string   `json:"formulaId"`
	FormulaSHA256 string   `json:"formulaSha256"`
	MoleculeIDs   []string `json:"moleculeIds"`
	Threshold     int      `json:"threshold"`
	CreatedAt     string   `json:"createdAt"`
	Status        string   `json:"status"`
	SuggestionID  string   `json:"suggestionId,omitempty"`
	Attempts      int      `json:"attempts,omitempty"`
	LastFailure   string   `json:"lastFailure,omitempty"`
	LastFailureAt string   `json:"lastFailureAt,omitempty"`
	// RunNonce and RunStartedAt are a durable intent written before the learn
	// command starts. A request that still carries one belongs to a run that never
	// recorded its outcome (a dead process), which `learn --pending` reconciles.
	RunNonce     string `json:"runNonce,omitempty"`
	RunStartedAt string `json:"runStartedAt,omitempty"`
}

// Recurrence is the outcome of one evaluation.
type Recurrence struct {
	// Created lists requests this evaluation wrote.
	Created []Request `json:"created,omitempty"`
	// Recommendations holds one line per firing rule when no learn Atom is bound.
	Recommendations []string `json:"recommendations,omitempty"`
	// Skipped names ledgers, requests or suggestions that could not be read and
	// were left out; the rest of the evaluation went ahead. Text is display-safe.
	Skipped []string `json:"skipped,omitempty"`
}

const maxSkippedReported = 5

// boundSkipped makes untrusted file names and reasons display-safe and keeps the
// list short.
func boundSkipped(skipped []string) []string {
	var out []string
	for i, s := range skipped {
		if i == maxSkippedReported {
			out = append(out, fmt.Sprintf("and %d more", len(skipped)-maxSkippedReported))
			break
		}
		out = append(out, DisplayText(s, 200))
	}
	return out
}

func requestsDir(lab string) string { return filepath.Join(lab, "learn-requests") }

func lockRequests(lab string) (func(), error) {
	dir := requestsDir(lab)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

func validateRequest(name string, r Request) error {
	if r.SchemaVersion != "v1" || !requestID.MatchString(r.ID) || r.ID+".json" != name {
		return fmt.Errorf("%s: not a learn request", name)
	}
	if !molecule.ValidFailureClass(r.Class) || !sha256Hex.MatchString(r.FormulaSHA256) || r.FormulaID == "" || strings.ContainsAny(r.FormulaID, "/\\\x00") {
		return fmt.Errorf("%s: invalid class or Formula identity", name)
	}
	if len(r.MoleculeIDs) == 0 || len(r.MoleculeIDs) > maxRequestMolecules || r.Threshold < 1 {
		return fmt.Errorf("%s: invalid Molecule evidence", name)
	}
	for _, id := range r.MoleculeIDs {
		if !validID.MatchString(id) {
			return fmt.Errorf("%s: invalid Molecule id", name)
		}
	}
	switch r.Status {
	case requestPending, requestSuggested, requestSuperseded, requestStale:
	default:
		return fmt.Errorf("%s: unknown status", name)
	}
	if r.SuggestionID != "" && !validID.MatchString(r.SuggestionID) || r.Status == requestSuggested && r.SuggestionID == "" {
		return fmt.Errorf("%s: invalid suggestion id", name)
	}
	if _, err := parseTime(r.CreatedAt); err != nil {
		return fmt.Errorf("%s: invalid creation time", name)
	}
	if r.LastFailureAt != "" {
		if _, err := parseTime(r.LastFailureAt); err != nil {
			return fmt.Errorf("%s: invalid failure time", name)
		}
	}
	if r.RunNonce != "" || r.RunStartedAt != "" {
		if !runNonce.MatchString(r.RunNonce) {
			return fmt.Errorf("%s: invalid run intent", name)
		}
		if _, err := parseTime(r.RunStartedAt); err != nil {
			return fmt.Errorf("%s: invalid run intent time", name)
		}
	}
	return nil
}

// readRequests returns every readable request. A request file that cannot be read
// or validated is skipped on its own and described in the second result, so one
// damaged file never hides the others. A damaged directory is an error.
func readRequests(lab string) ([]Request, []string, error) {
	dir := requestsDir(lab)
	before, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() {
		return nil, nil, fmt.Errorf("learn-requests is not a directory")
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	names, err := f.Readdirnames(maxRequestEntries + 1)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	if len(names) > maxRequestEntries {
		return nil, nil, fmt.Errorf("learn-requests holds more than %d entries", maxRequestEntries)
	}
	sort.Strings(names)
	var out []Request
	var skipped []string
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		info, e := os.Lstat(path)
		if e != nil {
			skipped = append(skipped, "learn request "+name+": "+e.Error())
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > maxRequestBytes {
			skipped = append(skipped, "learn request "+name+": expected a bounded regular file")
			continue
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			skipped = append(skipped, "learn request "+name+": "+e.Error())
			continue
		}
		var r Request
		if e = json.Unmarshal(raw, &r); e != nil {
			skipped = append(skipped, "learn request "+name+": "+e.Error())
			continue
		}
		if e = validateRequest(name, r); e != nil {
			skipped = append(skipped, "learn request "+e.Error())
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID < out[j].ID
		}
		a, _ := time.Parse(time.RFC3339Nano, out[i].CreatedAt)
		b, _ := time.Parse(time.RFC3339Nano, out[j].CreatedAt)
		if a.Equal(b) {
			return out[i].ID < out[j].ID
		}
		return a.Before(b)
	})
	return out, skipped, nil
}

func saveRequest(lab string, r Request) error {
	if err := validateRequest(r.ID+".json", r); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return write(filepath.Join(requestsDir(lab), r.ID+".json"), append(b, '\n'))
}

// suggestionStatus reads only what a rule needs from a saved suggestion.
func suggestionStatus(lab, id string) (status, reviewedAt string, err error) {
	if !validID.MatchString(id) {
		return "", "", fmt.Errorf("invalid suggestion id")
	}
	raw, err := bounded(filepath.Join(lab, "suggestions", id+".json"))
	if err != nil {
		return "", "", fmt.Errorf("suggestion %s: %w", id, err)
	}
	var s struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		ReviewedAt string `json:"reviewedAt"`
	}
	if err = json.Unmarshal(raw, &s); err != nil || s.ID != id {
		return "", "", fmt.Errorf("suggestion %s is unreadable", id)
	}
	return s.Status, s.ReviewedAt, nil
}

func activeFormula(lab string) (formula.Loaded, error) {
	active, err := formula.Inspect(lab, "")
	if os.IsNotExist(err) {
		return formula.Loaded{}, ErrNoActiveFormula
	}
	if err != nil {
		return formula.Loaded{}, err
	}
	if active.Formula.ID == "" {
		return formula.Loaded{}, ErrNoActiveFormula
	}
	loaded, err := formula.Load(lab, active.Formula.ID)
	if err != nil {
		return formula.Loaded{}, err
	}
	if !loaded.Approved {
		return formula.Loaded{}, fmt.Errorf("Formula %s is not currently approved", active.Formula.ID)
	}
	return loaded, nil
}

// learnBound reports whether a learn Atom with a resolvable command exists, which
// is exactly what Create needs to run.
func learnBound(f formula.Formula) bool {
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			_, _, _, err := learnInvocation(f, a)
			return err == nil
		}
	}
	return false
}

// eligibleForLearning repeats the preconditions Create enforces on each Molecule,
// so a request can always be run.
func eligibleForLearning(lab string, r molecule.Record) bool {
	if r.SchemaVersion != "v0" || r.Kind != "molecule" || r.Mode != "ticket" || r.FinishedAt == "" || !r.FormulaApproved || !validID.MatchString(r.ID) || hash(r.FormulaSnapshot.YAML) != r.FormulaSnapshot.SHA256 {
		return false
	}
	_, err := classifyEvidence(lab, r)
	return err == nil
}

// trailingStreak returns the newest consecutive Molecules, oldest first, that
// exhibit class. An ineligible Molecule ends the streak.
func trailingStreak(lab string, sequence []molecule.Record, class string) []molecule.Record {
	var run []molecule.Record
	for i := len(sequence) - 1; i >= 0; i-- {
		r := sequence[i]
		if !molecule.HasFailureClass(r, class) || !eligibleForLearning(lab, r) {
			break
		}
		run = append(run, r)
	}
	slices.Reverse(run)
	return run
}

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// markStale retires requests that belong to another Formula snapshot. A Formula
// change resets every counter.
func markStale(lab string, requests []Request, f formula.Formula) error {
	for i, r := range requests {
		if (r.Status == requestPending || r.Status == requestSuggested) && (r.FormulaSHA256 != f.SHA256 || r.FormulaID != f.ID) {
			requests[i].Status = requestStale
			if err := saveRequest(lab, requests[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// classHistory summarizes the requests of one class for the current Formula.
type classHistory struct {
	blocked bool
	// event is the latest dismissal or failed learn run; new occurrences count after it.
	event  time.Time
	failed []Request
}

func historyOf(lab string, requests []Request, class, sha string) (classHistory, error) {
	var h classHistory
	for _, r := range requests {
		if r.Class != class || r.FormulaSHA256 != sha {
			continue
		}
		var event string
		switch r.Status {
		case requestPending:
			if r.LastFailureAt == "" {
				h.blocked = true
				continue
			}
			event = r.LastFailureAt
			h.failed = append(h.failed, r)
		case requestSuperseded:
			event = r.LastFailureAt
		case requestSuggested:
			status, reviewed, err := suggestionStatus(lab, r.SuggestionID)
			if err != nil {
				return h, err
			}
			switch status {
			case "pending", "approved":
				h.blocked = true
			case "dismissed":
				event = reviewed
			default:
				return h, fmt.Errorf("suggestion %s has an unknown status", r.SuggestionID)
			}
		}
		if event == "" {
			continue
		}
		at, err := parseTime(event)
		if err != nil {
			return h, fmt.Errorf("learn request %s: invalid event time", r.ID)
		}
		if at.After(h.event) {
			h.event = at
		}
	}
	return h, nil
}

func newRequestID(class, sha string, ids []string) string {
	sum := sha256.Sum256([]byte(class + "\x00" + sha + "\x00" + strings.Join(ids, "\x00")))
	return "learn-request-" + hex.EncodeToString(sum[:8])
}

// EvaluateRecurrence applies the recurrence rules to the Lab's recent ledgers. A
// rule fires when the newest finished Molecules of the active Formula snapshot
// exhibit one failure class the Formula's threshold number of times in a row.
// With a bound learn Atom a firing rule writes one durable request; without one
// it only returns a recommendation. It never calls a model, and re-running it
// never duplicates a request.
func EvaluateRecurrence(lab string) (Recurrence, error) {
	var out Recurrence
	loaded, err := activeFormula(lab)
	if err != nil {
		return out, err
	}
	f := loaded.Formula
	threshold := f.RecurrenceThreshold()
	recent, err := molecule.ScanRecentFinished(lab)
	if err != nil {
		return out, err
	}
	skipped := recent.Skipped
	if recent.Incomplete != "" {
		// First, so boundSkipped never hides it behind per-ledger entries. Only the
		// oldest ledgers are missing, so a streak can be shortened, never invented.
		skipped = append([]string{recent.Incomplete}, skipped...)
	}
	var sequence []molecule.Record
	for _, r := range recent.Records {
		if r.FormulaID == f.ID && r.FormulaSnapshot.SHA256 == f.SHA256 {
			sequence = append(sequence, r)
		}
	}
	streaks := map[string][]molecule.Record{}
	for _, class := range molecule.FailureClasses {
		if s := trailingStreak(lab, sequence, class); len(s) >= threshold {
			streaks[class] = s
		}
	}
	if len(streaks) == 0 {
		out.Skipped = boundSkipped(skipped)
		return out, nil
	}
	bound := learnBound(f)
	var requests []Request
	// A Lab that never queued a request and has no learn Atom gains no state.
	if _, statErr := os.Lstat(requestsDir(lab)); bound || statErr == nil {
		unlock, err := lockRequests(lab)
		if err != nil {
			return out, err
		}
		defer unlock()
		var skippedRequests []string
		if requests, skippedRequests, err = readRequests(lab); err != nil {
			return out, err
		}
		skipped = append(skipped, skippedRequests...)
		if err = markStale(lab, requests, f); err != nil {
			return out, err
		}
		pruneRequests(lab, requests)
	}
	for _, class := range molecule.FailureClasses {
		streak, ok := streaks[class]
		if !ok {
			continue
		}
		history, err := historyOf(lab, requests, class, f.SHA256)
		if err != nil {
			// The class cannot be judged without knowing its suggestion, so it does
			// not fire; the other classes are still evaluated.
			skipped = append(skipped, fmt.Sprintf("class %s: %v", class, err))
			continue
		}
		if history.blocked {
			continue
		}
		var fresh []molecule.Record
		for _, r := range streak {
			at, e := parseTime(r.FinishedAt)
			if e != nil {
				skipped = append(skipped, fmt.Sprintf("Molecule %s: invalid finish time", r.ID))
				continue
			}
			if at.After(history.event) {
				fresh = append(fresh, r)
			}
		}
		if len(fresh) < threshold {
			continue
		}
		fresh = fresh[len(fresh)-threshold:]
		ids := make([]string, len(fresh))
		for i, r := range fresh {
			ids[i] = r.ID
		}
		if !bound {
			out.Recommendations = append(out.Recommendations, fmt.Sprintf("Recurring %s in %d consecutive Molecules (%s) but no learn Atom is bound, so no Formula improvement can be proposed; run `forgecell init --lab %s --meta-harness HARNESS_ID` to review a meta harness.", class, threshold, strings.Join(ids, ", "), formula.ShellWord(lab)))
			continue
		}
		request := Request{SchemaVersion: "v1", ID: newRequestID(class, f.SHA256, ids), Class: class, FormulaID: f.ID, FormulaSHA256: f.SHA256, MoleculeIDs: ids, Threshold: threshold, CreatedAt: clock().Format(time.RFC3339Nano), Status: requestPending}
		if slices.ContainsFunc(requests, func(r Request) bool { return r.ID == request.ID }) {
			continue
		}
		// Supersede first: a crash between the two writes leaves the class without a
		// pending request, never two, and the next evaluation queues the fresh one.
		for _, old := range history.failed {
			old.Status = requestSuperseded
			if err = saveRequest(lab, old); err != nil {
				return out, err
			}
		}
		if err = saveRequest(lab, request); err != nil {
			return out, err
		}
		out.Created = append(out.Created, request)
	}
	out.Skipped = boundSkipped(skipped)
	return out, nil
}

const maxTerminalRequests = 20

// pruneRequests keeps learn-requests/ bounded: it deletes the oldest terminal
// requests beyond maxTerminalRequests (stale, superseded, and suggested ones that a
// newer request of the same class and Formula replaced) and the run locks of
// requests that are no longer pending. It needs the requests lock and is
// best-effort; a file it cannot remove is retried next time.
func pruneRequests(lab string, requests []Request) {
	newest := map[string]string{}
	for _, r := range requests {
		newest[r.Class+"\x00"+r.FormulaSHA256] = r.ID
	}
	var terminal []Request
	pending := map[string]bool{}
	for _, r := range requests {
		switch {
		case r.Status == requestStale, r.Status == requestSuperseded, r.Status == requestSuggested && newest[r.Class+"\x00"+r.FormulaSHA256] != r.ID:
			terminal = append(terminal, r)
		case r.Status == requestPending:
			pending[r.ID] = true
		}
	}
	if len(terminal) > maxTerminalRequests {
		for _, r := range terminal[:len(terminal)-maxTerminalRequests] {
			os.Remove(filepath.Join(requestsDir(lab), r.ID+".json"))
		}
	}
	entries, err := os.ReadDir(requestsDir(lab))
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".run-lock")
		if !ok || pending[id] || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(requestsDir(lab), e.Name())
		file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			os.Remove(path)
		}
		file.Close()
	}
}

// PendingItem is a learn request, or a suggestion saved from one, that waits for a person.
type PendingItem struct {
	Kind          string   `json:"kind"`
	RequestID     string   `json:"requestId"`
	Class         string   `json:"class"`
	MoleculeIDs   []string `json:"moleculeIds"`
	FormulaSHA256 string   `json:"formulaSha256"`
	SuggestionID  string   `json:"suggestionId,omitempty"`
	Attempts      int      `json:"attempts,omitempty"`
	LastFailure   string   `json:"lastFailure,omitempty"`
}

// Pending lists what a person still has to act on for the active Formula
// snapshot: requests waiting for `forgecell learn --pending` and suggestions
// saved from them that are neither approved nor dismissed. It only reads. A
// request or suggestion it cannot read is left out and described once in the
// returned error, next to the items that could be read.
func Pending(lab string) ([]PendingItem, error) {
	requests, skipped, err := readRequests(lab)
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, skippedError(skipped)
	}
	active, err := formula.Inspect(lab, "")
	if err != nil || active.Formula.ID == "" {
		return nil, skippedError(skipped)
	}
	var items []PendingItem
	for _, r := range requests {
		if r.FormulaSHA256 != active.Formula.SHA256 || r.FormulaID != active.Formula.ID {
			continue
		}
		item := PendingItem{RequestID: r.ID, Class: r.Class, MoleculeIDs: r.MoleculeIDs, FormulaSHA256: r.FormulaSHA256, Attempts: r.Attempts}
		if r.LastFailure != "" {
			item.LastFailure = boundedMessage(r.LastFailure)
		}
		switch r.Status {
		case requestPending:
			item.Kind = "request"
		case requestSuggested:
			status, _, e := suggestionStatus(lab, r.SuggestionID)
			if e != nil {
				skipped = append(skipped, e.Error())
				continue
			}
			if status != "pending" {
				continue
			}
			item.Kind, item.SuggestionID, item.LastFailure = "suggestion", r.SuggestionID, ""
		default:
			continue
		}
		items = append(items, item)
	}
	return items, skippedError(skipped)
}

// skippedError describes unreadable items as one display-safe error, or nil.
func skippedError(skipped []string) error {
	if len(skipped) == 0 {
		return nil
	}
	return fmt.Errorf("skipped unreadable items: %s", strings.Join(boundSkipped(skipped), "; "))
}
