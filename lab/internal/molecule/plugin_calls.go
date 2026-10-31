package molecule

import (
	"errors"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Bounds of the plugin call records a Molecule keeps.
const (
	// MaxPluginCalls bounds Record.PluginCalls; a call past it only counts in
	// Record.PluginCallsDropped.
	MaxPluginCalls = 256
	// maxPluginDiagnosticNotes bounds the failed-call diagnostics notes of one
	// Molecule; one fixed note says when more were dropped.
	maxPluginDiagnosticNotes = 8
	// maxPluginDiagnosticBytes bounds the diagnostics text of one note.
	maxPluginDiagnosticBytes = 2 << 10
)

// PluginDiagnosticsNote starts every ledger note that holds a failed plugin
// call's diagnostics. Such notes are for the ledger only: run output leaves
// them out (WithoutPluginDiagnostics).
const PluginDiagnosticsNote = "Plugin call diagnostics (ledger only): "

// pluginDiagnosticsDropped is the one note added once the diagnostics notes
// reach their bound.
const pluginDiagnosticsDropped = PluginDiagnosticsNote + "further failed plugin calls were not described; at most 8 are kept."

// PluginCallLog collects the records of the plugin calls made for one
// Molecule, through the adapter's Observer, and the diagnostics of failed
// operations, until the Molecule takes them into its Record. Records hold
// digests, never request or response content. It is safe for concurrent use.
type PluginCallLog struct {
	mu      sync.Mutex
	labDirs []string
	calls   []mcpadapter.CallRecord
	dropped int
	notes   []string
}

// NewPluginCallLog returns an empty log; each of labDirs (the Lab as given and
// as resolved) is replaced in diagnostics.
func NewPluginCallLog(labDirs ...string) *PluginCallLog {
	return &PluginCallLog{labDirs: labDirs}
}

// Observe is an mcpadapter Observer: it keeps at most MaxPluginCalls pending
// records and counts the rest.
func (l *PluginCallLog) Observe(rec mcpadapter.CallRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) >= MaxPluginCalls {
		l.dropped++
		return
	}
	l.calls = append(l.calls, rec)
}

// Fail keeps the bounded, redacted diagnostics of a failed adapter operation
// for the ledger notes. An error that is not an adapter error, or carries no
// diagnostics, adds nothing: its text is not known to be ledger-safe.
func (l *PluginCallLog) Fail(err error) {
	var e *mcpadapter.Error
	if !errors.As(err, &e) || strings.TrimSpace(e.Diagnostics) == "" {
		return
	}
	note := PluginDiagnosticsNote + e.Error() + ": " + redactPluginDiagnostics(e.Diagnostics, l.labDirs...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.notes) <= maxPluginDiagnosticNotes {
		l.notes = append(l.notes, note)
	}
}

// take returns and clears the pending records, drop count and notes.
func (l *PluginCallLog) take() ([]mcpadapter.CallRecord, int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	calls, dropped, notes := l.calls, l.dropped, l.notes
	l.calls, l.dropped, l.notes = nil, 0, nil
	return calls, dropped, notes
}

// absorbPluginCalls moves the log's pending records into r, keeping at most
// MaxPluginCalls in all and counting the rest in PluginCallsDropped, and adds
// at most maxPluginDiagnosticNotes diagnostics notes. It reports whether r
// changed. A nil log changes nothing.
func absorbPluginCalls(r *Record, l *PluginCallLog) bool {
	if l == nil {
		return false
	}
	calls, dropped, notes := l.take()
	for _, rec := range calls {
		if len(r.PluginCalls) >= MaxPluginCalls {
			dropped++
			continue
		}
		r.PluginCalls = append(r.PluginCalls, rec)
	}
	r.PluginCallsDropped += dropped
	kept := 0
	for _, n := range r.Notes {
		if strings.HasPrefix(n, PluginDiagnosticsNote) {
			kept++
		}
	}
	for _, n := range notes {
		switch {
		case kept < maxPluginDiagnosticNotes:
			r.Notes = append(r.Notes, n)
		case kept == maxPluginDiagnosticNotes:
			r.Notes = append(r.Notes, pluginDiagnosticsDropped)
		default:
			continue
		}
		kept++
	}
	return len(calls) > 0 || dropped > 0 || len(notes) > 0
}

// WithoutPluginDiagnostics returns r without the ledger-only diagnostics notes,
// for run output. A record without them is returned unchanged.
func WithoutPluginDiagnostics(r Record) Record {
	found := false
	for _, n := range r.Notes {
		found = found || strings.HasPrefix(n, PluginDiagnosticsNote)
	}
	if !found {
		return r
	}
	notes := make([]string, 0, len(r.Notes))
	for _, n := range r.Notes {
		if !strings.HasPrefix(n, PluginDiagnosticsNote) {
			notes = append(notes, n)
		}
	}
	r.Notes = notes
	return r
}

// redactPluginDiagnostics replaces the Lab directory and the user's home in
// the adapter's already redacted diagnostics, makes them one line without
// control or format characters and cuts them to maxPluginDiagnosticBytes.
func redactPluginDiagnostics(s string, labDirs ...string) string {
	for _, dir := range labDirs {
		if len(dir) > 1 {
			s = strings.ReplaceAll(s, dir, "<lab>")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		s = strings.ReplaceAll(s, home, "<home>")
	}
	s = mcp.Diagnose(strings.Join(strings.Fields(s), " "))
	if len(s) <= maxPluginDiagnosticBytes {
		return s
	}
	cut := maxPluginDiagnosticBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " [truncated]"
}
