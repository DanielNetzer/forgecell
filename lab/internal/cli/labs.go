package cli

import (
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Ledger reads are bounded like the issue-queue inventory.
var maxLabLedgerFiles = 256

const (
	maxLabLedgerEntries = 4096
	maxLabLedgerBytes   = 20_000_000
)

type labMolecule struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"startedAt"`
}
type labSummary struct {
	Directory         string         `json:"directory"`
	Repo              string         `json:"repo"`
	FormulaID         string         `json:"activeFormulaId"`
	Binding           string         `json:"binding"`
	LastMolecule      *labMolecule   `json:"lastMolecule"`
	StatusCounts      map[string]int `json:"statusCounts"`
	InvalidRecords    int            `json:"invalidRecords"`
	OrderingUncertain bool           `json:"orderingUncertain"`
	Truncated         bool           `json:"truncated"`
	Diagnostics       []string       `json:"diagnostics"`
	Error             string         `json:"error,omitempty"`
}
type labsReport struct {
	ReadOnly  bool         `json:"readOnly"`
	Directory string       `json:"labsDirectory"`
	Truncated bool         `json:"truncated"`
	Labs      []labSummary `json:"labs"`
}

// runLabs lists every Lab under FORGECELL_HOME/labs, legacy path-keyed Labs
// included. It only reads: no approval, harness command or ledger body is
// executed or printed.
func runLabs(args []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	flags := flag.NewFlagSet("labs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonMode := flags.Bool("json", false, "Structured read-only list")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("labs accepts only --json"))
	}
	root, err := resolvedCheckoutRoot()
	if err != nil {
		return fail(err)
	}
	home, err := forgecellHome(root)
	if err != nil {
		return fail(err)
	}
	labs := filepath.Join(home, "labs")
	entries, truncated, err := scanLabs(labs)
	if err != nil {
		return fail(err)
	}
	report := labsReport{ReadOnly: true, Directory: labs, Truncated: truncated, Labs: []labSummary{}}
	for _, e := range entries {
		s := labSummary{Directory: e.Dir, Repo: e.Repo, FormulaID: e.FormulaID, Binding: e.Binding, Error: e.Err, StatusCounts: map[string]int{}, Diagnostics: []string{}}
		if !e.Linked {
			summarizeLedgers(e.Dir, &s)
		}
		report.Truncated = report.Truncated || s.Truncated
		report.Labs = append(report.Labs, s)
	}
	if *jsonMode {
		return encode(out, stderr, report)
	}
	renderLabs(out, report)
	return 0
}

func (s *labSummary) problem(message string) { s.Diagnostics = append(s.Diagnostics, message) }
func (s *labSummary) truncate(message string) {
	s.Truncated = true
	s.problem(message)
}

// summarizeLedgers counts valid ledger headers by raw status. Records that fail
// validation are counted as invalid, never dropped. The last Molecule is the
// latest start time among valid records, ties broken by the smaller id; it is
// withheld whenever any start time is unusable or the scan was truncated.
func summarizeLedgers(lab string, s *labSummary) {
	path := filepath.Join(lab, "ledgers")
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.problem("ledger directory unreadable")
		return
	}
	if !before.IsDir() {
		s.problem("ledger directory is not a regular directory")
		return
	}
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		s.problem("ledger directory unreadable")
		return
	}
	defer dir.Close()
	if opened, err := dir.Stat(); err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		s.problem("ledger directory identity changed")
		return
	}
	names, err := dir.Readdirnames(maxLabLedgerEntries + 1)
	if err != nil && err != io.EOF {
		s.problem("ledger directory unreadable")
		return
	}
	if len(names) > maxLabLedgerEntries {
		s.truncate(fmt.Sprintf("ledger directory scan truncated at %d entries", maxLabLedgerEntries))
		s.OrderingUncertain = true
		return
	}
	sort.Strings(names)
	var last *labMolecule
	var lastTime time.Time
	files, total := 0, 0
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if files >= maxLabLedgerFiles {
			s.truncate(fmt.Sprintf("ledger scan truncated at %d JSON files", maxLabLedgerFiles))
			break
		}
		files++
		raw, err := readLedgerForInspection(filepath.Join(path, name))
		if err != nil {
			s.InvalidRecords++
			continue
		}
		total += len(raw)
		if total > maxLabLedgerBytes {
			s.truncate(fmt.Sprintf("ledger byte scan truncated at %d bytes", maxLabLedgerBytes))
			break
		}
		record, err := ledger.Decode(raw)
		if err != nil || record.ID+".json" != name {
			s.InvalidRecords++
			continue
		}
		s.StatusCounts[record.Status]++
		var started time.Time
		if projection, err := record.QueueProjection(); err == nil {
			started, err = time.Parse(time.RFC3339Nano, projection.StartedAt)
		}
		if started.IsZero() {
			s.OrderingUncertain = true
			continue
		}
		if last == nil || started.After(lastTime) || (started.Equal(lastTime) && record.ID < last.ID) {
			last, lastTime = &labMolecule{ID: record.ID, Status: record.Status, StartedAt: started.Format(time.RFC3339Nano)}, started
		}
	}
	if s.Truncated {
		s.OrderingUncertain = true
	}
	if !s.OrderingUncertain {
		s.LastMolecule = last
	}
}

func renderLabs(out io.Writer, report labsReport) {
	if len(report.Labs) == 0 {
		fmt.Fprintf(out, "No Labs found under %s.\n", safeText(report.Directory))
		return
	}
	fmt.Fprintf(out, "Labs under %s (read-only):\n", safeText(report.Directory))
	if report.Truncated {
		fmt.Fprintln(out, "Warning: the listing is truncated; some Labs or ledgers are not shown.")
	}
	for _, l := range report.Labs {
		fmt.Fprintf(out, "%s\n", safeText(l.Directory))
		if l.Error != "" {
			fmt.Fprintf(out, "  error: %s\n", safeText(l.Error))
		}
		fmt.Fprintf(out, "  repository: %s · Formula: %s · binding: %s\n", orNone(l.Repo), orNone(l.FormulaID), orNone(l.Binding))
		switch {
		case l.LastMolecule != nil:
			fmt.Fprintf(out, "  last Molecule: %s · %s · started %s\n", safeText(l.LastMolecule.ID), safeText(l.LastMolecule.Status), l.LastMolecule.StartedAt)
		case l.OrderingUncertain:
			fmt.Fprintln(out, "  last Molecule: unknown (ordering uncertain)")
		default:
			fmt.Fprintln(out, "  last Molecule: none")
		}
		var counts []string
		for status, n := range l.StatusCounts {
			counts = append(counts, fmt.Sprintf("%s=%d", safeText(status), n))
		}
		sort.Strings(counts)
		fmt.Fprintf(out, "  statuses: %s · invalid records: %d\n", orNone(strings.Join(counts, " ")), l.InvalidRecords)
		for _, d := range l.Diagnostics {
			fmt.Fprintf(out, "  note: %s\n", d)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return safeText(s)
}

// safeText quotes values read from Lab files that could carry terminal control characters.
func safeText(s string) string {
	for _, r := range s {
		if !strconv.IsPrint(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
}
