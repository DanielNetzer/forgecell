package readiness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const independentAcceptanceMessage = "analysis cannot assert independent acceptance review; retain as a candidate check for separate human review"

func analysisWithCheck(t *testing.T, mutate func(*Check)) []byte {
	t.Helper()
	a := fixture().Analysis
	mutate(&a.Checks[0])
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAnalysisCorrectiveRetryClassifiesOnlyIndependentAcceptanceAsPolicy(t *testing.T) {
	for name, mutate := range map[string]func(*Check){
		"category":       func(c *Check) { c.Category = "independent-acceptance" },
		"provenance":     func(c *Check) { c.IndependentProvenance = "human review" },
		"reviewedRepair": func(c *Check) { c.ReviewedAcceptance = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			a, err := DecodeAnalysis(analysisWithCheck(t, mutate))
			var violation *PolicyViolation
			if err == nil || !errors.As(err, &violation) || !IsPolicyViolation(err) {
				t.Fatalf("not classified as a policy violation: %v", err)
			}
			if err.Error() != independentAcceptanceMessage {
				t.Fatalf("validator message changed: %q", err.Error())
			}
			if a.Summary == "" || len(a.Checks) != 1 {
				t.Fatalf("the rejected response was not returned for the corrective prompt: %+v", a)
			}
		})
	}
}

func TestAnalysisCorrectiveRetryDoesNotClassifyOtherErrors(t *testing.T) {
	valid := analysisWithCheck(t, func(*Check) {})
	if _, err := DecodeAnalysis(valid); err != nil {
		t.Fatalf("control analysis rejected: %v", err)
	}
	missingEvidence := fixture().Analysis
	missingEvidence.EvidenceRequests = []EvidenceRequest{{Path: "../escape", Reason: "needs content", Evidence: []string{"issue"}}}
	badEvidence, _ := json.Marshal(missingEvidence)
	noSummary := fixture().Analysis
	noSummary.Summary = ""
	missingSummary, _ := json.Marshal(noSummary)
	for name, raw := range map[string][]byte{
		"evidence request": badEvidence,
		"summary":          missingSummary,
		"unknown field":    []byte(`{"summary":"x","surprise":1}`),
		"not json":         []byte(`not json`),
		"oversize":         []byte(strings.Repeat(" ", MaxAnalysisBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeAnalysis(raw)
			if err == nil {
				t.Fatal("invalid analysis accepted")
			}
			if IsPolicyViolation(err) {
				t.Fatalf("non-policy error classified as retryable: %v", err)
			}
		})
	}
}

const evidenceRequestCapText = "at most 16 evidence requests permitted"

func analysisWithEvidenceRequests(t *testing.T, n int) []byte {
	t.Helper()
	a := fixture().Analysis
	for i := 0; i < n; i++ {
		a.EvidenceRequests = append(a.EvidenceRequests, EvidenceRequest{Path: fmt.Sprintf("src/file%02d.go", i), Reason: "needs content", Evidence: []string{"issue"}})
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAnalysisCorrectiveRetryClassifiesEvidenceRequestCapAsPolicy(t *testing.T) {
	if EvidenceRequestCapMessage() != evidenceRequestCapText || MaxEvidenceRequests != 16 {
		t.Fatalf("the cap or its message changed: %d %q", MaxEvidenceRequests, EvidenceRequestCapMessage())
	}
	for _, n := range []int{17, 20} {
		t.Run(fmt.Sprintf("%d requests", n), func(t *testing.T) {
			a, err := DecodeAnalysis(analysisWithEvidenceRequests(t, n))
			var violation *PolicyViolation
			if err == nil || !errors.As(err, &violation) || !IsPolicyViolation(err) {
				t.Fatalf("not classified as a policy violation: %v", err)
			}
			if err.Error() != evidenceRequestCapText || violation.Reason != evidenceRequestCapText {
				t.Fatalf("validator message changed: %q", err.Error())
			}
			if a.Summary == "" || len(a.EvidenceRequests) != n {
				t.Fatalf("the rejected response was not returned for the corrective prompt: %+v", a)
			}
		})
	}
	for _, n := range []int{0, 1, 16} {
		if _, err := DecodeAnalysis(analysisWithEvidenceRequests(t, n)); err != nil {
			t.Fatalf("%d requests rejected: %v", n, err)
		}
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapKeepsOtherRequestErrorsPlain(t *testing.T) {
	requests := func(n int) []EvidenceRequest {
		out := make([]EvidenceRequest, n)
		for i := range out {
			out[i] = EvidenceRequest{Path: fmt.Sprintf("src/file%02d.go", i), Reason: "needs content", Evidence: []string{"issue"}}
		}
		return out
	}
	if err := ValidateEvidenceRequests(requests(16), nil); err != nil {
		t.Fatalf("16 requests rejected: %v", err)
	}
	if err := ValidateEvidenceRequests(requests(17), nil); err == nil || !IsPolicyViolation(err) || err.Error() != evidenceRequestCapText {
		t.Fatalf("17 requests: %v", err)
	}
	badPath := requests(16)
	badPath[3].Path = "../escape"
	duplicate := requests(16)
	duplicate[3].Path = duplicate[2].Path
	badCitation := requests(16)
	badCitation[3].Evidence = []string{"unknown-evidence"}
	for name, rs := range map[string][]EvidenceRequest{"path": badPath, "duplicate": duplicate, "citation": badCitation} {
		t.Run(name, func(t *testing.T) {
			err := ValidateEvidenceRequests(rs, []Evidence{})
			if err == nil || IsPolicyViolation(err) {
				t.Fatalf("must be rejected and not retryable: %v", err)
			}
		})
	}
}

func TestHarnessTimeoutIsOmittedFromDigestWhenAbsent(t *testing.T) {
	p := fixture()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "harnessTimeoutMs") {
		t.Fatalf("absent per-Molecule timeout must not change the plan encoding: %s", raw)
	}
	before, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	p.HarnessTimeoutMS = 1_800_000
	after, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a reviewed timeout must change the digest so it needs exact approval")
	}
	raw, _ = json.Marshal(p)
	if !strings.Contains(string(raw), `"harnessTimeoutMs":1800000`) {
		t.Fatalf("timeout not encoded: %s", raw)
	}
}

func TestHarnessTimeoutBounds(t *testing.T) {
	for value, ok := range map[int]bool{0: true, 1: true, 900_000: true, 3_600_000: true, -1: false, 3_600_001: false} {
		p := fixture()
		p.HarnessTimeoutMS = value
		if err := p.Validate(); (err == nil) != ok {
			t.Fatalf("harnessTimeoutMs %d: valid=%t error=%v", value, ok, err)
		}
	}
}

func TestHarnessTimeoutIsNotModelControlled(t *testing.T) {
	if strings.Contains(AnalysisSchema(), "harnessTimeoutMs") {
		t.Fatal("the model-facing analysis schema must not expose the per-Molecule timeout")
	}
}
