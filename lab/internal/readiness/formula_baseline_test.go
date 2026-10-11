package readiness

import (
	"encoding/json"
	"strings"
	"testing"
)

func baselineFixturePlan() Plan {
	p := fixture()
	p.Analysis.Checks = append(p.Analysis.Checks,
		Check{ID: "baseline-gofmt", Category: "regression", Argv: []string{"sh", "-c", "test -z \"$(gofmt -l .)\""}, Dir: "lab", TimeoutMS: 120000, Required: true, Definitions: []string{"manifest"}, Reason: "Formula baseline: CI runs gofmt", Evidence: []string{"manifest"}},
		Check{ID: "candidate", Category: "candidate", Argv: []string{"go", "test", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{"manifest"}, Reason: "Ticket test", Evidence: []string{"manifest"}},
		Check{ID: "optional", Category: "regression", Argv: []string{"go", "vet", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: false, Definitions: []string{"manifest"}, Reason: "Optional vet", Evidence: []string{"manifest"}},
	)
	return p
}

func TestFormulaBaselineAcceptsRequiredRegressionChecks(t *testing.T) {
	p := baselineFixturePlan()
	without, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	p.FormulaBaseline = []string{"baseline-gofmt", "regression"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	with, err := p.Digest()
	if err != nil || with == without {
		t.Fatalf("formulaBaseline is not bound to the digest: %v", err)
	}
}

func TestFormulaBaselineAbsentKeepsEarlierPlanBytes(t *testing.T) {
	raw, err := json.Marshal(baselineFixturePlan())
	if err != nil || strings.Contains(string(raw), "formulaBaseline") {
		t.Fatalf("empty formulaBaseline changed the plan encoding: %s %v", raw, err)
	}
}

func TestFormulaBaselineRefusesLabelWithoutRequiredRegressionCheck(t *testing.T) {
	tests := []struct {
		name  string
		label []string
	}{
		{"unknown id", []string{"missing"}},
		{"duplicate id", []string{"baseline-gofmt", "baseline-gofmt"}},
		{"candidate check", []string{"candidate"}},
		{"optional check", []string{"optional"}},
		{"more labels than checks", []string{"a", "b", "c", "d", "e", "f", "g"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baselineFixturePlan()
			p.FormulaBaseline = tt.label
			if err := p.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
