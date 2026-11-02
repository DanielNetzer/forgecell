package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Bounds of the findings record's dismissals.
const (
	// MaxDismissals bounds the dismissals of one findings record, one per
	// fingerprint.
	MaxDismissals = 500
	// MaxDismissalReasonRunes bounds a dismissal's reason.
	MaxDismissalReasonRunes = 500
	// maxDismissalReviewerBytes bounds a dismissal's reviewer identity.
	maxDismissalReviewerBytes = 300
	// maxDismissalTimeBytes bounds a dismissal's timestamp.
	maxDismissalTimeBytes = 64
)

// dismissalReviewer is a reviewer identity as ReviewerKey forms it: a Formula
// index, a colon and a plugin name.
var dismissalReviewer = regexp.MustCompile(`^[0-9]{1,3}:[^\x00]+$`)

// Dismissal is one finding an operator dismissed, with the reviewer identity
// whose finding it is, the operator's plain-text reason and when.
type Dismissal struct {
	Fingerprint string `json:"fingerprint"`
	Reviewer    string `json:"reviewer"`
	Reason      string `json:"reason"`
	At          string `json:"at"`
}

// DismissalReason is the trimmed reason, or an error when it is empty, longer
// than MaxDismissalReasonRunes, or holds anything safetext would change: a
// control, format or separator character, or invalid UTF-8.
func DismissalReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", fmt.Errorf("a dismissal needs a reason")
	}
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > MaxDismissalReasonRunes {
		return "", fmt.Errorf("a dismissal reason is at most %d characters of plain text", MaxDismissalReasonRunes)
	}
	if safetext.Text(reason, MaxDismissalReasonRunes) != reason {
		return "", fmt.Errorf("a dismissal reason is one line of plain text without control or format characters")
	}
	return reason, nil
}

// DismissedSet is the record's dismissed fingerprints.
func (r FindingsRecord) DismissedSet() map[string]bool {
	set := make(map[string]bool, len(r.Dismissed))
	for _, d := range r.Dismissed {
		set[d.Fingerprint] = true
	}
	return set
}

// DismissFinding records the operator's dismissal of one finding of a recorded
// round, with its reason, under the delivery lock and with the record's
// symlink-safe atomic write. It refuses an invalid id or fingerprint, a reason
// DismissalReason refuses, a fingerprint in no recorded round, one already
// dismissed, a record that holds MaxDismissals, and a demo Lab. A refusal
// writes nothing. now defaults to time.Now.
func DismissFinding(lab, id, fingerprint, reason string, now func() time.Time) error {
	if err := RefuseDemoLab(lab); err != nil {
		return err
	}
	if !safeID.MatchString(id) {
		return fmt.Errorf("invalid Molecule id")
	}
	if !fingerprintHex.MatchString(fingerprint) {
		return fmt.Errorf("a finding fingerprint is 64 lower-case hex characters")
	}
	reason, err := DismissalReason(reason)
	if err != nil {
		return err
	}
	if now == nil {
		now = time.Now
	}
	unlock, err := lockFindings(lab, "dismissal")
	if err != nil {
		return err
	}
	defer unlock()
	record, err := ReadFindings(lab, id)
	if err != nil {
		return err
	}
	reviewerKey, found := recordedReviewer(record, fingerprint)
	if !found {
		return fmt.Errorf("finding %s is not in any recorded findings round", fingerprint)
	}
	if record.DismissedSet()[fingerprint] {
		return fmt.Errorf("finding %s is already dismissed", fingerprint)
	}
	if len(record.Dismissed) >= MaxDismissals {
		return fmt.Errorf("the findings record holds at most %d dismissals; undismiss one first", MaxDismissals)
	}
	record.Dismissed = append(record.Dismissed, Dismissal{Fingerprint: fingerprint, Reviewer: reviewerKey, Reason: reason, At: now().UTC().Format(time.RFC3339)})
	if err := checkFindings(id, record); err != nil {
		return err
	}
	return writeDismissals(lab, id, record)
}

// writeDismissals writes the record with writeFindings, reporting a file
// system failure without its path.
func writeDismissals(lab, id string, record FindingsRecord) error {
	err := writeFindings(lab, id, record)
	var pathErr *os.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
		return fmt.Errorf("the findings record could not be written")
	}
	return err
}

// UndismissFinding removes the dismissal of one finding under the delivery
// lock. It refuses a demo Lab, an invalid id or fingerprint and a finding that
// is not dismissed. A record left without dismissals is written exactly as one
// that never had any.
func UndismissFinding(lab, id, fingerprint string) error {
	if err := RefuseDemoLab(lab); err != nil {
		return err
	}
	if !safeID.MatchString(id) {
		return fmt.Errorf("invalid Molecule id")
	}
	if !fingerprintHex.MatchString(fingerprint) {
		return fmt.Errorf("a finding fingerprint is 64 lower-case hex characters")
	}
	unlock, err := lockFindings(lab, "dismissal")
	if err != nil {
		return err
	}
	defer unlock()
	record, err := ReadFindings(lab, id)
	if err != nil {
		return err
	}
	kept := make([]Dismissal, 0, len(record.Dismissed))
	for _, d := range record.Dismissed {
		if d.Fingerprint != fingerprint {
			kept = append(kept, d)
		}
	}
	if len(kept) == len(record.Dismissed) {
		return fmt.Errorf("finding %s is not dismissed", fingerprint)
	}
	record.Dismissed = kept
	if len(kept) == 0 {
		record.Dismissed = nil
	}
	return writeDismissals(lab, id, record)
}

// lockFindings takes the delivery lock Collect and PushFixRound take, so a
// dismissal refuses while another delivery operation runs.
func lockFindings(lab, operation string) (func(), error) {
	lock := filepath.Join(lab, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, fmt.Errorf("delivery/findings %s conflict: another delivery operation holds the lock", operation)
	}
	return func() { os.Remove(lock) }, nil
}

// recordedReviewer is the reviewer identity of the fingerprint's finding in
// any recorded round.
func recordedReviewer(record FindingsRecord, fingerprint string) (string, bool) {
	for i := len(record.Rounds) - 1; i >= 0; i-- {
		for _, entry := range record.Rounds[i].Reviewers {
			for _, f := range entry.Findings {
				if f.Fingerprint == fingerprint {
					return entry.Reviewer, true
				}
			}
		}
	}
	return "", false
}

// checkDismissals validates the record's dismissals: at most MaxDismissals,
// one per fingerprint, each with a reviewer identity, a reason DismissalReason
// accepts unchanged and an RFC 3339 time. A dismissal whose finding a bound
// dropped from every round stays valid; it no longer matches anything.
func checkDismissals(dismissed []Dismissal) bool {
	if len(dismissed) > MaxDismissals {
		return false
	}
	seen := make(map[string]bool, len(dismissed))
	for _, d := range dismissed {
		if !fingerprintHex.MatchString(d.Fingerprint) || seen[d.Fingerprint] {
			return false
		}
		seen[d.Fingerprint] = true
		if len(d.Reviewer) > maxDismissalReviewerBytes || !dismissalReviewer.MatchString(d.Reviewer) || safetext.Text(d.Reviewer, maxDismissalReviewerBytes) != d.Reviewer {
			return false
		}
		if reason, err := DismissalReason(d.Reason); err != nil || reason != d.Reason {
			return false
		}
		if len(d.At) > maxDismissalTimeBytes {
			return false
		}
		if _, err := time.Parse(time.RFC3339, d.At); err != nil {
			return false
		}
	}
	return true
}
