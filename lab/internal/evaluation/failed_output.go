package evaluation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// failedOutputTailBytes bounds each retained stream of a failed check.
const failedOutputTailBytes = 8 * 1024

// maxHistoryResultBytes matches the molecule's own bound on a verification history file.
const maxHistoryResultBytes = 64_000_000

// Phases of a failed command: a setup command or a plan check.
const (
	failedPhaseSetup = "setup"
	failedPhaseCheck = "check"
)

// FailedCheckOutput keeps the tail of a failed Molecule setup command's or check's
// streams in the attempt record. The Molecule ledger compacts captured streams away and
// the full history files live under the attempt's Lab, which cohort cleanup deletes; this
// copy survives it. The streams were scrubbed of the exact environment secret values when
// captured; each tail is additionally passed through harness.RedactSecrets, so
// credential-shaped text that was not an environment value is redacted too. Truncated
// reports that the stream was longer than its tail, so the tail's first line may be cut.
// FailedTests names the Go tests the check's full streams report failing, extracted before
// tailing so an early failure is never cut away; FailedTestsTruncated reports that the
// stored stream itself was cut at capture, so the names may be incomplete.
type FailedCheckOutput struct {
	ID                   string   `json:"id"`
	Phase                string   `json:"phase"`
	Protected            bool     `json:"protected,omitempty"`
	Code                 int      `json:"code"`
	StdoutTail           string   `json:"stdoutTail"`
	StdoutTruncated      bool     `json:"stdoutTruncated,omitempty"`
	StderrTail           string   `json:"stderrTail"`
	StderrTruncated      bool     `json:"stderrTruncated,omitempty"`
	FailedTests          []string `json:"failedTests,omitempty"`
	FailedTestsTruncated bool     `json:"failedTestsTruncated,omitempty"`
}

// failedCheckOutput reads the Molecule's latest verification history result and returns
// the redacted last 8 KiB of stdout and stderr of every failed setup command and check,
// in the candidate view and in its protected run. Passing commands record nothing. A
// missing, oversized or changed history file yields nothing rather than failing the
// attempt.
func failedCheckOutput(r molecule.Record) []FailedCheckOutput {
	var ref *molecule.VerificationFile
	for i := len(r.VerificationAttempts) - 1; i >= 0; i-- {
		if r.VerificationAttempts[i].Result != nil {
			ref = r.VerificationAttempts[i].Result
			break
		}
	}
	if ref == nil || r.LabDir == "" {
		return nil
	}
	prefix := filepath.ToSlash(filepath.Join("verification-history", r.ID)) + "/"
	if !strings.HasPrefix(ref.Path, prefix) || filepath.ToSlash(filepath.Clean(ref.Path)) != ref.Path || strings.Contains(ref.Path, "..") {
		return nil
	}
	file := filepath.Join(r.LabDir, filepath.FromSlash(ref.Path))
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxHistoryResultBytes {
		return nil
	}
	raw, err := os.ReadFile(file)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.SHA256 {
		return nil
	}
	var result verification.Result
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	var out []FailedCheckOutput
	collect := func(observations []verification.Observation, phase string, protected bool) {
		for _, obs := range observations {
			if obs.Result.OK {
				continue
			}
			f := FailedCheckOutput{ID: obs.ID, Phase: phase, Protected: protected, Code: obs.Result.Code}
			stdout, stderr := harness.RedactSecrets(obs.Result.Stdout), harness.RedactSecrets(obs.Result.Stderr)
			if phase == failedPhaseCheck {
				f.FailedTests = failedGoTests(stdout, stderr)
				f.FailedTestsTruncated = streamCut(obs.Result)
			}
			f.StdoutTail, f.StdoutTruncated = tail(stdout, failedOutputTailBytes), len(stdout) > failedOutputTailBytes
			f.StderrTail, f.StderrTruncated = tail(stderr, failedOutputTailBytes), len(stderr) > failedOutputTailBytes
			out = append(out, f)
		}
	}
	views := []struct {
		v         *verification.Result
		protected bool
	}{{&result, false}, {result.Protected, true}}
	for _, view := range views {
		if view.v == nil {
			continue
		}
		collect(view.v.Setup, failedPhaseSetup, view.protected)
		collect(view.v.Checks, failedPhaseCheck, view.protected)
	}
	return out
}

// streamCut reports that a check's stored streams are not its whole output: the capture
// kept only a prefix (Truncated) or stopped at the output cap (Overflow).
func streamCut(r process.Result) bool {
	return r.Truncated || r.Overflow
}

// goTestFailLine matches a plain go test "--- FAIL: TestName (…)" line, including an
// indented subtest line, whose name is kept whole (TestX/sub).
var goTestFailLine = regexp.MustCompile(`^[ \t]*--- FAIL: (\S+)`)

// failedGoTests returns the sorted, unique Go test names the given full check streams
// report failing. A stream carrying go test -json events is read from its structured
// events only (Action "fail" with a Test, subtests kept as Parent/Sub), so a "--- FAIL:"
// line printed inside an event's Output is never counted twice or invented; a plain
// stream is read from its "--- FAIL:" lines.
func failedGoTests(streams ...string) []string {
	var names []string
	for _, s := range streams {
		var events, plain []string
		structured := false
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "{") {
				var e goTestEvent
				if json.Unmarshal([]byte(line), &e) == nil && e.Action != "" {
					structured = true
					if e.Action == "fail" && e.Test != "" {
						events = append(events, e.Test)
					}
					continue
				}
			}
			if m := goTestFailLine.FindStringSubmatch(line); m != nil {
				plain = append(plain, m[1])
			}
		}
		if structured {
			names = append(names, events...)
		} else {
			names = append(names, plain...)
		}
	}
	return uniqueSorted(names)
}

// uniqueSorted returns the sorted distinct values of names, nil when there are none.
func uniqueSorted(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// tail returns at most the last n bytes of s, starting on a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// tailLines returns the last n lines of s (every line when n is negative), ignoring one
// trailing newline.
func tailLines(s string, n int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
