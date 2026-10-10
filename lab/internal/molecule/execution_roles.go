package molecule

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

type executionRoles struct {
	atoms map[formula.ExecutionRole]int
	order []formula.ExecutionRole
}

// resolveExecutionRoles verifies exact snapshot bytes and complete ledger
// membership. It adds no provenance and never modifies historical records.
func resolveExecutionRoles(r Record) (executionRoles, error) {
	f, err := formula.Parse([]byte(r.FormulaSnapshot.YAML))
	if err != nil {
		return executionRoles{}, fmt.Errorf("invalid execution Formula snapshot: %w", err)
	}
	if f.SHA256 != r.FormulaSnapshot.SHA256 || f.ID != r.FormulaID {
		return executionRoles{}, fmt.Errorf("execution Formula snapshot identity mismatch")
	}
	if r.Readiness != nil {
		for _, p := range r.Readiness.Plans {
			if p.Plan.Inputs.FormulaSHA256 != f.SHA256 {
				return executionRoles{}, fmt.Errorf("plan Formula hash does not match exact snapshot")
			}
		}
	}
	roles, err := f.ExecutionRoles()
	if err != nil {
		return executionRoles{}, err
	}
	if len(r.Atoms) != len(f.Atoms) {
		return executionRoles{}, fmt.Errorf("recorded Atom membership differs from Formula")
	}
	indices := make(map[string]int)
	for i, a := range r.Atoms {
		if _, ok := indices[a.ID]; ok {
			return executionRoles{}, fmt.Errorf("duplicate recorded Atom identity %q", a.ID)
		}
		indices[a.ID] = i
	}
	resolved := executionRoles{atoms: make(map[formula.ExecutionRole]int)}
	for role, a := range roles {
		i, ok := indices[a.ID]
		if !ok || r.Atoms[i].Type != a.Type {
			return executionRoles{}, fmt.Errorf("recorded Atom %q identity/type differs from Formula", a.ID)
		}
		resolved.atoms[role] = i
	}
	for _, a := range f.Atoms {
		for role, declared := range roles {
			if declared.ID == a.ID {
				resolved.order = append(resolved.order, role)
				break
			}
		}
	}
	return resolved, nil
}

func (roles executionRoles) after(role formula.ExecutionRole) []int {
	// The order comes from the validated Formula, independently of ledger storage.
	found := false
	var result []int
	for _, next := range roles.order {
		if found {
			if i, ok := roles.atoms[next]; ok {
				result = append(result, i)
			}
		}
		if next == role {
			found = true
		}
	}
	return result
}
