package readiness

import (
	"fmt"
	"path"
	"strings"
)

// InScopeDefinitionMessage prefixes the intake rejection of a check defined by a
// file the same plan proposes to edit. The corrective retry receives it verbatim.
const InScopeDefinitionMessage = "drop or replace checks defined by in-scope files"

// IsDocumentationPath reports whether file is prose documentation by extension.
// A check that cites such a file in its definitions treats it as information, not
// as something it executes or reads, so the citation never counts as a definition.
// A documentation path named in the command's argv is still a path the check reads.
func IsDocumentationPath(file string) bool {
	switch strings.ToLower(path.Ext(file)) {
	case ".md", ".markdown", ".mdx", ".rst", ".adoc":
		return true
	}
	return false
}

// BaselineIDMessage prefixes the intake rejection of a proposed check whose id is
// already taken by a Formula baseline check. The corrective retry receives it verbatim.
const BaselineIDMessage = "check ids reserved by the Formula baseline cannot be proposed"

// ValidateBaselineIDs rejects, as a retryable policy violation, a proposed check
// whose id equals a Formula baseline check id. The Lab adds every baseline check
// itself, so the duplicate can simply be dropped.
func ValidateBaselineIDs(a Analysis, baselineIDs []string) error {
	var found []string
	for _, c := range a.Checks {
		if containsID(baselineIDs, c.ID) && !containsID(found, c.ID) {
			found = append(found, c.ID)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return &PolicyViolation{Reason: BaselineIDMessage + ": " + strings.Join(found, ", ") + "; drop the duplicate (the Lab already includes it) or give the new check a different id"}
}

// definitionAnchored reports whether a check has a definition that keeps it
// verifiable when a documentation file it cites is edited: a frozen file that is
// neither in scope nor documentation, or a new file this plan adds (a definition
// that is not frozen evidence).
func definitionAnchored(c Check, refs map[string]string, scope map[string]bool) bool {
	for _, id := range c.Definitions {
		file, frozen := refs[id]
		if !frozen {
			if id != "" && id != "issue" {
				return true
			}
			continue
		}
		if !scope[file] && !IsDocumentationPath(file) {
			return true
		}
	}
	return false
}

// ValidateInScopeDefinitions rejects a check whose definition evidence or exact argv path is in scope; test paths named in argv are exempt, as in the run-time guard.
// Documentation files get no exemption here: the auto-approval policy declines any
// plan whose check cites an in-scope file.
func ValidateInScopeDefinitions(a Analysis, evidence []Evidence) error {
	return validateInScopeDefinitions(a, evidence, nil, false)
}

// ValidateInScopeDefinitionsForNewFiles is ValidateInScopeDefinitions except that
// a scope entry listed in newFiles may define a check. Such a file is absent from
// the frozen tree, so nothing frozen is edited and the check stays candidate
// evidence; every existing in-scope file still rejects. The caller derives
// newFiles from the frozen tree (see NewScopeFiles), never from model output.
//
// A documentation file cited as a definition is informational and does not reject
// by itself, but a check that cites an in-scope documentation file and has no
// other anchor (see definitionAnchored) is rejected.
func ValidateInScopeDefinitionsForNewFiles(a Analysis, evidence []Evidence, newFiles []string) error {
	return validateInScopeDefinitions(a, evidence, newFiles, true)
}

func validateInScopeDefinitions(a Analysis, evidence []Evidence, newFiles []string, docsInformational bool) error {
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
		anchored := definitionAnchored(c, refs, scope)
		for _, id := range c.Definitions {
			if file, ok := refs[id]; ok && (!docsInformational || !IsDocumentationPath(file) || !anchored) {
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
