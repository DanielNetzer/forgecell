package delivery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// recordingPath puts a git and a gh on PATH that record any call and fail, and
// returns the log they write.
func recordingPath(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	for _, name := range []string{"git", "gh"} {
		body := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> '" + log + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return log
}

func assertNoCalls(t *testing.T, log string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(raw))) != 0 {
		t.Fatalf("a process started:\n%s", raw)
	}
}

func labWithConfig(t *testing.T, config string) string {
	t.Helper()
	lab := t.TempDir()
	if err := os.WriteFile(filepath.Join(lab, "lab.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return lab
}

func TestDeliveryRefusesADemoLabBeforeAnyProcess(t *testing.T) {
	for name, config := range map[string]string{
		"demo Formula":     `{"schemaVersion":"v0","activeFormulaId":"forgecell-demo","currentApproval":{"formulaId":"forgecell-demo","sourceKind":"demo"}}`,
		"demo source kind": `{"schemaVersion":"v0","activeFormulaId":"renamed","currentApproval":{"formulaId":"renamed","sourceKind":"demo"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			log := recordingPath(t)
			lab := labWithConfig(t, config)
			o := Options{LabDir: lab, MoleculeID: "mol-1-abc", BaseBranch: "main", Files: []string{"greeting.go"}}
			if _, err := Preview(context.Background(), o); !errors.Is(err, ErrDemoLab) {
				t.Fatalf("Preview did not refuse the demo Lab: %v", err)
			}
			ran := 0
			runner := func(context.Context, process.Options) process.Result { ran++; return process.Result{Code: 1} }
			_, err := Publish(context.Background(), PublishOptions{Options: o, Approve: strings.Repeat("a", 64), Title: "Add Greeting", Runner: runner})
			if !errors.Is(err, ErrDemoLab) || !strings.Contains(err.Error(), "this is a forgecell demo Lab; it never contacts GitHub") {
				t.Fatalf("Publish did not refuse the demo Lab: %v", err)
			}
			if ran != 0 {
				t.Fatalf("Publish ran %d remote operation(s)", ran)
			}
			assertNoCalls(t, log)
			if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !os.IsNotExist(err) {
				t.Fatalf("Publish took the delivery lock in a demo Lab: %v", err)
			}
		})
	}
}

func TestDemoLabCheckFailsClosedAndPreviewDemoNeedsADemoLab(t *testing.T) {
	log := recordingPath(t)
	if demo, err := IsDemoLab(t.TempDir()); demo || err != nil {
		t.Fatalf("a Lab without lab.json is a demo Lab: %t %v", demo, err)
	}
	userLab := labWithConfig(t, `{"schemaVersion":"v0","activeFormulaId":"forgecell-github-issue","currentApproval":{"formulaId":"forgecell-github-issue","sourceKind":"init"}}`)
	if demo, err := IsDemoLab(userLab); demo || err != nil {
		t.Fatalf("a real Lab is a demo Lab: %t %v", demo, err)
	}
	if _, err := PreviewDemo(context.Background(), Options{LabDir: userLab, MoleculeID: "mol-1-abc", BaseBranch: "main", Files: []string{"a.go"}}); err == nil || !strings.Contains(err.Error(), "only in a forgecell demo Lab") {
		t.Fatalf("PreviewDemo ran in a real Lab: %v", err)
	}
	// Not JSON: formula.Load refuses it, so the command keeps its own error.
	if demo, err := IsDemoLab(labWithConfig(t, `{"activeFormulaId":`)); demo || err != nil {
		t.Fatalf("a malformed lab.json: %t %v", demo, err)
	}
	oversized := labWithConfig(t, `{"activeFormulaId":"forgecell-demo","pad":"`+strings.Repeat("x", maxLabConfigBytes)+`"}`)
	if _, err := IsDemoLab(oversized); err == nil {
		t.Fatal("an oversized lab.json was not refused")
	}
	if _, err := Preview(context.Background(), Options{LabDir: oversized, MoleculeID: "mol-1-abc", BaseBranch: "main", Files: []string{"a.go"}}); err == nil || errors.Is(err, ErrDemoLab) || !strings.Contains(err.Error(), "could not be checked for a demo Lab") {
		t.Fatalf("Preview did not fail closed on an oversized lab.json: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "lab")
	if err := os.MkdirAll(filepath.Join(dir, "lab.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := IsDemoLab(dir); err == nil {
		t.Fatal("a lab.json directory was not refused")
	}
	assertNoCalls(t, log)
}
