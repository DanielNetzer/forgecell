package molecule

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// formulaBaselineChecks turns the approved Formula's requiredChecks into plan
// checks. Each is a required regression check whose definition and evidence are the
// workflow's frozen evidence, so it fails closed when that workflow was not frozen.
func formulaBaselineChecks(f formula.Formula, evidence []readiness.Evidence) ([]readiness.Check, error) {
	byPath := map[string]string{}
	for _, e := range evidence {
		byPath[e.Path] = e.ID
	}
	checks := make([]readiness.Check, 0, len(f.RequiredChecks))
	for _, rc := range f.RequiredChecks {
		id, ok := byPath[rc.Workflow]
		if !ok {
			return nil, fmt.Errorf("Formula baseline check %s cites workflow %s, which is not in the frozen repository evidence; refresh intake or review a Formula change", rc.ID, rc.Workflow)
		}
		checks = append(checks, readiness.Check{ID: rc.ID, Category: "regression", Argv: append([]string(nil), rc.Argv...), Dir: rc.Dir, TimeoutMS: rc.TimeoutMS, Required: true, Definitions: []string{id}, Reason: "Formula baseline: " + rc.Reason, Evidence: []string{id}})
	}
	return checks, nil
}

// applyFormulaBaseline adds every Formula baseline check to the plan and labels it in
// FormulaBaseline, whatever the analysis or the supplied checks propose. A proposed
// check that reuses a baseline id is refused rather than allowed to redefine it.
func applyFormulaBaseline(f formula.Formula, p *readiness.Plan) error {
	baseline, err := formulaBaselineChecks(f, p.Evidence)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, c := range append(append([]readiness.Check{}, p.Analysis.Checks...), p.Setup...) {
		taken[c.ID] = true
	}
	p.FormulaBaseline = nil
	for _, c := range baseline {
		if taken[c.ID] {
			return fmt.Errorf("check id %q is reserved by the Formula baseline and cannot be proposed by the analysis or supplied checks", c.ID)
		}
		p.FormulaBaseline = append(p.FormulaBaseline, c.ID)
	}
	p.Analysis.Checks = append(p.Analysis.Checks, baseline...)
	return requireFormulaBaseline(f, *p)
}

// requireFormulaBaseline refuses a plan that omits, alters or fails to label any
// Formula baseline check, or that labels a check the Formula does not declare.
func requireFormulaBaseline(f formula.Formula, p readiness.Plan) error {
	want, err := formulaBaselineChecks(f, p.Evidence)
	if err != nil {
		return err
	}
	have := map[string]readiness.Check{}
	for _, c := range p.Analysis.Checks {
		have[c.ID] = c
	}
	listed := map[string]bool{}
	for _, id := range p.FormulaBaseline {
		listed[id] = true
	}
	for _, w := range want {
		c, ok := have[w.ID]
		if !ok {
			return fmt.Errorf("plan omits Formula baseline check %s (%s); every plan must include the Formula baseline", w.ID, strings.Join(w.Argv, " "))
		}
		if !reflect.DeepEqual(c, w) {
			return fmt.Errorf("plan alters Formula baseline check %s; it must match the approved Formula exactly", w.ID)
		}
		if !listed[w.ID] {
			return fmt.Errorf("plan does not label Formula baseline check %s in formulaBaseline", w.ID)
		}
		delete(listed, w.ID)
	}
	extra := make([]string, 0, len(listed))
	for id := range listed {
		extra = append(extra, id)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("plan labels %s as Formula baseline, but the approved Formula declares no such check", strings.Join(extra, ", "))
	}
	return nil
}

// formulaBaselineNote names the baseline for the scope gate, apart from candidate checks.
func formulaBaselineNote(f formula.Formula) string {
	items := make([]string, len(f.RequiredChecks))
	for i, rc := range f.RequiredChecks {
		items[i] = fmt.Sprintf("%s (%s in %s)", rc.ID, strings.Join(rc.Argv, " "), rc.Dir)
	}
	return fmt.Sprintf("Formula baseline: %d required regression check(s) from the approved Formula are included in this plan regardless of the analysis. They are required, not candidate checks: %s.", len(items), strings.Join(items, "; "))
}
