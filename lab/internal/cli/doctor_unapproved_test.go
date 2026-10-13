package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// approveSavedFormula gives the Formula already saved in lab a complete exact-byte approval.
func approveSavedFormula(t *testing.T, lab string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(lab, "formulas", "f.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := formula.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := formula.FileHash(filepath.Join(lab, "lab.json"))
	if err != nil {
		t.Fatal(err)
	}
	decision := "d" + f.SHA256[:16]
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: decision, SourceKind: "test-review", SourceID: decision}
	if err = formula.BeginActivation(lab, string(data), a, cfg, f.SHA256); err != nil {
		t.Fatal(err)
	}
	if err = formula.ApplyActivation(lab, a); err != nil {
		t.Fatal(err)
	}
	if err = formula.CompleteActivation(lab, a); err != nil {
		t.Fatal(err)
	}
}

func markerProvider(t *testing.T) (provider, marker string) {
	t.Helper()
	provider = writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	return provider, provider + ".calls"
}

func TestDoctorUnapprovedNeverExecutesTheProvider(t *testing.T) {
	bin, lab := doctorFixture(t)
	_ = bin
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider, marker := markerProvider(t)
	writeRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", provider), "")
	for _, extra := range [][]string{nil, {"--verbose"}} {
		code, out := runDoctor(t, lab, extra...)
		if code != 2 {
			t.Fatalf("an unapproved Formula must not be ready: %d %s", code, out)
		}
		if _, err = os.Stat(marker); err == nil {
			t.Fatalf("the unapproved provider was executed:\n%s", out)
		}
		if !strings.Contains(out, "not probed (unapproved)") {
			t.Fatalf("doctor did not say the binding was not probed: %s", out)
		}
	}
	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	if want := "forgecell init --lab " + shellWord(lab); report["nextCommand"] != want {
		t.Fatalf("nextCommand = %v, want %q", report["nextCommand"], want)
	}
	if saved, _ := report["savedBinding"].(map[string]any); saved == nil || saved["readiness"] != "blocked" {
		t.Fatalf("savedBinding = %v", report["savedBinding"])
	}
}

func TestDoctorUnapprovedProbesAnApprovedFormula(t *testing.T) {
	_, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider, marker := markerProvider(t)
	writeRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", provider), "")
	approveSavedFormula(t, lab)
	code, out := runDoctor(t, lab)
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("the approved provider was not probed (code %d): %s", code, out)
	}
	if strings.Contains(out, "not probed (unapproved)") {
		t.Fatalf("an approved Formula was reported unapproved: %s", out)
	}
}
