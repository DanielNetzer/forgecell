package molecule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func executedBindings(r Record, atomID string) map[string]string {
	result := map[string]string{}
	for _, a := range r.Atoms {
		if a.ID == atomID && a.Provenance != nil {
			for _, action := range a.Provenance.Actions {
				result[action.AttemptID] = action.ExecutedBinding
			}
		}
	}
	return result
}

func TestMultiHarnessRoles(t *testing.T) {
	o, repo := fixture(t)
	root := filepath.Dir(repo)
	marker := func(name string) string { return filepath.Join(root, name+".calls") }
	script := func(name, body string) string {
		path := filepath.Join(root, name)
		os.WriteFile(path, []byte("#!/bin/sh\ncat > request.json\necho run >> "+marker(name)+"\n"+body), 0700)
		return path
	}
	completed := "printf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"
	fallback := script("fallback", "exit 9\n")
	analyzer := script("analyzer", "exit 9\n")
	coder := script("coder", "printf changed > code.txt\n"+completed)
	command := func(path string) string { b, _ := json.Marshal([]string{path}); return string(b) }
	recipe := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: fallback\n  command: " + command(fallback) + "\nharnesses:\n  analysis: {binding: analyst, command: " + command(analyzer) + "}\n  coding: {binding: coder, command: " + command(coder) + "}\natoms:\n  - {id: intake, type: intake, binding: analyst}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness, binding: coder}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
	approveFixture(t, o.LabDir, recipe, "multi-harness")

	var analysisBindings []string
	base := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		analysisBindings = append(analysisBindings, b.Binding)
		return base(c, b, i, s)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" {
		t.Fatalf("%s: %+v", r.Status, r.Notes)
	}
	if len(analysisBindings) != 1 || analysisBindings[0] != "analyst" {
		t.Fatalf("analysis ran on %v, want the analysis binding", analysisBindings)
	}
	if data, _ := os.ReadFile(marker("coder")); strings.Count(string(data), "run") != 1 {
		t.Fatalf("coding script calls = %q, want exactly one", data)
	}
	for _, name := range []string{"fallback", "analyzer"} {
		if _, err := os.Stat(marker(name)); !os.IsNotExist(err) {
			t.Fatalf("the %s binding was invoked although no role resolves to it", name)
		}
	}
	if got := executedBindings(r, "intake")["analysis-1"]; got != "analyst" {
		t.Fatalf("intake executed binding = %q", got)
	}
	coding := executedBindings(r, "harness")
	if len(coding) != 1 {
		t.Fatalf("coding actions: %v", coding)
	}
	for _, binding := range coding {
		if binding != "coder" {
			t.Fatalf("coding executed binding = %q", binding)
		}
	}
	saved, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil || !strings.Contains(string(saved), `"executedBinding": "analyst"`) && !strings.Contains(string(saved), `"executedBinding":"analyst"`) || !strings.Contains(string(saved), "coder") {
		t.Fatalf("ledger omits the executed bindings: %v", err)
	}
}

func TestMultiHarnessRolesSingleBindingRecordsDefault(t *testing.T) {
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := executedBindings(r, "intake")["analysis-1"]; got != "custom" {
		t.Fatalf("intake executed binding = %q, want the default", got)
	}
	for _, binding := range executedBindings(r, "harness") {
		if binding != "custom" {
			t.Fatalf("coding executed binding = %q, want the default", binding)
		}
	}
	for _, id := range []string{"scope", "check", "gate", "ship", "document"} {
		for attempt, binding := range executedBindings(r, id) {
			if binding != "" {
				t.Fatalf("%s/%s claims binding %q", id, attempt, binding)
			}
		}
	}
}

func TestMultiHarnessRolesLegacyLedgerActionsRoundTrip(t *testing.T) {
	legacy := `{"attemptId":"analysis-1","plannedAction":"analyze-ticket","startedAt":"2026-01-01T00:00:00Z"}`
	var action AtomAction
	if err := json.Unmarshal([]byte(legacy), &action); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(action)
	if err != nil || strings.Contains(string(out), "executedBinding") {
		t.Fatalf("legacy action gained a binding field: %s %v", out, err)
	}
}

func TestMultiHarnessRolesRejectsConflictingAtomBinding(t *testing.T) {
	o, _ := fixture(t)
	recipe := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: custom\n  command: [/bin/true]\nharnesses:\n  coding: {binding: coder, command: [/bin/true]}\natoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness, binding: other}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
	approveFixture(t, o.LabDir, recipe, "conflict")
	calls := 0
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		return readiness.Analysis{}, nil
	}
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "conflicts") || calls != 0 {
		t.Fatalf("conflicting role binding ran: error=%v calls=%d", err, calls)
	}
}
