package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const timeoutRecipe = `schemaVersion: v0
kind: formula
id: sample
harness:
  binding: custom
  command: [forgecell]
  timeoutMs: 900000
%satoms:
  - id: intake
    type: intake
`

// The per-Molecule timeout is part of what the human approves, so the scope gate
// shows the effective value next to the Formula default and maximum.
func TestHarnessTimeoutShownAtScopeGate(t *testing.T) {
	withMaximum := strings.Replace(timeoutRecipe, "%s", "  maxTimeoutMs: 1800000\n", 1)
	withoutMaximum := strings.Replace(timeoutRecipe, "%s", "", 1)
	for _, tc := range []struct {
		name         string
		yaml         string
		timeout      int
		continuation string
		want         string
		absent       string
	}{
		{"reviewed value", withMaximum, 1500000, "", "Coding harness timeout: 1500000ms (reviewed per-Molecule harnessTimeoutMs; Formula timeoutMs 900000, maximum 1800000)", ""},
		{"formula default with maximum", withMaximum, 0, "", "Coding harness timeout: 900000ms (Formula timeoutMs; maximum 1800000, no per-Molecule extension proposed)", ""},
		{"formula default without maximum", withoutMaximum, 0, "", "Coding harness timeout: 900000ms (Formula timeoutMs; the Formula declares no maximum, so no extension is possible)", ""},
		{"formula timeout unset", "", 0, "", "Coding harness timeout: Formula timeoutMs unset (process default applies)", ""},
		{"unparsable snapshot keeps the reviewed value", "", 1500000, "", "Coding harness timeout: 1500000ms (reviewed per-Molecule harnessTimeoutMs)", ""},
		{"checks only never invokes the harness", withoutMaximum, 0, "checks-only", "", "Coding harness timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := readiness.Plan{HarnessTimeoutMS: tc.timeout, Continuation: tc.continuation}
			r := molecule.Record{ID: "mol-1", FormulaSnapshot: molecule.Snapshot{YAML: tc.yaml}, Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: p, Digest: "d"}}}}
			var out bytes.Buffer
			presentRun(&out, r)
			if tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, out.String())
			}
			if tc.absent != "" && strings.Contains(out.String(), tc.absent) {
				t.Fatalf("unexpected %q in:\n%s", tc.absent, out.String())
			}
		})
	}
}
