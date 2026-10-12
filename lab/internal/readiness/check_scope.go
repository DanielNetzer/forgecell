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
	return ValidateInScopeDefinitionsForNewFiles(a, evidence, nil)
}

// ValidateInScopeDefinitionsForNewFiles is ValidateInScopeDefinitions except that
// a scope entry listed in newFiles may define a check. Such a file is absent from
// the frozen tree, so nothing frozen is edited and the check stays candidate
// evidence; every existing in-scope file still rejects. The caller derives
// newFiles from the frozen tree (see NewScopeFiles), never from model output.
func ValidateInScopeDefinitionsForNewFiles(a Analysis, evidence []Evidence, newFiles []string) error {
	scope := map[string]bool{}
	for _, s := range a.Scope {
		scope[s.Path] = true
	}
	for _, file := range newFiles {
		delete(scope, file)
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

// DefinitionReferenceMessage prefixes the intake rejection of a check whose
// definition is the issue rather than a file; Plan.Validate rejects it the same way.
const DefinitionReferenceMessage = "command definitions must reference frozen files"

// ValidateDefinitionReferences rejects, as a retryable policy violation, a check
// defined by the issue. The message names the two accepted alternatives, so the
// corrective retry can cite an existing frozen file or a new in-scope file.
func ValidateDefinitionReferences(a Analysis) error {
	var found []string
	for _, c := range a.Checks {
		if containsID(c.Definitions, "issue") {
			found = append(found, c.ID)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return &PolicyViolation{Reason: DefinitionReferenceMessage + " (" + strings.Join(found, ", ") + " cites the issue); cite an existing frozen evidence ID such as the parent manifest or workflow, or, for a new file this plan adds, the exact scope path of that new file"}
}

// NewScopeFiles lists the scope paths that are absent from the frozen tree, in
// scope order. A path is new only when the tree has no entry for it.
func NewScopeFiles(scope []ScopedPath, tree []TreeFile) []string {
	existing := map[string]bool{}
	for _, f := range tree {
		existing[f.Path] = true
	}
	var out []string
	for _, s := range scope {
		if !existing[s.Path] && !containsID(out, s.Path) {
			out = append(out, s.Path)
		}
	}
	return out
}

// NewFileOnlyChecks returns the checks whose every definition is a member of
// newFiles: they have no frozen anchor at all.
func NewFileOnlyChecks(checks []Check, newFiles []string) []Check {
	var out []Check
	for _, c := range checks {
		if len(c.Definitions) == 0 {
			continue
		}
		only := true
		for _, id := range c.Definitions {
			only = only && containsID(newFiles, id)
		}
		if only {
			out = append(out, c)
		}
	}
	return out
}

// CitedNewFiles returns the members of newFiles that some check cites as a
// definition by exact path, in newFiles order.
func CitedNewFiles(checks []Check, newFiles []string) []string {
	var out []string
	for _, file := range newFiles {
		for _, c := range checks {
			if containsID(c.Definitions, file) {
				out = append(out, file)
				break
			}
		}
	}
	return out
}
