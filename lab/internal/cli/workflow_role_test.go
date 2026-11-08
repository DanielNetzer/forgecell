package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

// saveCursorFormula writes and approves an active Formula that codes with Cursor,
// with optional per-role bindings from the caller.
func saveCursorFormula(t *testing.T, lab, cursorCommand, harnesses string) {
	t.Helper()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := "kind: formula\nid: f\nintake: {repo: acme/widgets}\nharness:\n  binding: cursor\n  command: " + cursorCommand + "\n" + harnesses + roleAtoms
	if err := os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveSavedFormula(t, lab)
}

// TestDoctorWorkflowAnalysisRole: a Formula that codes with Cursor and plans with
// a ready Codex through harnesses.analysis reports the GitHub Issue workflow from
// the analysis binding; without the override it stays unsupported, as before.
func TestDoctorWorkflowAnalysisRole(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	cursor := writeTool(t, bin, "cursor-agent", strings.NewReplacer("LOGGED", "true", "HELPFLAGS", "--sandbox --mode --trust").Replace(cursorTool))
	cursorCommand := argv(t, launcher, "__adapter", "cursor", cursor)
	codexCommand := argv(t, launcher, "__adapter", "codex", codex)

	saveCursorFormula(t, lab, cursorCommand, "harnesses:\n  analysis: {binding: codex, command: "+codexCommand+"}\n")
	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	workflow := report["workflow"].(map[string]any)
	analysis := rolesByName(t, report)["analysis"]
	if workflow["state"] != "conditional" || workflow["reason"] != analysis["capability"].(map[string]any)["reason"] {
		t.Fatalf("workflow must come from the analysis binding: %v (analysis role %v)", workflow, analysis)
	}
	if coding := rolesByName(t, report)["coding"]; coding["binding"] != "cursor" || coding["ready"] != true {
		t.Fatalf("coding readiness must come from the coding binding: %v", coding)
	}
	for _, key := range []string{"schemaVersion", "roles", "workflow", "savedBinding", "candidates", "selection", "gitReady", "githubAuthenticated"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("doctor JSON lost %s: %s", key, out)
		}
	}
	if _, text := runDoctor(t, lab, "--verbose"); !strings.Contains(text, "GitHub Issue workflow: conditional") {
		t.Fatalf("human doctor output did not read the analysis binding:\n%s", text)
	}

	saveCursorFormula(t, lab, cursorCommand, "")
	_, out = runDoctor(t, lab)
	workflow = decodeDoctor(t, out)["workflow"].(map[string]any)
	if workflow["state"] != "unsupported" || workflow["reason"] != harness.AdapterCapabilities("cursor").Analysis.Reason {
		t.Fatalf("a Formula without harnesses.analysis must keep the default binding's workflow: %v", workflow)
	}
}

// TestDoctorWorkflowAnalysisRoleSummary: the proposal summary's GitHub Issue
// workflow line reads the saved analysis-binding workflow only when the Formula
// declares harnesses.analysis.
func TestDoctorWorkflowAnalysisRoleSummary(t *testing.T) {
	cursor := onboarding.Candidate{ID: "cursor", Readiness: "ready", Capabilities: harness.AdapterCapabilities("cursor")}.CompleteUnknowns()
	planned := harness.Capability{State: "conditional", Reason: "planner ready"}
	report := onboarding.BindingReport{Candidate: cursor, Workflow: planned}
	declared := "kind: formula\nid: f\nharness: {binding: cursor, command: [/forgecell, __adapter, cursor, /cursor]}\nharnesses:\n  analysis: {binding: codex, command: [/forgecell, __adapter, codex, /codex]}\n" + roleAtoms
	if got := bindingWorkflow(report, declared); got != planned {
		t.Fatalf("declared analysis: %v", got)
	}
	plain := "kind: formula\nid: f\nharness: {binding: cursor, command: [/forgecell, __adapter, cursor, /cursor]}\n" + roleAtoms
	if got := bindingWorkflow(report, plain); got != cursor.WorkflowCapability() {
		t.Fatalf("no analysis override must keep the default's workflow: %v", got)
	}
	if got := bindingWorkflow(report, "not: [a formula"); got != cursor.WorkflowCapability() {
		t.Fatalf("unparsable YAML must keep the default's workflow: %v", got)
	}
}
