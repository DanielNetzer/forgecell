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

// TestFailedCheckOutputExtractsFailedTestsFromFullStream: failing Go test names are read
// from the whole stored stream before it is tailed. A go test -json stream whose only
// failure comes before more than 8 KiB of pass events still yields it, a JSON-escaped
// subtest name is decoded, and a "--- FAIL:" line printed inside an event's Output is not
// counted. A check whose stored stream was itself cut is flagged, and the flag reaches
// the attempt and report.md.
func TestFailedCheckOutputExtractsFailedTestsFromFullStream(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(`{"Action":"run","Package":"x","Test":"TestEarly"}` + "\n")
	stream.WriteString(`{"Action":"output","Package":"x","Test":"TestEarly","Output":"--- FAIL: TestPrinted (0.00s)\n"}` + "\n")
	stream.WriteString(`{"Action":"fail","Package":"x","Test":"TestEarly","Elapsed":0.01}` + "\n")
	stream.WriteString(`{"Action":"fail","Package":"x","Test":"TestEsc/a\u0026b\"q\"","Elapsed":0}` + "\n")
	for i := 0; stream.Len() < 12*1024; i++ {
		fmt.Fprintf(&stream, `{"Action":"pass","Package":"x","Test":"TestPass%03d","Elapsed":0}`+"\n", i)
	}
	stream.WriteString(`{"Action":"fail","Package":"x","Elapsed":1}` + "\n")
	full := process.Result{OK: false, Code: 1, Stdout: stream.String()}
	cut := process.Result{OK: false, Code: 1, Stdout: "--- FAIL: TestCut (0.00s)\n", Truncated: true}
	record := writeHistory(t, verification.Result{
		SchemaVersion: "v1",
		Checks:        []verification.Observation{{ID: "acceptance", Result: process.Result{OK: true}}, {ID: "go-test", Result: full}, {ID: "go-vet", Result: cut}},
	})
	got := failedCheckOutput(record)
	if len(got) != 2 {
		t.Fatalf("failed outputs = %d, want 2: %+v", len(got), got)
	}
	test := got[0]
	if strings.Contains(test.StdoutTail, "TestEarly") || !test.StdoutTruncated {
		t.Fatalf("the early failure should be cut from the 8 KiB tail; truncated=%v", test.StdoutTruncated)
	}
	if names := strings.Join(test.FailedTests, ","); names != `TestEarly,TestEsc/a&b"q"` || test.FailedTestsTruncated {
		t.Fatalf("go-test failed tests = %q truncated=%v", names, test.FailedTestsTruncated)
	}
	if vet := got[1]; strings.Join(vet.FailedTests, ",") != "TestCut" || !vet.FailedTestsTruncated {
		t.Fatalf("cut stream = %+v", vet)
	}

	a := completedAttempt(nil)
	a.FailedCheckOutput = got
	a.Molecule.Verification = &verification.Result{Checks: []verification.Observation{{ID: "acceptance", Result: process.Result{OK: true}}}}
	setHiddenAcceptance(&a)
	if names := strings.Join(a.RegressionFailedTests, ","); names != `TestCut,TestEarly,TestEsc/a&b"q"` || !a.RegressionFailedTestsTruncated {
		t.Fatalf("regression failed tests = %q truncated=%v", names, a.RegressionFailedTestsTruncated)
	}
	md := renderCohortMarkdown(buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 1}, []string{"base"}, map[string]string{}, []string{"task-a"}, []CohortAttempt{{Task: "task-a", Variant: "base", Repetition: 1, Attempt: a}}))
	if want := "  - regression failed tests: TestCut, TestEarly, TestEsc/a&b\"q\" (output truncated)\n"; !strings.Contains(md, want) {
		t.Fatalf("report.md missing %q:\n%s", want, md)
	}
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

// TestGoTestFailuresRecordPackages: each failed test is recorded with its package import
// path, from the Package field of go test -json events and from the "FAIL\t<pkg>" result
// line that follows a plain stream's "--- FAIL:" lines. A package failure no failed test
// accounts for (a build failure, a panic outside any test) is reported as unattributed.
func TestGoTestFailuresRecordPackages(t *testing.T) {
	plain := "--- FAIL: TestB (0.00s)\n    b_test.go:3: boom\n    --- FAIL: TestB/sub (0.00s)\nFAIL\nFAIL\texample.test/one\t0.01s\nok  \texample.test/two\t0.02s\n--- FAIL: TestA (0.00s)\nFAIL\nFAIL\texample.test/three\t0.01s\nFAIL\n"
	failures, unattributed := goTestFailures(plain, "")
	want := []GoTestFailure{{"example.test/one", "TestB"}, {"example.test/one", "TestB/sub"}, {"example.test/three", "TestA"}}
	if fmt.Sprint(failures) != fmt.Sprint(want) || unattributed {
		t.Fatalf("plain failures = %v unattributed=%v, want %v", failures, unattributed, want)
	}
	if names := strings.Join(failedGoTests(plain), ","); names != "TestA,TestB,TestB/sub" {
		t.Fatalf("failed test names = %q", names)
	}

	jsonStream := `{"Action":"run","Package":"example.test/one","Test":"TestX"}
{"Action":"output","Package":"example.test/one","Test":"TestX","Output":"--- FAIL: TestX (0.00s)\n"}
{"Action":"fail","Package":"example.test/one","Test":"TestX"}
{"Action":"fail","Package":"example.test/one"}
{"Action":"pass","Package":"example.test/two","Test":"TestY"}
`
	failures, unattributed = goTestFailures(jsonStream)
	if fmt.Sprint(failures) != fmt.Sprint([]GoTestFailure{{"example.test/one", "TestX"}}) || unattributed {
		t.Fatalf("json failures = %v unattributed=%v", failures, unattributed)
	}

	for name, stream := range map[string]string{
		"plain build failure": "# example.test/one\none.go:3:1: syntax error\nFAIL\texample.test/one [build failed]\n",
		"plain timeout":       "panic: test timed out after 1s\nFAIL\texample.test/one\t1.01s\n",
		"json build failure":  `{"Action":"fail","Package":"example.test/one"}` + "\n",
	} {
		if failures, unattributed := goTestFailures(stream); len(failures) != 0 || !unattributed {
			t.Fatalf("%s: failures=%v unattributed=%v, want none and unattributed", name, failures, unattributed)
		}
	}

	// A named failed test beside a non-test failure in the same package (a timeout panic, a
	// build or setup failure) still records the test, and the check is unattributed.
	plainTimeout := "--- FAIL: TestFlaky (0.00s)\n    flaky_test.go:3: flaked\npanic: test timed out after 1s\n\trunning tests:\n\t\tTestHang (1s)\nFAIL\texample.test/one\t1.01s\n"
	jsonTimeout := `{"Action":"run","Package":"example.test/one","Test":"TestFlaky"}
{"Action":"fail","Package":"example.test/one","Test":"TestFlaky"}
{"Action":"run","Package":"example.test/one","Test":"TestHang"}
{"Action":"output","Package":"example.test/one","Output":"panic: test timed out after 1s\n"}
{"Action":"output","Package":"example.test/one","Output":"FAIL\texample.test/one\t1.01s\n"}
{"Action":"fail","Package":"example.test/one"}
`
	for name, stream := range map[string]string{
		"plain timeout panic":   plainTimeout,
		"json timeout panic":    jsonTimeout,
		"plain panic":           "--- FAIL: TestFlaky (0.00s)\npanic: runtime error: index out of range\nFAIL\texample.test/one\t0.01s\n",
		"plain setup failed":    "--- FAIL: TestFlaky (0.00s)\nFAIL\texample.test/one\t0.01s\nFAIL\texample.test/two [setup failed]\n",
		"json build-fail":       `{"Action":"fail","Package":"example.test/one","Test":"TestFlaky"}` + "\n" + `{"ImportPath":"example.test/two","Action":"build-fail"}` + "\n",
		"json build failed out": `{"Action":"fail","Package":"example.test/one","Test":"TestFlaky"}` + "\n" + `{"Action":"output","Package":"example.test/two","Output":"FAIL\texample.test/two [build failed]\n"}` + "\n",
		// Go runtime aborts beside a flaky named failure: a deadlock and a killed binary.
		"plain fatal error":       "--- FAIL: TestFlaky (0.00s)\n    flaky_test.go:3: flaked\nfatal error: all goroutines are asleep - deadlock!\n\ngoroutine 1 [chan receive]:\nexit status 2\nFAIL\texample.test/one\t0.01s\n",
		"plain signal":            "--- FAIL: TestFlaky (0.00s)\n    flaky_test.go:3: flaked\nsignal: segmentation fault\nFAIL\texample.test/one\t0.01s\n",
		"json fatal error output": `{"Action":"fail","Package":"example.test/one","Test":"TestFlaky"}` + "\n" + `{"Action":"output","Package":"example.test/one","Output":"fatal error: all goroutines are asleep - deadlock!\n"}` + "\n" + `{"Action":"fail","Package":"example.test/one"}` + "\n",
		// An exit status with no failed test before it in its package, the stream cut before
		// that package's result line.
		"plain exit status outside a test": "--- FAIL: TestFlaky (0.00s)\nFAIL\nexit status 1\nFAIL\texample.test/one\t0.01s\nexit status 2\n",
	} {
		failures, unattributed := goTestFailures(stream)
		if fmt.Sprint(failures) != fmt.Sprint([]GoTestFailure{{"example.test/one", "TestFlaky"}}) || !unattributed {
			t.Fatalf("%s: failures=%v unattributed=%v, want TestFlaky and unattributed", name, failures, unattributed)
		}
	}

	// The exit status go test prints after a package's named failures stays attributed.
	if failures, unattributed := goTestFailures("--- FAIL: TestFlaky (0.00s)\nFAIL\nexit status 1\nFAIL\texample.test/one\t0.01s\n"); fmt.Sprint(failures) != fmt.Sprint([]GoTestFailure{{"example.test/one", "TestFlaky"}}) || unattributed {
		t.Fatalf("exit status after a named failure: failures=%v unattributed=%v", failures, unattributed)
	}

	// A plain "--- FAIL:" line no result line follows keeps an empty package.
	if failures, _ := goTestFailures("--- FAIL: TestCut (0.00s)\n"); fmt.Sprint(failures) != fmt.Sprint([]GoTestFailure{{"", "TestCut"}}) {
		t.Fatalf("cut stream failures = %v", failures)
	}
}
