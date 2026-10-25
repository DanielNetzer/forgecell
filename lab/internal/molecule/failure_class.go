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
	recentLedgerWindow = 128
	// recentLedgerTotal bounds the bytes read by ReadRecentFinished and, in
	// ScanRecentFinished, the bytes of the fields kept from the recent ledgers, not
	// their whole size: fields the recurrence check never reads are skipped.
	recentLedgerTotal = 40_000_000
)

// RecentFinished is the recurrence view of a Lab's recent finished Molecules.
type RecentFinished struct {
	// Records are projections ordered by finish time and then ID. Each keeps only
	// the fields the failure classes and the learning eligibility check read; see
	// decodeRecent. They are never saved.
	Records []Record
	// Skipped lists ledgers left out on their own ("name: reason").
	Skipped []string
	// Incomplete is non-empty when the byte budget ran out. Records then holds the
	// newest ledgers that fit, so a trailing streak can only be shortened, never
	// formed across a gap.
	Incomplete string
}

const (
	recentLedgerBytes    = 5_000_000
	recentLedgerEntryCap = 4096
)

// ReadRecentFinished returns up to recentLedgerWindow of the most recently
// modified finished ticket Molecules in the Lab, ordered by finish time and then
// ID, as whole Records. Unfinished Molecules are omitted, and files that are not
// ledgers are ignored. A ledger that cannot be read or decoded is skipped on its
// own and listed in the second result ("name: reason"), so one damaged file never
// hides the other Molecules from evaluation. A damaged directory is an error. It
// reads whole ledgers within recentLedgerTotal bytes; the recurrence check uses
// ScanRecentFinished, which streams only the fields it needs.
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

// ScanRecentFinished returns up to recentLedgerWindow of the most recently
// modified finished ticket Molecules in the Lab. It chooses them from the shared
// header scan (ledger.ScanDir) and then streams only the needed fields of each,
// newest first, within recentLedgerTotal bytes of kept fields. Unfinished
// Molecules are omitted, and files that are not ledgers are ignored. A ledger that
// cannot be read or decoded is skipped on its own, so one damaged file never hides
// the other Molecules. A damaged or truncated directory scan is an error, since the
// newest Molecules could be among the files it did not read.
func ScanRecentFinished(lab string) (RecentFinished, error) {
	return readRecentFinished(lab, recentLedgerTotal)
}

func readRecentFinished(lab string, budget int64) (RecentFinished, error) {
	var out RecentFinished
	dirPath := filepath.Join(lab, "ledgers")
	scan := ledger.ScanDir(dirPath, ledger.ScanOptions{})
	if scan.Missing {
		return out, nil
	}
	if scan.Problem != "" {
		return out, fmt.Errorf("recent ledger scan stopped: %s", scan.Problem)
	}
	type candidate struct {
		name string
		at   time.Time
	}
	var files []candidate
	for _, f := range scan.Files {
		switch f.Kind {
		case ledger.ProblemNone:
		case ledger.ProblemMalformed, ledger.ProblemIdentity:
			// Not a Molecule ledger.
			continue
		default:
			out.Skipped = append(out.Skipped, f.Problem)
			continue
		}
		if f.Header.FinishedAt == "" || f.Header.Mode != "ticket" {
			continue
		}
		if _, e := time.Parse(time.RFC3339Nano, f.Header.FinishedAt); e != nil {
			out.Skipped = append(out.Skipped, f.Name+": invalid finish time")
			continue
		}
		info, e := os.Lstat(filepath.Join(dirPath, f.Name))
		if e != nil {
			out.Skipped = append(out.Skipped, f.Name+": "+e.Error())
			continue
		}
		files = append(files, candidate{f.Name, info.ModTime()})
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
	total := int64(0)
	for i, f := range files {
		r, kept, e := readRecentLedger(filepath.Join(dirPath, f.name))
		if e != nil {
			out.Skipped = append(out.Skipped, f.name+": "+e.Error())
			continue
		}
		if r.SchemaVersion != "v0" || r.Kind != "molecule" || r.ID+".json" != f.name || r.FinishedAt == "" || r.Mode != "ticket" {
			out.Skipped = append(out.Skipped, f.name+": ledger changed during the recurrence scan")
			continue
		}
		if _, e = time.Parse(time.RFC3339Nano, r.FinishedAt); e != nil {
			out.Skipped = append(out.Skipped, f.name+": invalid finish time")
			continue
		}
		if total+kept > budget {
			out.Incomplete = fmt.Sprintf("recurrence scan incomplete: the needed fields of recent ledgers exceed %d bytes; read the newest %d of %d, the %d older ones were not evaluated", budget, i, len(files), len(files)-i)
			break
		}
		total += kept
		out.Records = append(out.Records, r)
	}
	sort.Slice(out.Records, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, out.Records[i].FinishedAt)
		b, _ := time.Parse(time.RFC3339Nano, out.Records[j].FinishedAt)
		if a.Equal(b) {
			return out.Records[i].ID < out.Records[j].ID
		}
		return a.Before(b)
	})
	return out, nil
}

// readRecentLedger opens one ledger without following links, refuses a replaced
// or oversized file, and streams its recurrence fields.
func readRecentLedger(path string) (Record, int64, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return Record{}, 0, err
	}
	if !before.Mode().IsRegular() || before.Size() > ledger.MaxHeaderLedger {
		return Record{}, 0, fmt.Errorf("ledger must be a regular file of at most %d bytes", ledger.MaxHeaderLedger)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Record{}, 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return Record{}, 0, fmt.Errorf("opened file identity mismatch")
	}
	limited := &io.LimitedReader{R: f, N: ledger.MaxHeaderLedger + 1}
	r, kept, err := decodeRecent(limited)
	if limited.N <= 0 {
		return Record{}, 0, fmt.Errorf("ledger grew beyond %d bytes", ledger.MaxHeaderLedger)
	}
	return r, kept, err
}

// decodeRecent streams one ledger object and keeps only the top-level fields that
// HasFailureClass and the learning eligibility check read (identity, status, finish
// time, Lab directory, issue, workspace, readiness, Formula approval and snapshots,
// the harness and verification attempts and the verification result). Every other
// value is skipped token by token. Of the notes it keeps the last one, and of the
// analysis attempts the last one's status, error and whether it stored a response
// (as an empty placeholder), which is all AnalysisPolicyRejectionClass reads. kept
// is the size of what was retained. Keys match case-insensitively and the last
// duplicate wins, as in encoding/json; the object must be the whole stream.
func decodeRecent(src io.Reader) (Record, int64, error) {
	var r Record
	var kept int64
	dec := json.NewDecoder(src)
	tok, err := dec.Token()
	if err != nil {
		return r, 0, fmt.Errorf("ledger must be a JSON object: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return r, 0, fmt.Errorf("ledger must be a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return r, 0, err
		}
		key, _ := tok.(string)
		var target any
		name := ""
		for _, field := range []struct {
			name   string
			target any
		}{
			{"schemaVersion", &r.SchemaVersion}, {"kind", &r.Kind}, {"id", &r.ID}, {"status", &r.Status},
			{"formulaId", &r.FormulaID}, {"mode", &r.Mode}, {"finishedAt", &r.FinishedAt}, {"labDir", &r.LabDir},
			{"issue", &r.Issue}, {"workspace", &r.Workspace}, {"readiness", &r.Readiness},
			{"formulaApproved", &r.FormulaApproved}, {"formulaSnapshot", &r.FormulaSnapshot}, {"priorFormulaSnapshots", &r.PriorFormulaSnapshots},
			{"harnessAttempts", &r.HarnessAttempts}, {"verificationAttempts", &r.VerificationAttempts},
			{"verification", &r.Verification}, {"notes", nil}, {"analysisAttempts", nil},
		} {
			if strings.EqualFold(key, field.name) {
				target, name = field.target, field.name
				if target == nil {
					target = field.name
				}
				break
			}
		}
		if target == nil {
			if err = skipJSONValue(dec); err != nil {
				return r, 0, err
			}
			continue
		}
		var raw json.RawMessage
		if err = dec.Decode(&raw); err != nil {
			return r, 0, err
		}
		switch target {
		case "notes":
			var notes []string
			if err = json.Unmarshal(raw, &notes); err != nil {
				return r, 0, fmt.Errorf("notes: %w", err)
			}
			r.Notes = nil
			if len(notes) > 0 {
				r.Notes = notes[len(notes)-1:]
				kept += int64(len(r.Notes[0]))
			}
		case "analysisAttempts":
			var attempts []struct {
				Status   string          `json:"status"`
				Error    string          `json:"error"`
				Response json.RawMessage `json:"response"`
			}
			if err = json.Unmarshal(raw, &attempts); err != nil {
				return r, 0, fmt.Errorf("analysisAttempts: %w", err)
			}
			r.AnalysisAttempts = nil
			if len(attempts) > 0 {
				last := attempts[len(attempts)-1]
				a := AnalysisAttempt{Ordinal: len(attempts), Status: last.Status, Error: last.Error}
				if len(last.Response) > 0 && string(last.Response) != "null" {
					a.Response = &readiness.Analysis{}
				}
				r.AnalysisAttempts = []AnalysisAttempt{a}
				kept += int64(len(last.Status) + len(last.Error))
			}
		default:
			if err = json.Unmarshal(raw, target); err != nil {
				return r, 0, fmt.Errorf("%s: %w", name, err)
			}
			kept += int64(len(raw))
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return r, 0, fmt.Errorf("ledger must be a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return r, 0, fmt.Errorf("ledger must be a single JSON object")
	}
	return r, kept, nil
}

// skipJSONValue discards one value token by token, so a large skipped field costs
// no more memory than its largest scalar.
func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
