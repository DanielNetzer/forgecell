package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const roleAtoms = "atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"

func argv(t *testing.T, items ...string) string {
	t.Helper()
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// saveRoleFormula writes an active Formula whose default harness is codex and
// whose optional per-role bindings come from the caller. Doctor never needs approval.
func saveRoleFormula(t *testing.T, lab, defaultCommand, harnesses string) {
	t.Helper()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := "kind: formula\nid: f\nintake: {repo: acme/widgets}\nharness:\n  binding: codex\n  command: " + defaultCommand + "\n" + harnesses + roleAtoms
	if err := os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
}

func rolesByName(t *testing.T, report map[string]any) map[string]map[string]any {
	t.Helper()
	list, ok := report["roles"].([]any)
	if !ok {
		t.Fatalf("doctor JSON has no roles array: %v", report)
	}
	byName := map[string]map[string]any{}
	for _, v := range list {
		role := v.(map[string]any)
		for _, key := range []string{"role", "binding", "source", "ready", "readiness", "capability", "detail"} {
			if _, ok := role[key]; !ok {
				t.Fatalf("role entry lacks %s: %v", key, role)
			}
		}
		byName[role["role"].(string)] = role
	}
	return byName
}

func TestDoctorRoleReadinessReportsEachRole(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	claude := writeTool(t, bin, "claude", strings.NewReplacer("LOGGED", "false").Replace(claudeTool))
	saveRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", codex), "harnesses:\n  analysis: {binding: claude-code, command: "+argv(t, launcher, "__adapter", "claude-code", claude)+"}\n")
	code, out := runDoctor(t, lab)
	roles := rolesByName(t, decodeDoctor(t, out))
	if code != 2 {
		t.Fatalf("a role that is not ready must fail doctor: %d %s", code, out)
	}
	analysis, coding := roles["analysis"], roles["coding"]
	if analysis["binding"] != "claude-code" || analysis["source"] != "override" || analysis["ready"] != false || analysis["nextCommand"] == nil {
		t.Fatalf("analysis: %v", analysis)
	}
	if coding["binding"] != "codex" || coding["source"] != "default" || coding["ready"] != true || coding["nextCommand"] != nil {
		t.Fatalf("coding: %v", coding)
	}
	if _, present := roles["meta"]; present {
		t.Fatal("meta is not configured and must not be reported")
	}
	_, human := runDoctor(t, lab, "--verbose")
	for _, want := range []string{"Role readiness:", "analysis: claude-code (override binding) — not ready", "coding: codex (default binding) — ready"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human doctor output lacks %q:\n%s", want, human)
		}
	}
}

func TestDoctorRoleReadinessUnsupportedAnalysisBlocksReadiness(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	saveRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", codex), "harnesses:\n  analysis: {binding: custom, command: [/bin/custom]}\n")
	code, out := runDoctor(t, lab)
	analysis := rolesByName(t, decodeDoctor(t, out))["analysis"]
	capability := analysis["capability"].(map[string]any)
	if code != 2 || analysis["ready"] != false || capability["state"] != "unsupported" || !strings.Contains(capability["reason"].(string), "no verified read-only ticket-analysis protocol") {
		t.Fatalf("%d %v", code, analysis)
	}
}

func TestDoctorRoleReadinessSingleBindingKeepsResult(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	saveRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", codex), "")
	code, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	roles := rolesByName(t, report)
	if code != 0 || report["nextCommand"] != nil || len(roles) != 2 || roles["analysis"]["source"] != "default" || roles["coding"]["source"] != "default" || roles["analysis"]["binding"] != "codex" {
		t.Fatalf("%d %s", code, out)
	}
	for _, key := range []string{"activeFormulaId", "labDir", "savedBinding", "workflow", "candidates", "selection", "note"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("existing key %s missing", key)
		}
	}
}

// olderBuild lays out a Forgecell build beside its release manifest. Doctor reads
// both and executes neither; the protocol decides whether the build is compatible.
func olderBuild(t *testing.T, protocol string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"0.2.0","protocolVersion":"`+protocol+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return writeTool(t, dir, "forgecell", "#!/bin/sh\nexit 1\n")
}

func TestDoctorStaleMetaLauncherPointsAtRebindOfTheCodingBinding(t *testing.T) {
	for _, c := range []struct {
		name, source string
		override     bool
		launcher     func(t *testing.T) string
		ready        bool
		noCoding     bool
		wantRebind   bool
	}{
		{name: "learn atom on an incompatible build", source: "learn-atom", launcher: func(t *testing.T) string { return olderBuild(t, "7") }, wantRebind: true},
		{name: "learn atom on a compatible build", source: "learn-atom", launcher: func(t *testing.T) string { return olderBuild(t, "1") }, ready: true, wantRebind: true},
		{name: "meta override on an incompatible build", source: "override", override: true, launcher: func(t *testing.T) string { return olderBuild(t, "7") }, wantRebind: true},
		{name: "meta override on a compatible build", source: "override", override: true, launcher: func(t *testing.T) string { return olderBuild(t, "1") }, ready: true, wantRebind: true},
		{name: "learn atom on a tool that is not Forgecell", source: "learn-atom", launcher: func(t *testing.T) string { return writeTool(t, t.TempDir(), "custom-tool", "#!/bin/sh\nexit 1\n") }},
		{name: "coding harness that init cannot discover ready", source: "learn-atom", launcher: func(t *testing.T) string { return olderBuild(t, "7") }, noCoding: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, lab := doctorFixture(t)
			launcher, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			codex := filepath.Join(bin, "codex")
			if !c.noCoding {
				codex = writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
			}
			claude := writeTool(t, bin, "claude", strings.NewReplacer("LOGGED", "true").Replace(claudeTool))
			stale := argv(t, c.launcher(t), "__adapter", "claude-code", claude)
			harnesses, learn := "", ""
			if c.override {
				harnesses = "harnesses:\n  meta: {binding: claude-code, command: " + stale + "}\n"
			} else {
				learn = "  - {id: learn, type: learn, binding: claude-code, command: " + stale + ", timeoutMs: 900000}\n"
			}
			os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
			os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
			recipe := "kind: formula\nid: f\nintake: {repo: acme/widgets}\nharness:\n  binding: codex\n  command: " + argv(t, launcher, "__adapter", "codex", codex) + "\n" + harnesses + roleAtoms + learn
			if err = os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("forgecell init --lab %q --rebind codex", lab)
			_, out := runDoctor(t, lab)
			meta := rolesByName(t, decodeDoctor(t, out))["meta"]
			if meta["source"] != c.source || meta["binding"] != "claude-code" || meta["ready"] != c.ready {
				t.Fatalf("meta role = %v", meta)
			}
			if !c.wantRebind {
				if next, _ := meta["nextCommand"].(string); strings.Contains(next, "--rebind") {
					t.Fatalf("doctor offers a rebind that init would refuse or that would not move the launcher: %v", meta)
				}
				return
			}
			if meta["nextCommand"] != want {
				t.Fatalf("meta role = %v, want nextCommand %q", meta, want)
			}
			_, human := runDoctor(t, lab, "--verbose")
			if !strings.Contains(human, "Role readiness:") || !strings.Contains(human, "Next: "+want) {
				t.Fatalf("human doctor output lacks %q:\n%s", want, human)
			}
		})
	}
}

func TestRebindFlagConflicts(t *testing.T) {
	lab := t.TempDir()
	cases := map[string][]string{
		"harness":       {"--rebind", "codex", "--harness", "codex"},
		"meta harness":  {"--rebind", "codex", "--meta-harness", "codex"},
		"meta command":  {"--rebind", "codex", "--meta-command", `["/bin/x"]`},
		"timeout":       {"--rebind", "codex", "--harness-timeout-ms", "1000"},
		"max timeout":   {"--rebind", "codex", "--max-timeout-ms", "1000"},
		"review":        {"--rebind", "codex", "--review", "assay-x"},
		"approve":       {"--rebind", "codex", "--approve", "assay-x"},
		"dismiss":       {"--rebind", "codex", "--dismiss", "assay-x"},
		"empty harness": {"--rebind", ""},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"init", "--lab", lab}, extra...)
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 1 || !strings.Contains(stderr.String(), "--rebind") {
				t.Fatalf("exit %d stderr %q", code, stderr.String())
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused --rebind wrote to the Lab: %v", entries)
			}
		})
	}
}

func TestRebindFlagIsDocumentedInHelp(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || !strings.Contains(out.String(), "--rebind ID") {
		t.Fatalf("help omits --rebind: %s", out.String())
	}
}
