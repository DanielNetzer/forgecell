package molecule

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Failure classes are pure functions of a finished Record. They read existing
// ledger fields only; none of them is stored.
const (
	FailureProcessTimedOut           = "process-timed-out"
	FailureAnalysisPolicyViolation   = "analysis-policy-violation"
	FailureProtectedRegressionFailed = "protected-regression-failed"
)

// FailureClasses lists the classes in the order they are evaluated.
var FailureClasses = []string{FailureProcessTimedOut, FailureAnalysisPolicyViolation, FailureProtectedRegressionFailed}

// ValidFailureClass reports whether class is one of the enumerated classes.
func ValidFailureClass(class string) bool {
	for _, c := range FailureClasses {
		if c == class {
			return true
		}
	}
	return false
}

// HasFailureClass reports whether the finished Record exhibits class.
func HasFailureClass(r Record, class string) bool {
	if r.FinishedAt == "" {
		return false
	}
	switch class {
	case FailureProcessTimedOut:
		return failureClassTimedOut(r)
	case FailureAnalysisPolicyViolation:
		return AnalysisPolicyRejectionClass(r) != ""
	case FailureProtectedRegressionFailed:
		return failureClassProtectedRegression(r)
	}
	return false
}

// FailureClassesOf returns every class the finished Record exhibits.
func FailureClassesOf(r Record) []string {
	var out []string
	for _, class := range FailureClasses {
		if HasFailureClass(r, class) {
			out = append(out, class)
		}
	}
	return out
}

// failureClassTimedOut is true when the Molecule's final coding attempt hit its
// timeout and verification did not pass afterwards. A timeout that a later attempt
// or a passing verification superseded is not the Molecule's outcome. Analysis and
// review attempts never count, whatever their diagnostics say.
func failureClassTimedOut(r Record) bool {
	if len(r.HarnessAttempts) == 0 || !r.HarnessAttempts[len(r.HarnessAttempts)-1].Result.TimedOut {
		return false
	}
	v := r.Verification
	return v == nil || !v.RequiredChecksPassed || v.Error != "" || (v.Protected != nil && !v.Protected.RequiredChecksPassed)
}

const (
	retryAlsoRejectedSuffix = " (the corrective analysis retry was also rejected; no coding harness was invoked)"
	retryLimitMarker        = " corrective analysis retries was reached; no coding harness was invoked)"
)

// AnalysisPolicyRejectionClass returns the readiness policy class (see
// readiness.PolicyClass) of the rejection that stopped intake of a finished
// Record, or "" when intake did not stop on a policy rejection. A policy
// rejection stores the rejected response on its failed attempt, and intake only
// stops on one after the corrective retry was also rejected or the retry limit
// was reached; both stops put their reason last in Notes.
func AnalysisPolicyRejectionClass(r Record) string {
	if r.FinishedAt == "" || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 || len(r.AnalysisAttempts) == 0 || len(r.Notes) == 0 {
		return ""
	}
	last := r.AnalysisAttempts[len(r.AnalysisAttempts)-1]
	if last.Status != "failed" || last.Response == nil || last.Error == "" {
		return ""
	}
	note := r.Notes[len(r.Notes)-1]
	if !strings.HasPrefix(note, last.Error) || !(strings.HasSuffix(note, retryAlsoRejectedSuffix) || (strings.Contains(note, "the limit of ") && strings.HasSuffix(note, retryLimitMarker))) {
		return ""
	}
	return readiness.PolicyClass(&readiness.PolicyViolation{Reason: last.Error})
}

// failureClassProtectedRegression is true when verification ran the protected view
// and its required checks did not pass.
func failureClassProtectedRegression(r Record) bool {
	return r.Verification != nil && r.Verification.Protected != nil && !r.Verification.Protected.RequiredChecksPassed
}

const (
	recentLedgerWindow   = 128
	recentLedgerBytes    = 5_000_000
	recentLedgerTotal    = 40_000_000
	recentLedgerEntryCap = 4096
)

// ReadRecentFinished returns up to recentLedgerWindow of the most recently
// modified finished ticket Molecules in the Lab, ordered by finish time and then
// ID. Unfinished Molecules are omitted, and files that are not ledgers are
// ignored. A ledger that cannot be read or decoded is skipped on its own and
// listed in the second result ("name: reason"), so one damaged file never hides
// the other Molecules from evaluation. A damaged directory is an error.
func ReadRecentFinished(lab string) ([]Record, []string, error) {
	dirPath := filepath.Join(lab, "ledgers")
	before, err := os.Lstat(dirPath)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() {
		return nil, nil, fmt.Errorf("ledger directory is not a regular directory")
	}
	dir, err := os.OpenFile(dirPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, nil, fmt.Errorf("ledger directory identity changed")
	}
	names, _, err := readLedgerNames(dir)
	if err != nil {
		return nil, nil, err
	}
	if len(names) > recentLedgerEntryCap {
		return nil, nil, fmt.Errorf("ledger directory holds more than %d entries", recentLedgerEntryCap)
	}
	var skipped []string
	type candidate struct {
		name string
		at   time.Time
	}
	var files []candidate
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		info, e := os.Lstat(filepath.Join(dirPath, name))
		if e != nil {
			skipped = append(skipped, name+": "+e.Error())
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, candidate{name, info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].at.Equal(files[j].at) {
			return files[i].name > files[j].name
		}
		return files[i].at.After(files[j].at)
	})
	if len(files) > recentLedgerWindow {
		files = files[:recentLedgerWindow]
	}
	var records []Record
	total := int64(0)
	for _, f := range files {
		path := filepath.Join(dirPath, f.name)
		raw, e := readBoundedLedger(path)
		if e != nil {
			skipped = append(skipped, f.name+": "+e.Error())
			continue
		}
		total += int64(len(raw))
		if total > recentLedgerTotal {
			skipped = append(skipped, fmt.Sprintf("%s: recent ledgers exceed %d bytes", f.name, recentLedgerTotal))
			continue
		}
		header, e := ledger.Decode(raw)
		if e != nil || header.ID+".json" != f.name {
			continue
		}
		var r Record
		if e = json.Unmarshal(raw, &r); e != nil {
			skipped = append(skipped, f.name+": "+e.Error())
			continue
		}
		if r.FinishedAt == "" || r.Mode != "ticket" {
			continue
		}
		if _, e = time.Parse(time.RFC3339Nano, r.FinishedAt); e != nil {
			skipped = append(skipped, f.name+": invalid finish time")
			continue
		}
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, records[i].FinishedAt)
		b, _ := time.Parse(time.RFC3339Nano, records[j].FinishedAt)
		if a.Equal(b) {
			return records[i].ID < records[j].ID
		}
		return a.Before(b)
	})
	return records, skipped, nil
}

func readBoundedLedger(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > recentLedgerBytes {
		return nil, fmt.Errorf("ledger must be a bounded regular file: %s", filepath.Base(path))
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, recentLedgerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > recentLedgerBytes {
		return nil, fmt.Errorf("ledger exceeds %d bytes: %s", recentLedgerBytes, filepath.Base(path))
	}
	return raw, nil
}
