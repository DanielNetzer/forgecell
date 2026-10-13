package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// saveMetaFormula writes an active Formula whose default harness and meta role are
// codex, with the learn Atom the caller supplies.
func saveMetaFormula(t *testing.T, lab, command, learn string) {
	t.Helper()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := "kind: formula\nid: f\nintake: {repo: acme/widgets}\nharness:\n  binding: codex\n  command: " + command + "\nharnesses:\n  meta: {binding: codex, command: " + command + "}\n" + roleAtoms + learn
	if err := os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveSavedFormula(t, lab)
}

func TestDoctorReportsMetaRoleConflictAsNotReady(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	command := argv(t, launcher, "__adapter", "codex", codex)

	saveMetaFormula(t, lab, command, "  - {id: learn, type: learn}\n")
	code, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	if meta := rolesByName(t, report)["meta"]; code != 0 || meta["ready"] != true || report["learnBound"] != true {
		t.Fatalf("the control Formula must be healthy: %d %v", code, out)
	}

	conflicting := argv(t, launcher, "__adapter", "codex", filepath.Join(bin, "other-codex"))
	saveMetaFormula(t, lab, command, "  - {id: learn, type: learn, binding: codex, command: "+conflicting+"}\n")
	code, out = runDoctor(t, lab)
	report = decodeDoctor(t, out)
	meta := rolesByName(t, report)["meta"]
	if code != 2 || meta["ready"] != false || meta["readiness"] != "blocked" || meta["source"] != "override" || report["learnBound"] != false {
		t.Fatalf("a conflicting meta role was reported healthy (exit %d): %v learnBound=%v", code, meta, report["learnBound"])
	}
	for _, text := range []string{meta["detail"].(string), meta["capability"].(map[string]any)["reason"].(string)} {
		if !strings.Contains(text, "harnesses.meta") || !strings.Contains(text, "learn Atom learn") {
			t.Fatalf("the conflict does not name the role and the Atom: %q", text)
		}
	}
	if capability := report["metaBinding"].(map[string]any)["capability"].(map[string]any); capability["state"] != "blocked" {
		t.Fatalf("metaBinding: %v", capability)
	}
	_, human := runDoctor(t, lab, "--verbose")
	if !strings.Contains(human, "meta: codex (override binding) — not ready") {
		t.Fatalf("the human view does not show the meta role as not ready:\n%s", human)
	}
}

func TestDoctorBlocksRelativeLauncherAsUnknown(t *testing.T) {
	bin, lab := doctorFixture(t)
	provider := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	sentinel := filepath.Join(bin, "executed")
	writeTool(t, bin, "forgecell", "#!/bin/sh\ntouch '"+sentinel+"'\n")
	manifest := `{"kind":"` + harness.ReleaseKind + `","version":"0.2.0","sha256":"x","protocolVersion":"` + harness.AdapterProtocolVersion + `"}`
	if err := os.WriteFile(filepath.Join(bin, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(bin)
	for _, launcher := range []string{"forgecell", "./forgecell"} {
		saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
		saved := savedBindingOf(t, lab)
		states := bindingStates(saved)
		if saved["readiness"] != "blocked" || len(states) != 1 || states[0]["state"] != "launcher-unknown" || states[0]["path"] != launcher {
			t.Fatalf("a relative launcher %q must classify unknown: %v", launcher, saved)
		}
		if detail := states[0]["detail"].(string); !strings.Contains(detail, "absolute path") || !strings.Contains(detail, "not resolved, read or executed") {
			t.Fatalf("detail: %s", detail)
		}
		diagnostics := saved["binding"].(map[string]any)
		if diagnostics["launcherVersion"] != "unknown" || diagnostics["launcherProtocol"] != "unknown" {
			t.Fatalf("a manifest beside a relative launcher was read: %v", diagnostics)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("the relative launcher %q was executed", launcher)
		}
	}
}
