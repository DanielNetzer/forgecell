package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// TestFailedCheckOutputTail: a check that prints 20 KiB and exits 1 yields an 8 KiB tail
// ending with its last line, in both the candidate view and its protected run, read from
// the verification history result file. Passing checks store nothing, and a history file
// whose hash no longer matches the ledger yields nothing.
func TestFailedCheckOutputTail(t *testing.T) {
	ctx := context.Background()
	run := func(script string) process.Result {
		t.Helper()
		return process.Run(ctx, process.Options{Argv: []string{"/bin/sh", "-c", script}, Dir: t.TempDir(), Stdin: []byte{}, Timeout: 10 * time.Second, MaxOutputBytes: 2_000_000})
	}
	// 440 lines of 48 bytes is 21120 bytes (over 20 KiB), then a distinct last line.
	failing := run(`i=0; while [ $i -lt 440 ]; do printf 'line-%04d-0123456789012345678901234567890123456\n' $i; i=$((i+1)); done; echo last-line; printf 'stderr-detail\n' >&2; exit 1`)
	if failing.OK || failing.Code != 1 || len(failing.Stdout) < 20*1024 {
		t.Fatalf("stub check: ok=%v code=%d stdout=%d bytes", failing.OK, failing.Code, len(failing.Stdout))
	}
	passing := run(`echo fine`)
	if !passing.OK {
		t.Fatalf("passing stub failed: %+v", passing)
	}
	result := verification.Result{
		SchemaVersion: "v1",
		Checks:        []verification.Observation{{ID: "go-test", Result: failing}, {ID: "go-vet", Result: passing}},
		Protected:     &verification.Result{SchemaVersion: "v1", Checks: []verification.Observation{{ID: "go-test", Result: failing}, {ID: "go-vet", Result: passing}}},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	lab := t.TempDir()
	rel := "verification-history/mol-1/001-result.json"
	if err := os.MkdirAll(filepath.Join(lab, filepath.Dir(rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, rel), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref := molecule.VerificationFile{Path: rel, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	record := molecule.Record{ID: "mol-1", LabDir: lab, VerificationAttempts: []molecule.VerificationAttempt{{Status: "failed", Result: &ref}}}

	got := failedCheckOutput(record)
	if len(got) != 2 {
		t.Fatalf("failed check outputs = %d, want 2 (passing checks store nothing): %+v", len(got), got)
	}
	for i, f := range got {
		if f.ID != "go-test" || f.Phase != "check" || f.Code != 1 || f.Protected != (i == 1) || !f.StdoutTruncated || f.StderrTruncated {
			t.Fatalf("entry %d = %+v", i, FailedCheckOutput{ID: f.ID, Phase: f.Phase, Protected: f.Protected, Code: f.Code, StdoutTruncated: f.StdoutTruncated, StderrTruncated: f.StderrTruncated})
		}
		if len(f.StdoutTail) != 8*1024 || !strings.HasSuffix(f.StdoutTail, "last-line\n") || !strings.HasSuffix(failing.Stdout, f.StdoutTail) {
			t.Fatalf("entry %d stdout tail: %d bytes, suffix %q", i, len(f.StdoutTail), f.StdoutTail[len(f.StdoutTail)-20:])
		}
		if f.StderrTail != "stderr-detail\n" {
			t.Fatalf("entry %d stderr tail = %q", i, f.StderrTail)
		}
	}

	// No failed check: nothing is stored.
	allPass, _ := json.Marshal(verification.Result{SchemaVersion: "v1", Checks: []verification.Observation{{ID: "go-vet", Result: passing}}})
	passRel := "verification-history/mol-1/002-result.json"
	os.WriteFile(filepath.Join(lab, passRel), allPass, 0600)
	passRef := molecule.VerificationFile{Path: passRel, SHA256: fmt.Sprintf("%x", sha256.Sum256(allPass))}
	if got := failedCheckOutput(molecule.Record{ID: "mol-1", LabDir: lab, VerificationAttempts: []molecule.VerificationAttempt{{Result: &passRef}}}); got != nil {
		t.Fatalf("passing verification stored output: %+v", got)
	}

	// A changed history file is not trusted.
	tampered := ref
	tampered.SHA256 = strings.Repeat("0", 64)
	if got := failedCheckOutput(molecule.Record{ID: "mol-1", LabDir: lab, VerificationAttempts: []molecule.VerificationAttempt{{Result: &tampered}}}); got != nil {
		t.Fatalf("changed history file trusted: %+v", got)
	}

	// The tail starts on a rune boundary.
	if s := tail("é"+strings.Repeat("a", 8191), 8192); s != strings.Repeat("a", 8191) {
		t.Fatalf("tail split a rune: %d bytes", len(s))
	}
}

// writeHistory stores a verification result as the Molecule's latest history file.
func writeHistory(t *testing.T, result verification.Result) molecule.Record {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	lab := t.TempDir()
	rel := "verification-history/mol-1/001-result.json"
	if err := os.MkdirAll(filepath.Join(lab, filepath.Dir(rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, rel), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref := molecule.VerificationFile{Path: rel, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	return molecule.Record{ID: "mol-1", LabDir: lab, VerificationAttempts: []molecule.VerificationAttempt{{Status: "failed", Result: &ref}}}
}

// TestFailedSetupOutputRecordedAndRedacted: a failing setup command's tail is recorded with
// Phase setup (in the protected view too), and a credential-shaped string that was never an
// environment value is redacted in attempt.json and report.md.
func TestFailedSetupOutputRecordedAndRedacted(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1b2C3d4", 5)
	failing := process.Result{OK: false, Code: 3, Stdout: "installing\nfetched with " + token + "\nsetup failed\n", Stderr: "auth token=" + token + "\n"}
	passing := process.Result{OK: true, Stdout: "fine\n"}
	record := writeHistory(t, verification.Result{
		SchemaVersion: "v1",
		Setup:         []verification.Observation{{ID: "deps", Result: failing}, {ID: "tools", Result: passing}},
		Checks:        []verification.Observation{{ID: "go-test", Result: passing}},
		Protected:     &verification.Result{SchemaVersion: "v1", Setup: []verification.Observation{{ID: "deps", Result: failing}}},
	})
	got := failedCheckOutput(record)
	if len(got) != 2 {
		t.Fatalf("failed outputs = %d, want 2: %+v", len(got), got)
	}
	for i, f := range got {
		if f.ID != "deps" || f.Phase != "setup" || f.Code != 3 || f.Protected != (i == 1) || f.StdoutTruncated {
			t.Fatalf("entry %d = %+v", i, f)
		}
		if strings.Contains(f.StdoutTail, token) || strings.Contains(f.StderrTail, token) || !strings.Contains(f.StdoutTail, "[REDACTED]") || !strings.HasSuffix(f.StdoutTail, "setup failed\n") {
			t.Fatalf("entry %d not redacted: %+v", i, f)
		}
	}
	a := completedAttempt(nil)
	a.FailedCheckOutput = got
	ca := CohortAttempt{Task: "task-a", Variant: "base", Repetition: 1, Attempt: a}
	path := filepath.Join(t.TempDir(), "attempt.json")
	if err := saveAttemptRecord(path, ca); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), `"phase": "setup"`) {
		t.Fatalf("attempt.json leaks the token or lacks the phase:\n%s", raw)
	}
	md := renderCohortMarkdown(buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 1}, []string{"base", "cand"}, map[string]string{}, []string{"task-a"}, []CohortAttempt{ca}))
	if strings.Contains(md, token) || !strings.Contains(md, "  - failed setup `deps` exit 3; last 20 lines of stdout:\n") || !strings.Contains(md, "        fetched with [REDACTED]\n") {
		t.Fatalf("report.md leaks the token or lacks the setup entry:\n%s", md)
	}
}
