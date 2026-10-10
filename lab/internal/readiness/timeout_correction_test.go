package readiness

import (
	"encoding/json"
	"errors"
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
