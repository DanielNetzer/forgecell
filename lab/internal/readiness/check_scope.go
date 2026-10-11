package readiness

import (
	"fmt"
	"path"
	"strings"
)

// InScopeDefinitionMessage prefixes the intake rejection of a check defined by a
// file the same plan proposes to edit. The corrective retry receives it verbatim.
const InScopeDefinitionMessage = "drop or replace checks defined by in-scope files"

// ValidateInScopeDefinitions rejects a check whose definition evidence or exact argv path is in scope; test paths named in argv are exempt, as in the run-time guard.
func ValidateInScopeDefinitions(a Analysis, evidence []Evidence) error {
	scope := map[string]bool{}
	for _, s := range a.Scope {
		scope[s.Path] = true
	}
	refs := map[string]string{}
	for _, e := range evidence {
		refs[e.ID] = e.Path
	}
	var order []string
	byPath := map[string][]string{}
	add := func(file, check string) {
		if !scope[file] || containsID(byPath[file], check) {
			return
		}
		if len(byPath[file]) == 0 {
			order = append(order, file)
		}
		byPath[file] = append(byPath[file], check)
	}
	for _, c := range a.Checks {
		for _, id := range c.Definitions {
			if file, ok := refs[id]; ok {
				add(file, c.ID)
			}
		}
		for _, arg := range c.Argv {
			if arg == "" || strings.HasPrefix(arg, "-") || path.IsAbs(arg) {
				continue
			}
			for _, file := range []string{path.Clean(arg), path.Join(c.Dir, arg)} {
				if !ConventionalTestPath(file) {
					add(file, c.ID)
				}
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	found := make([]string, len(order))
	for i, file := range order {
		found[i] = fmt.Sprintf("%s (%s)", file, strings.Join(byPath[file], ", "))
	}
	return &PolicyViolation{Reason: InScopeDefinitionMessage + ": " + strings.Join(found, "; ")}
}
