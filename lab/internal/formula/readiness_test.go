package formula

import (
	"strings"
	"testing"
)

const gatedRecipe = `schemaVersion: v0
kind: formula
id: ready
atoms:
- id: intake
  type: intake
- id: scope
  type: gate
  purpose: scope
- id: work
  type: harness
- id: checks
  type: check
- id: review
  type: gate
  purpose: review
- id: ship
  type: ship
- id: document
  type: document
`

func TestReadinessGateContract(t *testing.T) {
	f, err := Parse([]byte(gatedRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ValidateReadiness(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		recipe,
		strings.Replace(gatedRecipe, "purpose: scope", "purpose: review", 1),
		strings.Replace(gatedRecipe, "purpose: scope", "purpose: unknown", 1),
		strings.Replace(gatedRecipe, "- id: scope\n  type: gate\n  purpose: scope\n", "", 1),
		strings.Replace(gatedRecipe, "- id: checks\n  type: check\n", "", 1),
		strings.Replace(gatedRecipe, "- id: work\n  type: harness", "- id: work\n  type: ship", 1),
		strings.Replace(gatedRecipe, "type: intake", "type: harness", 1),
		strings.Replace(gatedRecipe, "type: check", "type: gate\n  purpose: scope", 1),
	} {
		f, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal("historical reading must still work:", err)
		}
		if err = f.ValidateReadiness(); err == nil {
			t.Fatalf("unsafe execution ordering accepted:\n%s", raw)
		}
	}
}
