package molecule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RecordInventory reports one ListRecords pass. Complete is false when any ledger
// was skipped, and each skipped ledger is named in Diagnostics; nothing is guessed.
type RecordInventory struct {
	Missing     bool     `json:"missing"`
	Complete    bool     `json:"complete"`
	Truncated   bool     `json:"truncated"`
	Diagnostics []string `json:"diagnostics"`
}

func (q *RecordInventory) problem(message string) {
	q.Complete = false
	q.Diagnostics = append(q.Diagnostics, message)
}

// ListRecords visits every readable ledger in lab in file-name order. The shared
// bounded header scan chooses which ledgers exist (with its entry, file, byte and
// symlink bounds), then each one is read in full with ReadRecord and handed to
// visit before the next is read, so memory holds one ledger at a time. It never
// writes, recovers or locks.
func ListRecords(lab string, visit func(Record)) RecordInventory {
	return ListLedgers(lab, func(id string) error {
		r, err := ReadRecord(lab, id)
		if err == nil {
			visit(r)
		}
		return err
	})
}

// ListLedgers is ListRecords without the full read: visit receives the id of
// each ledger the header scan accepted and decides whether to read it, so a
// caller can reuse what it already holds for an unchanged ledger. An error from
// visit is reported under the ledger's file name and marks the pass incomplete.
func ListLedgers(lab string, visit func(id string) error) RecordInventory {
	q := RecordInventory{Complete: true, Diagnostics: []string{}}
	scan := scanLedgers(lab, nil)
	q.Missing = scan.Missing
	for _, f := range scan.Files {
		switch f.Kind {
		case ledger.ProblemNone:
		case ledger.ProblemOversized:
			q.Truncated = true
			q.problem(f.Problem)
			continue
		default:
			q.problem(f.Problem)
			continue
		}
		if err := visit(f.Header.ID); err != nil {
			q.problem(f.Name + ": " + err.Error())
		}
	}
	switch {
	case scan.Problem == "":
	case scan.Truncated:
		q.Truncated = true
		q.problem(scan.Problem)
	default:
		q.problem(scan.Problem)
	}
	return q
}

// ReadRecord reads one ledger without writing or locking. It refuses an invalid
// id, a symlinked ledger directory, a symlink, a non-regular file, a file replaced
// while opened and a ledger larger than ledger.MaxHeaderLedger bytes, and applies
// the same identity rules as the header scan and the loop: the schema and kind,
// an id matching the file name, and the Lab the ledger was written for.
func ReadRecord(lab, id string) (Record, error) {
	var r Record
	if !safeLoopID(id) {
		return r, fmt.Errorf("invalid Molecule id")
	}
	dir := filepath.Join(lab, "ledgers")
	if info, err := os.Lstat(dir); err != nil {
		return r, err
	} else if !info.IsDir() {
		return r, fmt.Errorf("ledger directory is not a regular directory")
	}
	file := filepath.Join(dir, id+".json")
	info, err := os.Lstat(file)
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Size() > ledger.MaxHeaderLedger {
		return r, fmt.Errorf("ledger is not a bounded regular file")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return r, err
	}
	defer f.Close()
	if stat, err := f.Stat(); err != nil || !stat.Mode().IsRegular() || !os.SameFile(info, stat) {
		return r, fmt.Errorf("opened ledger identity mismatch")
	}
	raw, err := io.ReadAll(io.LimitReader(f, ledger.MaxHeaderLedger+1))
	if err != nil {
		return r, err
	}
	if len(raw) > ledger.MaxHeaderLedger {
		return r, fmt.Errorf("ledger grew beyond %d bytes", ledger.MaxHeaderLedger)
	}
	if _, err := ledger.ReadHeader(bytes.NewReader(raw)); err != nil {
		return r, fmt.Errorf("malformed or unsupported ledger: %w", err)
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("malformed ledger: %w", err)
	}
	if r.ID != id || !sameLab(r.LabDir, lab) {
		return Record{}, fmt.Errorf("ledger identity mismatch")
	}
	return r, nil
}

// protectedJSONAddedLines renders, for the Markdown ledger, one line for each
// protected regression check the protected view ran with engine-added -json: the
// jsonAdded marker, the failing tests it recorded and each accepted delta with the
// check it was observed on. Every name comes from the record, so it is bounded and
// stripped of control characters. Checks without jsonAdded render nothing.
func protectedJSONAddedLines(v verification.Result) string {
	if v.Protected == nil {
		return ""
	}
	var b strings.Builder
	for _, o := range v.Protected.Checks {
		if !o.JSONAdded {
			continue
		}
		fmt.Fprintf(&b, "Protected regression `%s` jsonAdded: the protected run added -json after `go test` (approved argv unchanged); %d failing test(s) recorded, %d declared delta(s) accepted\n", oneLine(o.ID), len(o.FailedTests), len(o.AcceptedDeltas))
		for _, d := range o.AcceptedDeltas {
			fmt.Fprintf(&b, "- accepted `%s` `%s` · observed on check `%s` · %s\n", oneLine(d.Package), oneLine(d.Test), oneLine(d.CheckID), oneLine(d.ScopeText()))
		}
	}
	return b.String()
}

// sameLab compares a recorded Lab directory with the one being read, after making
// both absolute and clean.
func sameLab(recorded, lab string) bool {
	a, err := filepath.Abs(recorded)
	if err != nil || recorded == "" {
		return false
	}
	b, err := filepath.Abs(lab)
	return err == nil && a == b
}
