package formula

import "fmt"

// ValidateReadiness is an execution precondition, not a historical parsing rule.
// A legacy recipe stays inspectable and must be explicitly revised by its owner.
func (f Formula) ValidateReadiness() error {
	required := []struct{ typ, purpose string }{{"intake", ""}, {"gate", "scope"}, {"harness", ""}, {"check", ""}, {"gate", "review"}, {"ship", ""}, {"document", ""}}
	if len(f.Atoms) != len(required) && !(len(f.Atoms) == len(required)+1 && f.Atoms[len(required)].Type == "learn" && f.Atoms[len(required)].Purpose == "") {
		return fmt.Errorf("ticket readiness requires intake → scope gate → harness → check → review gate → ship → document; review a Formula change explicitly")
	}
	for i, want := range required {
		a := f.Atoms[i]
		if a.Type != want.typ || a.Purpose != want.purpose {
			return fmt.Errorf("Atom %s: expected type %s and purpose %q; no Formula was modified", a.ID, want.typ, want.purpose)
		}
	}
	return nil
}
