package molecule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// Attempts recorded before CodingOutcomeFromProcess projected stdout only, so
// a failed spawn or timeout was stored as "...: EOF". Both projections must
// stay verifiable; any other reason is still tampering.
func TestHistoricalCodingProjectionStaysVerifiable(t *testing.T) {
	o, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Coding == nil {
		t.Fatalf("missing harness attempt: %+v", r.HarnessAttempts)
	}
	a := &r.HarnessAttempts[0]
	if a.Result.Error == "" || strings.Contains(a.Coding.Reason, "EOF") {
		t.Fatalf("precise failure reason not kept: error=%q reason=%q", a.Result.Error, a.Coding.Reason)
	}
	if err = ValidateVerificationHistory(r); err != nil {
		t.Fatalf("current projection rejected: %v", err)
	}
	legacy := harness.CodingOutcomeFromResult("")
	a.Coding = &legacy
	if err = ValidateVerificationHistory(r); err != nil {
		t.Fatalf("historical projection rejected: %v", err)
	}
	a.Coding.Reason = "tampered"
	if err = ValidateVerificationHistory(r); err == nil {
		t.Fatal("tampered coding outcome accepted")
	}
}
