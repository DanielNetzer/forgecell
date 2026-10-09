package formula

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recipe = `schemaVersion: v0
kind: formula
id: sample
intake:
  source: github-issues
  repo: acme/widgets
harness:
  binding: codex
  command: [forgecell, __adapter, codex, /bin/codex]
  timeoutMs: 1000
atoms:
  - id: intake
    type: intake
  - id: work
    type: harness
  - id: gate
    type: gate
`

func TestParsePreservesSnapshot(t *testing.T) {
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "sample" || f.Intake.Repo != "acme/widgets" || len(f.Atoms) != 3 || len(f.Harness.Command) != 4 {
		t.Fatalf("%+v", f)
	}
	if f.YAML != recipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(recipe))) {
		t.Fatal("snapshot changed")
	}
}
func TestInvalidRecipes(t *testing.T) {
	for _, s := range []string{"[]", strings.Replace(recipe, "kind: formula", "kind: molecule", 1), strings.Replace(recipe, "v0", "v9", 1), strings.Replace(recipe, "id: work", "id: intake", 1), strings.Replace(recipe, "[forgecell, __adapter, codex, /bin/codex]", "echo bad", 1), strings.Replace(recipe, "timeoutMs: 1000", "timeoutMs: -1", 1), recipe + "\n---\nkind: formula\n", recipe + "id: duplicate\n"} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestApprovedLoading(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir, "")
	if err != nil || got.Approved {
		t.Fatalf("%+v %v", got, err)
	}
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	os.WriteFile(filepath.Join(dir, "formulas", "sample.yaml"), []byte(recipe), 0600)
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"sample","formulaFile":"ignored"}`), 0600)
	approveTest(t, dir, recipe, "first")
	got, err = Load(dir, "")
	if err != nil || !got.Approved || got.Formula.ID != "sample" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = Load(dir, "../escape"); err == nil {
		t.Fatal("unsafe id accepted")
	}
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte("bad json"), 0600)
	if _, err = Load(dir, ""); err == nil {
		t.Fatal("corrupt approval silently became draft")
	}
}

func TestExistingFormulaWithExtraContextKeepsExactApprovedBytes(t *testing.T) {
	const existing = `# An approved v0 Formula can contain fields outside the runtime subset.
schemaVersion: v0
kind: formula
id: existing
name: Existing issue workflow
intake:
  source: github-issues
  repo: acme/widgets
harness:
  binding: codex
  command: [forgecell, __adapter, codex, /bin/codex]
  instructions: >-
    Inspect the issue and preserve the human review gate.
  timeoutMs: 900000
lab:
  dir: /Users/example/.forgecell/labs/widgets
  observe: false
repositoryContext:
  defaultBranch: main
atoms:
  - id: intake
    type: intake
  - id: harness
    type: harness
  - id: gate
    type: gate
`
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "formulas", "existing.yaml"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"existing"}`), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Inspect(dir, "")
	if err != nil || loaded.Approved {
		t.Fatalf("existing Formula unreadable: %+v %v", loaded, err)
	}
	if loaded.Formula.YAML != existing || loaded.Formula.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(existing))) {
		t.Fatal("approved YAML bytes or hash changed during parsing")
	}
	if loaded.Formula.Harness.Binding != "codex" || loaded.Formula.Harness.Instructions != "Inspect the issue and preserve the human review gate." {
		t.Fatalf("existing binding or instructions changed: %+v", loaded.Formula.Harness)
	}
}

func TestExistingFilenameNormalization(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	text := strings.Replace(recipe, "id: sample", "id: Custom Formula.v1", 1)
	os.WriteFile(filepath.Join(dir, "formulas", "custom-formula.v1.yaml"), []byte(text), 0600)
	got, err := Inspect(dir, "Custom Formula.v1")
	if err != nil || got.Formula.ID != "Custom Formula.v1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func approveTest(t *testing.T, dir, yaml, id string) Approval {
	t.Helper()
	f, e := Parse([]byte(yaml))
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		t.Fatal(e)
	}
	target, e := Inspect(dir, f.ID)
	before := "absent"
	if e == nil {
		before = target.Formula.SHA256
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	a := Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: id, SourceKind: "test-review", SourceID: id}
	if e = BeginActivation(dir, yaml, a, cfg, before); e != nil {
		t.Fatal(e)
	}
	if e = ApplyActivation(dir, a); e != nil {
		t.Fatal(e)
	}
	if e = CompleteActivation(dir, a); e != nil {
		t.Fatal(e)
	}
	return a
}
func TestCurrentExactApproval(t *testing.T) {
	for _, kind := range []string{"comment", "missing", "injected", "historical-id", "restored-bytes", "stale-record", "incomplete", "same-bytes-new-decision"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			a := approveTest(t, dir, recipe, "first")
			initial, e := Load(dir, "")
			if e != nil {
				t.Fatal(e)
			}
			selected := "sample"
			switch kind {
			case "comment":
				os.WriteFile(initial.File, []byte(recipe+"# edited\n"), 0600)
			case "missing":
				os.Remove(initial.File)
			case "injected":
				selected = "injected"
				os.WriteFile(filepath.Join(dir, "formulas/injected.yaml"), []byte(strings.Replace(recipe, "id: sample", "id: injected", 1)), 0600)
			case "historical-id":
				approveTest(t, dir, strings.Replace(recipe, "id: sample", "id: next", 1), "next")
			case "restored-bytes":
				approveTest(t, dir, recipe+"# new\n", "next")
				os.WriteFile(initial.File, []byte(recipe), 0600)
			case "stale-record":
				os.Remove(filepath.Join(dir, "formula-decisions/first.json"))
			case "incomplete":
				v, e := readActivation(dir, a)
				if e != nil {
					t.Fatal(e)
				}
				v.Complete = false
				if e = saveActivation(dir, v); e != nil {
					t.Fatal(e)
				}
			case "same-bytes-new-decision":
				approveTest(t, dir, recipe, "next")
				if e = Revalidate(dir, initial); e == nil {
					t.Fatal("same-byte identity drift accepted")
				}
				return
			}
			if _, e = Load(dir, selected); e == nil {
				t.Fatal("non-current execution admitted")
			}
			if kind != "missing" {
				if _, e = Inspect(dir, selected); e != nil {
					t.Fatal("inspection failed", e)
				}
			}
		})
	}
}
