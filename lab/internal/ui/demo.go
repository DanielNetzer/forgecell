package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"strconv"
	"strings"
)

// Demo mode serves the forest-glass prototype's synthetic Molecules. Every
// response carries sample=true and the UI labels it Sample data. None of it is
// read from, or written to, a Lab, and real mode never calls these functions.

const demoCount = 196

var demoTitles = []string{"Clarify setup instructions", "Check CLI documentation", "Explain Formula approval", "Document ledger inspection", "Review installation examples", "Improve troubleshooting guide"}

var demoSteps = []string{"intake", "harness", "check", "gate", "ship", "document"}

var demoEvidence = []string{"Issue text and local guidance retained.", "Changes limited to the reviewed checkout.", "Commands and results retained in the ledger.", "No publication occurs while this gate waits.", "Publication is distinct from merge or deployment.", "Commenting requires the approved delivery scope."}

// demoPhase follows the prototype's generator: four fixed runs, then a mix.
func demoPhase(i int) string {
	switch {
	case i < 4:
		return []string{"decision", "working", "finished", "failed"}[i]
	case i%11 == 0:
		return "failed"
	case i%13 == 0:
		return "decision"
	case i%3 == 0 || i%5 == 0:
		return "working"
	}
	return "finished"
}

func demoTitle(i int) string {
	if i < 4 {
		return []string{"Fix retry handling", "Clarify installation", "Check documentation links", "Bound ledger inspection"}[i]
	}
	return demoTitles[i%len(demoTitles)]
}

func demoFormula(i int) string {
	switch {
	case i == 0:
		return "github-issue"
	case i%2 == 1:
		return "docs-change"
	}
	return "repository-care"
}

// demoState maps a prototype phase to an engine-shaped status, phase and receipt.
func demoState(p string) (status, phase string, d *DeliveryState) {
	switch p {
	case "working":
		return "running", "coding", nil
	case "decision":
		return "waiting", "review-waiting", nil
	case "failed":
		return "blocked", "blocked", nil
	}
	return "completed", "", &DeliveryState{State: "published", Lifecycle: "OPEN"}
}

func demoAtomStates(p string) []string {
	switch p {
	case "working":
		return []string{"Complete", "Running", "Queued", "Waiting", "Not started", "Not started"}
	case "finished":
		return []string{"Complete", "Complete", "Passed", "Approved", "Draft published", "Recorded"}
	case "failed":
		return []string{"Complete", "Complete", "Failed", "Blocked", "Skipped", "Skipped"}
	}
	return []string{"Complete", "Complete", "Passed", "Needs you", "Not started", "Not started"}
}

func demoCurrent(p string) string {
	return map[string]string{"working": "harness", "decision": "gate", "failed": "check"}[p]
}

func demoSummary(i int) Summary {
	p := demoPhase(i)
	status, phase, d := demoState(p)
	return Summary{ID: fmt.Sprintf("demo-%d", 101+i), Repository: "sample/lab", Number: int64(101 + i), Title: demoTitle(i), FormulaID: demoFormula(i), Status: status, Phase: phase, CurrentAtom: demoCurrent(p), Lane: Lane(status, phase, d), Delivery: DeliveryLabel(d)}
}

func demoLab(version string) LabView {
	return LabView{Sample: true, ReadOnly: true, Name: "Sample Lab", Version: version, Repository: "sample/lab", Binding: "sample binding", Formula: formula.Identity{State: "sample", FormulaID: "repository-care", Name: "repository-care.yaml"}, Unavailable: unavailable}
}

func demoList() MoleculeList {
	out := MoleculeList{Sample: true, ETag: `"sample"`, Complete: true, Diagnostics: []string{}, Molecules: make([]Summary, 0, demoCount)}
	for i := 0; i < demoCount; i++ {
		out.Molecules = append(out.Molecules, demoSummary(i))
	}
	out.Counts = counts(out.Molecules)
	return out
}

// demoAnalytics is derived only from demoList. Sample Molecules record no times
// and no harness attempts, so every time and cost measure reads Not recorded or
// Unavailable with 0 of N coverage; nothing is invented.
func demoAnalytics() AnalyticsView {
	return buildAnalytics(inventory{list: demoList()})
}

func demoDetail(id string, withYAML bool) (Detail, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "demo-"))
	if !strings.HasPrefix(id, "demo-") || err != nil || n < 101 || n >= 101+demoCount {
		return Detail{}, false
	}
	i := n - 101
	s := demoSummary(i)
	p := demoPhase(i)
	_, _, d := demoState(p)
	out := Detail{Sample: true, ID: s.ID, Status: s.Status, Phase: s.Phase, Lane: s.Lane, Mode: "sample", FormulaID: s.FormulaID, CurrentAtom: s.CurrentAtom, Diagnostics: []string{"Sample data: synthetic design fixture, not Lab evidence."}, Attempts: []AttemptView{}}
	out.Issue = IssueView{Repository: s.Repository, Number: s.Number, Title: s.Title, Body: "Synthetic sample Issue. No GitHub Issue exists for this number.", State: "SAMPLE"}
	out.Workspace = WorkspaceView{Repository: s.Repository, Branch: "sample/" + s.ID, BaseCommit: "sample"}
	out.Formula = FormulaView{SHA256: "sample"}
	if withYAML {
		out.Formula.YAML = "# Design fixture — not executable Forgecell configuration\nid: " + s.FormulaID + "\natoms:\n  - intake\n  - harness\n  - check\n  - gate\n  - ship\n  - document\n"
	}
	out.Readiness = &ReadinessView{Phase: s.Phase, Plans: []PlanView{{Revision: 1, Digest: "sample"}}, Events: []EventView{{Kind: "proposed", Digest: "sample", Detail: "Sample event"}}}
	for k, state := range demoAtomStates(p) {
		out.Atoms = append(out.Atoms, AtomView{ID: demoSteps[k], Type: demoSteps[k], Status: state, Detail: demoEvidence[k] + " (sample)"})
	}
	if d != nil {
		out.Delivery = &DeliveryView{State: d.State, Lifecycle: d.Lifecycle, Published: d.Published(), Label: DeliveryLabel(d)}
	}
	// Sample data has no gates: its gate state is published or none, never open.
	out.Gate = GateView{State: GateNone}
	if d.Published() {
		out.Gate = GateView{State: GatePublished, Receipt: d.State, Lifecycle: d.Lifecycle}
	}
	out.Next = nextAction(out.Lane, "", out.Status, out.Phase, out.CurrentAtom, out.Gate, out.Delivery)
	return out, true
}
