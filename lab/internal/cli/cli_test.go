package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}} {
		var out, stderr bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test-version")
		if code != 0 || out.Len() == 0 {
			t.Fatalf("code %d: %s", code, stderr.String())
		}
	}
}
func TestLedgerInspectionIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ledgers"), 0700)
	raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-test","formulaId":"f","status":"waiting","extra":"preserve"}`)
	file := filepath.Join(dir, "ledgers", "mol-test.json")
	os.WriteFile(file, raw, 0600)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"ledger", "mol-test", "--lab", dir}, strings.NewReader(""), &out, &stderr, "dev")
	if code != 0 || !strings.Contains(out.String(), "preserve") {
		t.Fatalf("%d %s %s", code, out.String(), stderr.String())
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(after, raw) {
		t.Fatal("reading rewrote ledger")
	}
	if code = Run(context.Background(), []string{"ledger", "../escape", "--lab", dir}, strings.NewReader(""), &out, &stderr, "dev"); code == 0 {
		t.Fatal("unsafe id accepted")
	}
}

func TestDoctorPreservesSavedBinding(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "codex", "claude", "cursor-agent"} {
		body := "#!/bin/sh\nprintf '%s\\n' 'Codex Claude Code Cursor Agent logged in --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --mode --trust'\n"
		if name == "claude" {
			body = "#!/bin/sh\ncase \"$1\" in auth) printf '%s\\n' '{\"loggedIn\":true}' ;; *) printf '%s\\n' 'Claude Code --print --output-format --permission-mode --tools --json-schema --no-session-persistence' ;; esac\n"
		}
		if e := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CURSOR_AGENT", "")
	t.Setenv("TERM_PROGRAM", "")
	lab := t.TempDir()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := []byte("kind: formula\nid: f\nharness:\n  binding: codex\n  command: [/missing/old-forgecell, __adapter, codex, /missing/saved-codex]\natoms:\n  - type: harness\n")
	file := filepath.Join(lab, "formulas/f.yaml")
	os.WriteFile(file, recipe, 0600)
	for _, extra := range [][]string{nil, {"--harness", "claude-code"}} {
		var out, err bytes.Buffer
		args := append([]string{"doctor", "--lab", lab, "--json"}, extra...)
		code := Run(context.Background(), args, strings.NewReader(""), &out, &err, "dev")
		if code != 2 || !strings.Contains(out.String(), `"savedBinding":{"id":"codex","readiness":"blocked"`) {
			t.Fatalf("%d %s %s", code, out.String(), err.String())
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(after, recipe) {
		t.Fatal("doctor changed Formula")
	}
}

func TestSavedProposalReviewOutsideRepositoryIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	recipe := "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: codex}\natoms: [{id: scope, type: gate, mode: human}, {id: tests, type: check, command: [go, test, ./...]}]\n"
	p, err := onboarding.SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "assays", p.ID+".json")
	before, _ := os.ReadFile(file)
	t.Setenv("PATH", t.TempDir())
	if err = os.Mkdir(filepath.Join(dir, ".formula-write.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, jsonMode := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"init", "--lab", dir, "--review", p.ID}
		if jsonMode {
			args = append(args, "--json")
		}
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev")
		if code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		if jsonMode {
			var view struct {
				Verified   bool                `json:"verified"`
				Proposal   onboarding.Proposal `json:"proposal"`
				NextAction string              `json:"nextAction"`
			}
			if err = json.Unmarshal(out.Bytes(), &view); err != nil || !view.Verified || view.Proposal.YAML != recipe || !strings.Contains(view.NextAction, "--approve") {
				t.Fatalf("%s %v", out.String(), err)
			}
		} else {
			for _, text := range []string{recipe, p.YAMLHash, "acme/widgets", "codex", "human", "tests", "--approve", "--review", "Historical capability evidence was not recorded", "saved observation; not current verification", "Model access: unknown", "Meta-learning adapter: unknown"} {
				if !strings.Contains(out.String(), text) {
					t.Fatalf("missing %q: %s", text, out.String())
				}
			}
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("review wrote proposal")
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("review activated")
	}
	p.Status = "dismissed"
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--lab", dir, "--review", p.ID}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || strings.Contains(out.String(), "--approve") {
		t.Fatalf("%d %s %s", code, out.String(), stderr.String())
	}
	p.YAML += "# tampered"
	raw, _ = json.Marshal(p)
	os.WriteFile(file, raw, 0600)
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"init", "--lab", dir, "--review", p.ID, "--json"}, strings.NewReader(""), &out, &stderr, "dev"); code == 0 || out.Len() != 0 {
		t.Fatalf("unverified output: %d %s", code, out.String())
	}
}

func TestSavedReviewActivationCrashWindows(t *testing.T) {
	for _, stage := range []string{"intent", "applied", "approved", "complete"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\natoms: [{type: harness}]\n")
			if err != nil {
				t.Fatal(err)
			}
			a := formula.Approval{FormulaID: "sample", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
			if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
				t.Fatal(err)
			}
			if stage != "intent" {
				p.Applying = true
				if err = formula.ApplyActivation(dir, a); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "approved" || stage == "complete" {
				p.Status = "approved"
			}
			if stage == "complete" {
				if err = formula.CompleteActivation(dir, a); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(dir, "assays", p.ID+".json")
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			decision := filepath.Join(dir, "formula-decisions", p.ID+".json")
			before, err := os.ReadFile(decision)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			for _, jsonMode := range []bool{false, true} {
				var out, stderr bytes.Buffer
				args := []string{"init", "--lab", dir, "--review", p.ID}
				if jsonMode {
					args = append(args, "--json")
				}
				if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
					t.Fatalf("%d %s", code, stderr.String())
				}
				next := out.String()
				if jsonMode {
					var view struct {
						NextAction string `json:"nextAction"`
					}
					if err = json.Unmarshal(out.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
					next = view.NextAction
				}
				if stage == "complete" {
					if strings.Contains(next, "--approve") || strings.Contains(next, "--dismiss") {
						t.Fatalf("completed activation requests decision: %s", next)
					}
				} else if !strings.Contains(next, fmt.Sprintf("forgecell init --lab %q --approve %s", dir, p.ID)) || strings.Contains(next, "--dismiss") {
					t.Fatalf("incomplete activation lacks exact resume action: %s", next)
				}
			}
			after, _ := os.ReadFile(decision)
			saved, _ := os.ReadFile(file)
			if !bytes.Equal(before, after) || !bytes.Equal(raw, saved) {
				t.Fatal("review mutated saved activation or proposal")
			}
		})
	}
}

func TestSavedCursorReviewExplainsWorkflowLimitation(t *testing.T) {
	dir := t.TempDir()
	p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: cursor, command: [/forgecell, __adapter, cursor, /cursor]}\natoms: [{type: gate}]\n")
	if err != nil {
		t.Fatal(err)
	}
	c := onboarding.Candidate{ID: "cursor", Readiness: "ready", Detail: "Authenticated CLI probe; model access unverified.", Capabilities: harness.AdapterCapabilities("cursor")}
	p.CapabilityEvidence = &onboarding.BindingReport{Candidate: c, Command: []string{"/forgecell", "__adapter", "cursor", "/cursor"}, Workflow: c.WorkflowCapability()}
	t.Setenv("PATH", t.TempDir())
	for _, jsonMode := range []bool{false, true} {
		var out, stderr bytes.Buffer
		if code := renderProposal(&out, &stderr, p, dir, jsonMode); code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		for _, want := range []string{"action-capable MCP/plugin isolation has not been verified", "saved observation; not current verification", "unknown", "unsupported"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %s: %s", want, out.String())
			}
		}
	}
}

// Candidate documentation evidence; this does not establish independent acceptance.
func TestOnboardingDocumentationConsistency(t *testing.T) {
	readDoc := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(data)), " ")
	}
	guide, atoms := readDoc("agent-onboarding.md"), readDoc("atoms.md")
	require := func(doc, claim string) {
		t.Helper()
		if !strings.Contains(doc, claim) {
			t.Errorf("missing contract claim %q", claim)
		}
	}
	dir := t.TempDir()
	recipe := "kind: formula\nid: documentation-fixture\nintake: {repo: acme/widgets}\nharness: {binding: codex}\natoms: [{type: harness}, {type: check}, {type: gate}]\n"
	p, err := onboarding.SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	// No discovery executables are available; review must use saved bytes only.
	t.Setenv("PATH", t.TempDir())
	var out, stderr bytes.Buffer
	args := []string{"init", "--lab", dir, "--review", p.ID, "--json"}
	if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("review exit=%d: %s", code, &stderr)
	}
	var view map[string]json.RawMessage
	if err = json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proposal", "verified", "repository", "harness", "atoms", "repositoryContext", "reviewCommand", "nextAction", "capabilityEvidence", "capabilityEvidenceSource"} {
		if _, ok := view[key]; !ok {
			t.Errorf("review missing %s", key)
		}
		require(guide, "`"+key+"`")
	}
	var next, source string
	if err = json.Unmarshal(view["nextAction"], &next); err != nil || !strings.Contains(next, "--approve") {
		t.Fatalf("nextAction is not descriptive decision text: %s", view["nextAction"])
	}
	if err = json.Unmarshal(view["capabilityEvidenceSource"], &source); err != nil {
		t.Fatal(err)
	}
	require(guide, source)
	var saved onboarding.Proposal
	if err = json.Unmarshal(view["proposal"], &saved); err != nil || saved.YAML != recipe || saved.YAMLHash != p.YAMLHash {
		t.Fatal("review did not retain exact YAML and hash")
	}
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), args[:len(args)-1], strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("terminal review exit=%d: %s", code, &stderr)
	}
	for _, claim := range []string{recipe, p.YAMLHash, source, "Learn command bound: false"} {
		if !strings.Contains(out.String(), claim) {
			t.Errorf("terminal review missing %q", claim)
		}
	}
	// Preparation has its own JSON shape, distinct from saved review and decisions.
	raw, err := json.Marshal(onboarding.Outcome{Status: "pending", Proposal: &p})
	if err != nil {
		t.Fatal(err)
	}
	var preparation map[string]json.RawMessage
	if err = json.Unmarshal(raw, &preparation); err != nil {
		t.Fatal(err)
	}
	for key := range preparation {
		require(guide, "`"+key+"`")
	}
	if _, ok := preparation["nextAction"]; ok {
		t.Fatal("preparation unexpectedly exposes nextAction")
	}
	for _, claim := range []string{"Initial init output has no `nextAction`", "`nextAction` is a descriptive string", "Both terminal and JSON modes are noninteractive", "no learn Atom command", "verified: true", "ticket scope, checks and publication require their separate approvals"} {
		require(guide, claim)
	}
	for _, obsolete := range []string{"**argv array**", "interactive Approve / Edit / Deny flow", "proposed coding and learn bindings"} {
		if strings.Contains(guide, obsolete) {
			t.Errorf("obsolete promise %q", obsolete)
		}
	}
	for _, claim := range []string{"fresh checkout", "temporary HOME", "unrestricted network", "review gate waiting", "publication Atoms remain skipped", "Passing candidate checks does not establish independent acceptance", "exact PR head"} {
		require(atoms, claim)
	}
	if strings.Contains(atoms, "Reads recent GitHub Actions runs as context") || strings.Contains(atoms, "Unbound runs remain record-only") {
		t.Error("obsolete Atom behavior")
	}
}
