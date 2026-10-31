package molecule

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
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
		checks = append(checks, readiness.Check{ID: rc.ID, Category: "regression", Argv: append([]string(nil), rc.Argv...), Dir: rc.Dir, TimeoutMS: rc.TimeoutMS, Required: true, Definitions: []string{id}, Reason: "Formula baseline: " + rc.Reason, Evidence: []string{id}, Select: rc.Select})
	}
	return checks, nil
}

// gitlabCIWorkflow is the one non-GitHub workflow a RequiredCheck may cite.
const gitlabCIWorkflow = ".gitlab-ci.yml"

// formulaCollectOptions collects the root .gitlab-ci.yml only when the approved
// Formula's baseline cites it, so a stray file in a GitHub repository is never
// collected and GitHub evidence digests never change.
func formulaCollectOptions(f formula.Formula) readiness.CollectOptions {
	for _, rc := range f.RequiredChecks {
		if rc.Workflow == gitlabCIWorkflow {
			return readiness.CollectOptions{GitLabCI: true}
		}
	}
	return readiness.CollectOptions{}
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
	applyFullSuiteTriggers(f, p)
	return requireFormulaBaseline(f, *p)
}

// applyFullSuiteTriggers places the Formula's resolved fullSuiteTriggers on a plan with
// a check that declares select, so verification reads them and they join the digest.
// A plan without one keeps none, and its digest is unchanged.
func applyFullSuiteTriggers(f formula.Formula, p *readiness.Plan) {
	p.FullSuiteTriggers = nil
	for _, c := range append(append([]readiness.Check{}, p.Analysis.Checks...), p.Setup...) {
		if c.Select != "" {
			p.FullSuiteTriggers = f.ResolvedFullSuiteTriggers()
			return
		}
	}
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
	return requireFormulaSelection(f, p)
}

// requireFormulaSelection refuses a plan whose check selection the approved Formula or
// a frozen evaluation plan did not declare. A check may carry select only as a Formula
// baseline check (requireFormulaBaseline binds its exact value) or as a command of the
// cited evaluation-plan check input declaring the same name, dir, argv and select. The
// plan's fullSuiteTriggers must be exactly what the Formula resolves to, so neither an
// amendment nor supplied verification inputs can drop, replace or add triggers.
func requireFormulaSelection(f formula.Formula, p readiness.Plan) error {
	baseline := map[string]bool{}
	for _, id := range p.FormulaBaseline {
		baseline[id] = true
	}
	for _, c := range p.Setup {
		if c.Select != "" {
			return fmt.Errorf("setup command %s cannot declare select", c.ID)
		}
	}
	for _, c := range p.Analysis.Checks {
		if c.Select == "" || baseline[c.ID] {
			continue
		}
		if !evaluationPlanDeclaresSelect(p, c) {
			return fmt.Errorf("check %s declares select, but neither the approved Formula baseline nor the frozen evaluation plan declares that selection for it", c.ID)
		}
	}
	want := p
	applyFullSuiteTriggers(f, &want)
	if !slices.Equal(p.FullSuiteTriggers, want.FullSuiteTriggers) {
		return fmt.Errorf("plan fullSuiteTriggers must match the approved Formula exactly")
	}
	return nil
}

// evaluationPlanCheckInput is the check input id under which an evaluation run freezes
// its reviewed plan (evaluation.configureReadiness).
const evaluationPlanCheckInput = "evaluation-plan"

// evaluationPlanDeclaresSelect reports whether c cites the frozen evaluation-plan check
// input and that input declares a command with c's exact name, dir, argv and select.
// The content is bounded by readiness.MaxCheckInputBytes and hash-checked at intake.
func evaluationPlanDeclaresSelect(p readiness.Plan, c readiness.Check) bool {
	if !slices.Contains(c.Definitions, evaluationPlanCheckInput) {
		return false
	}
	for _, input := range p.CheckInputs {
		if input.ID != evaluationPlanCheckInput || len(input.Content) > readiness.MaxCheckInputBytes {
			continue
		}
		var plan struct {
			Checks []struct {
				Name   string   `json:"name"`
				Dir    string   `json:"dir"`
				Argv   []string `json:"argv"`
				Select string   `json:"select"`
			} `json:"checks"`
		}
		if json.Unmarshal([]byte(input.Content), &plan) != nil {
			return false
		}
		for _, command := range plan.Checks {
			if command.Name == c.ID && command.Name != "acceptance" && command.Dir == c.Dir && command.Select == c.Select && slices.Equal(command.Argv, c.Argv) {
				return true
			}
		}
	}
	return false
}

// formulaBaselineNote names the baseline for the scope gate, apart from candidate checks.
func formulaBaselineNote(f formula.Formula) string {
	items := make([]string, len(f.RequiredChecks))
	for i, rc := range f.RequiredChecks {
		items[i] = fmt.Sprintf("%s (%s in %s)", rc.ID, strings.Join(rc.Argv, " "), rc.Dir)
	}
	return fmt.Sprintf("Formula baseline: %d required regression check(s) from the approved Formula are included in this plan regardless of the analysis. They are required, not candidate checks: %s.", len(items), strings.Join(items, "; "))
}
