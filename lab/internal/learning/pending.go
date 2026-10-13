package learning

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// PendingOptions selects the Lab and the directory Create inspects for Claude
// project configuration, exactly as for `forgecell learn`.
type PendingOptions struct {
	LabDir, Dir string
	// MaxAttempts and RetryAfter, when positive, make RunPending skip a request
	// that already failed that many times or failed less than RetryAfter ago. A
	// skipped request stays pending and is reported with Skipped set. The zero
	// value runs every pending request, as `forgecell learn --pending` does.
	MaxAttempts int
	RetryAfter  time.Duration
}

// throttled says why a request must wait for a person or for the cooldown.
func (o PendingOptions) throttled(r Request) string {
	if o.MaxAttempts > 0 && r.Attempts >= o.MaxAttempts {
		return fmt.Sprintf("waiting for operator: %d failed attempts", r.Attempts)
	}
	if o.RetryAfter > 0 && r.LastFailureAt != "" {
		if at, err := parseTime(r.LastFailureAt); err == nil && clock().Sub(at) < o.RetryAfter {
			return fmt.Sprintf("retrying after %s: the last of %d failed attempt(s) was under %s ago", at.Add(o.RetryAfter).Format(time.RFC3339), r.Attempts, o.RetryAfter)
		}
	}
	return ""
}

// PendingResult reports one request. Err is a bounded, redacted message.
type PendingResult struct {
	RequestID    string   `json:"requestId"`
	Class        string   `json:"class"`
	MoleculeIDs  []string `json:"moleculeIds"`
	SuggestionID string   `json:"suggestionId,omitempty"`
	Err          string   `json:"error,omitempty"`
	Skipped      string   `json:"skipped,omitempty"`
}

type invokeFunc = func(context.Context, []string, string, time.Duration, map[string]any) process.Result

// RunPending runs each pending learn request through Create over exactly the
// request's Molecules. Success saves a pending suggestion and nothing else:
// approval stays a separate human step. A failure leaves the request pending.
func RunPending(ctx context.Context, o PendingOptions) ([]PendingResult, error) {
	return runPending(ctx, o, harness.InvokeBinding)
}

func runPending(ctx context.Context, o PendingOptions, invoke invokeFunc) ([]PendingResult, error) {
	loaded, err := activeFormula(o.LabDir)
	if err != nil {
		return nil, err
	}
	unlock, err := lockRequests(o.LabDir)
	if err != nil {
		return nil, err
	}
	requests, _, err := readRequests(o.LabDir)
	if err == nil {
		err = markStale(o.LabDir, requests, loaded.Formula)
	}
	if err == nil {
		pruneRequests(o.LabDir, requests)
	}
	unlock()
	if err != nil {
		return nil, err
	}
	var results []PendingResult
	for _, r := range requests {
		if r.Status != requestPending || r.FormulaSHA256 != loaded.Formula.SHA256 || r.FormulaID != loaded.Formula.ID {
			continue
		}
		result := PendingResult{RequestID: r.ID, Class: r.Class, MoleculeIDs: r.MoleculeIDs}
		if why := o.throttled(r); why != "" {
			result.Skipped = why
			results = append(results, result)
			continue
		}
		release, e := lockRun(o.LabDir, r.ID)
		if e != nil {
			result.Err = "another learn run holds this request; it stays pending"
			results = append(results, result)
			continue
		}
		result.SuggestionID, result.Err = runRequest(ctx, o, r.ID, invoke)
		release()
		if result.SuggestionID == "" && result.Err == "" {
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

func lockRun(lab, id string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(requestsDir(lab), id+".run-lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

func loadRequest(lab, id string) (Request, []Request, error) {
	requests, _, err := readRequests(lab)
	if err != nil {
		return Request{}, nil, err
	}
	for _, r := range requests {
		if r.ID == id {
			return r, requests, nil
		}
	}
	return Request{}, nil, fmt.Errorf("learn request %s disappeared or is unreadable", id)
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// savedSuggestionFor finds the suggestion a run that died after Create saved it:
// a suggestion of the request's Formula over exactly the request's Molecules that
// no other request owns and that was created after the run's intent was written.
func savedSuggestionFor(lab string, r Request, all []Request) string {
	started, err := parseTime(r.RunStartedAt)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(lab, "suggestions"))
	if err != nil || len(entries) > maxRequestEntries {
		return ""
	}
	owned := map[string]bool{}
	for _, other := range all {
		if other.ID != r.ID && other.SuggestionID != "" {
			owned[other.SuggestionID] = true
		}
	}
	want := slices.Clone(r.MoleculeIDs)
	slices.Sort(want)
	var found, foundAt string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !validID.MatchString(id) || owned[id] {
			continue
		}
		raw, err := bounded(filepath.Join(lab, "suggestions", e.Name()))
		if err != nil {
			continue
		}
		var s struct {
			ID           string   `json:"id"`
			FormulaID    string   `json:"formulaId"`
			OriginalHash string   `json:"originalHash"`
			MoleculeIDs  []string `json:"moleculeIds"`
			CreatedAt    string   `json:"createdAt"`
		}
		if json.Unmarshal(raw, &s) != nil || s.ID != id || s.FormulaID != r.FormulaID || s.OriginalHash != r.FormulaSHA256 {
			continue
		}
		created, err := parseTime(s.CreatedAt)
		if err != nil || created.Before(started) {
			continue
		}
		have := slices.Clone(s.MoleculeIDs)
		slices.Sort(have)
		if !slices.Equal(have, want) {
			continue
		}
		if found == "" || s.CreatedAt < foundAt {
			found, foundAt = id, s.CreatedAt
		}
	}
	return found
}

// runRequest returns the saved suggestion id or a bounded failure message. Both
// are empty when another run already finished the request.
//
// A durable intent (nonce and start time) is written before the learn command
// starts. If the process dies before the outcome is recorded, the next run finds
// the intent, adopts the suggestion Create already saved, and calls the model
// again only when there is none.
func runRequest(ctx context.Context, o PendingOptions, id string, invoke invokeFunc) (string, string) {
	unlock, err := lockRequests(o.LabDir)
	if err != nil {
		return "", boundedMessage("learn request could not be started: " + err.Error())
	}
	r, all, err := loadRequest(o.LabDir, id)
	if err != nil || r.Status != requestPending {
		unlock()
		if err != nil {
			return "", boundedMessage(err.Error())
		}
		return "", ""
	}
	if r.RunNonce != "" {
		if saved := savedSuggestionFor(o.LabDir, r, all); saved != "" {
			r.Status, r.SuggestionID = requestSuggested, saved
			r.LastFailure, r.LastFailureAt, r.RunNonce, r.RunStartedAt = "", "", "", ""
			err = saveRequest(o.LabDir, r)
			unlock()
			if err != nil {
				return saved, boundedMessage(fmt.Sprintf("learn request could not be updated (suggestion %s was saved): %v", saved, err))
			}
			return saved, ""
		}
	}
	nonce, err := newNonce()
	if err == nil {
		r.RunNonce, r.RunStartedAt = nonce, stamp()
		err = saveRequest(o.LabDir, r)
	}
	unlock()
	if err != nil {
		return "", boundedMessage("learn request could not be started: " + err.Error())
	}
	p, runErr := create(ctx, Options{LabDir: o.LabDir, Dir: o.Dir, MoleculeIDs: r.MoleculeIDs}, invoke)
	saved := p.ID
	if runErr != nil {
		saved = ""
	}
	unlock, err = lockRequests(o.LabDir)
	if err != nil {
		return saved, boundedMessage("learn request could not be updated: " + err.Error())
	}
	defer unlock()
	current, _, err := loadRequest(o.LabDir, id)
	if err != nil {
		return saved, boundedMessage(err.Error())
	}
	current.RunNonce, current.RunStartedAt = "", ""
	// A run the caller cancelled (a stop signal) did not fail: it is not an attempt.
	interrupted := runErr != nil && ctx.Err() != nil
	switch {
	case interrupted:
	case runErr != nil:
		current.Attempts++
		current.LastFailure = boundedMessage(runErr.Error())
		current.LastFailureAt = clock().Format(time.RFC3339Nano)
	default:
		current.Attempts++
		current.Status, current.SuggestionID, current.LastFailure, current.LastFailureAt = requestSuggested, p.ID, "", ""
	}
	if err = saveRequest(o.LabDir, current); err != nil {
		return saved, boundedMessage(fmt.Sprintf("learn request could not be updated (suggestion %s was saved): %v", p.ID, err))
	}
	if interrupted {
		return "", "learn run interrupted; the request stays pending"
	}
	if runErr != nil {
		return "", current.LastFailure
	}
	return p.ID, ""
}

// DisplayText makes untrusted text safe to print on one terminal line: invalid
// UTF-8 is dropped, terminal escape sequences, controls, format characters (such
// as bidirectional overrides), line and paragraph separators become spaces,
// whitespace collapses, and the result is cut to limit runes, never bytes.
func DisplayText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, s)
	return harness.ReviewDisplayText(s, limit)
}

// boundedMessage redacts secrets and makes untrusted failure text one display-safe line.
func boundedMessage(s string) string {
	s = DisplayText(harness.RedactSecrets(s), 300)
	if s == "" {
		return "learn run failed"
	}
	return s
}
