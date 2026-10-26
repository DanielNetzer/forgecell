package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// retireLab writes one waiting Molecule ledger into a fresh fixture Lab.
func retireLab(t *testing.T) (lab, id string) {
	t.Helper()
	lab, id = t.TempDir(), "mol-7-abc"
	r := molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: id, Status: "waiting", FormulaID: "f", Mode: "ticket", StartedAt: "2026-10-10T10:00:00Z", LabDir: lab, Issue: molecule.Issue{Repo: "o/r", Number: 7, Title: "t"}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return lab, id
}

func runCLI(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test")
	return code, out.String(), stderr.String()
}

func TestCLIRetireRetiresOneMolecule(t *testing.T) {
	lab, id := retireLab(t)
	code, out, stderr := runCLI("retire", id, "--reason", "superseded by a later attempt", "--lab", lab)
	if code != 0 || !strings.Contains(out, "Retired Molecule "+id+" (was waiting)") || !strings.Contains(out, "Reason: superseded by a later attempt") {
		t.Fatalf("code %d\n%s\n%s", code, out, stderr)
	}
	if strings.Contains(out+stderr, lab) {
		t.Fatalf("output names the Lab path:\n%s%s", out, stderr)
	}
	r, err := molecule.ReadRecord(lab, id)
	if err != nil || r.Status != molecule.StatusRetired || r.Retired == nil || r.Retired.Reason != "superseded by a later attempt" {
		t.Fatalf("ledger %v %q %+v", err, r.Status, r.Retired)
	}
	// Retiring again is a refusal with a non-zero exit, and changes nothing.
	before, _ := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	code, _, stderr = runCLI("retire", id, "--reason", "again", "--lab", lab)
	after, _ := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	if code == 0 || !strings.Contains(stderr, "already retired") || !bytes.Equal(before, after) || strings.Contains(stderr, lab) {
		t.Fatalf("second retire: %d %s", code, stderr)
	}
}

func TestCLIRetireUsageErrorsAndRefusals(t *testing.T) {
	lab, id := retireLab(t)
	before, _ := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	for name, args := range map[string][]string{
		"no id":           {"retire", "--reason", "stale", "--lab", lab},
		"missing reason":  {"retire", id, "--lab", lab},
		"empty reason":    {"retire", id, "--reason", "", "--lab", lab},
		"multi-line":      {"retire", id, "--reason", "one\ntwo", "--lab", lab},
		"oversized":       {"retire", id, "--reason", strings.Repeat("x", molecule.MaxRetireReason+1), "--lab", lab},
		"bulk":            {"retire", id, "mol-8-def", "--reason", "stale", "--lab", lab},
		"unknown flag":    {"retire", id, "--reason", "stale", "--all", "--lab", lab},
		"unsafe id":       {"retire", "../escape", "--reason", "stale", "--lab", lab},
		"unknown":         {"retire", "mol-404-none", "--reason", "stale", "--lab", lab},
		"flags before id": {"retire", "--lab", lab, id, "--reason", "stale"},
	} {
		code, out, stderr := runCLI(args...)
		if code == 0 || out != "" {
			t.Errorf("%s: code %d out %q", name, code, out)
		}
		if strings.Contains(stderr, lab) {
			t.Errorf("%s: stderr names the Lab path: %s", name, stderr)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(lab, "ledgers", id+".json")); !bytes.Equal(before, after) {
		t.Fatal("a refused retire changed the ledger")
	}
}

func TestCLIHelpListsRetire(t *testing.T) {
	code, out, _ := runCLI("--help")
	if code != 0 || !strings.Contains(out, "\nretire <molecule> --reason TEXT [--lab DIR]") {
		t.Fatalf("help lacks retire: %d", code)
	}
}
