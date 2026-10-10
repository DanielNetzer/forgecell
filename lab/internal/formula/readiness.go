package formula

import (
	"fmt"
	"strings"
)

// ExecutionRole describes a supported action, independently of its owner-chosen ID.
type ExecutionRole string

const (
	IntakeRole     ExecutionRole = "intake"
	ScopeApproval  ExecutionRole = "scope-approval"
	Coding         ExecutionRole = "coding"
	Verification   ExecutionRole = "verification"
	ReviewApproval ExecutionRole = "review-approval"
	Publication    ExecutionRole = "publication"
	Documentation  ExecutionRole = "documentation"
	Learning       ExecutionRole = "learning"
)

// ExecutionRoles validates the Stage 1 execution contract. Intake and exact human
// scope approval precede coding; post-coding verification and human review precede
// publication. An optional pre-coding check requires a subsequent contract stage.
// Parsing historical recipes is deliberately separate from execution validation.
func (f Formula) ExecutionRoles() (map[ExecutionRole]Atom, error) {
	required := []struct {
		role         ExecutionRole
		typ, purpose string
	}{
		{IntakeRole, "intake", ""}, {ScopeApproval, "gate", "scope"}, {Coding, "harness", ""},
		{Verification, "check", ""}, {ReviewApproval, "gate", "review"}, {Publication, "ship", ""}, {Documentation, "document", ""},
	}
	if len(f.Atoms) != len(required) && !(len(f.Atoms) == len(required)+1 && f.Atoms[len(required)].Type == "learn" && f.Atoms[len(required)].Purpose == "") {
		return nil, fmt.Errorf("ticket readiness requires intake → scope gate → harness → check → review gate → ship → document; review a Formula change explicitly")
	}
	roles := make(map[ExecutionRole]Atom)
	seen := make(map[string]bool)
	for i, a := range f.Atoms {
		if strings.TrimSpace(a.ID) == "" || a.ID != strings.TrimSpace(a.ID) || seen[a.ID] {
			return nil, fmt.Errorf("invalid or duplicate Atom identity %q", a.ID)
		}
		seen[a.ID] = true
		if i == len(required) {
			roles[Learning] = a
			continue
		}
		want := required[i]
		if a.Type != want.typ || a.Purpose != want.purpose {
			return nil, fmt.Errorf("Atom %s: expected type %s and purpose %q; no Formula was modified", a.ID, want.typ, want.purpose)
		}
		roles[want.role] = a
	}
	return roles, nil
}

// ValidateReadiness is an execution precondition, not a historical parsing rule.
func (f Formula) ValidateReadiness() error { _, err := f.ExecutionRoles(); return err }
