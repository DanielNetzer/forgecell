package formula

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestTimeoutClampDefaultBinding(t *testing.T) {
	cases := []struct {
		name     string
		defaults string
		want     int
	}{
		{"max below the process default clamps", "  maxTimeoutMs: 120000\n", 120000},
		{"max at the process default is unchanged", "  maxTimeoutMs: 600000\n", 0},
		{"max above the process default is unchanged", "  maxTimeoutMs: 900000\n", 0},
		{"explicit timeout wins", "  timeoutMs: 1000\n  maxTimeoutMs: 120000\n", 1000},
		{"no maximum leaves it unset", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(inheritFormula(c.defaults, "  analysis: {binding: analyst, command: [/bin/a]}\n"))
			f, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if b, override := f.RoleBinding(MetaHarness); override || b.EffectiveTimeoutMS() != c.want {
				t.Fatalf("default binding timeout = %+v override=%v, want %d", b, override, c.want)
			}
			if b, _ := f.ExecutionBinding(Coding); b.EffectiveTimeoutMS() != c.want {
				t.Fatalf("coding timeout = %d, want %d", b.EffectiveTimeoutMS(), c.want)
			}
			if f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || f.YAML != string(data) {
				t.Fatal("the Formula identity changed")
			}
		})
	}
}

func TestTimeoutClampRoleBinding(t *testing.T) {
	f, err := Parse([]byte(inheritFormula("", "  coding: {binding: coder, command: [/bin/coder], maxTimeoutMs: 90000}\n  analysis: {binding: analyst, command: [/bin/a], maxTimeoutMs: 700000}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.RoleBinding(CodingHarness); b.EffectiveTimeoutMS() != 90000 {
		t.Fatalf("coding role timeout = %d, want 90000", b.EffectiveTimeoutMS())
	}
	if b, _ := f.RoleBinding(AnalysisHarness); b.EffectiveTimeoutMS() != 0 {
		t.Fatalf("a maximum above the process default must not set a timeout: %d", b.EffectiveTimeoutMS())
	}
	if declared := f.Harnesses["coding"]; declared.TimeoutMS != 0 {
		t.Fatalf("resolving changed the parsed declaration: %+v", declared)
	}
}

func TestTimeoutClampKeepsTheBindingAsDeclared(t *testing.T) {
	f, err := Parse([]byte(inheritFormula("  maxTimeoutMs: 120000\n", "  analysis: {binding: analyst, command: [/bin/a]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.RoleBinding(CodingHarness)
	if b.TimeoutMS != 0 || b.MaxTimeoutMS != 120000 {
		t.Fatalf("the resolved binding must stay as declared so earlier binding hashes still verify: %+v", b)
	}
	if got := b.EffectiveTimeoutMS(); got != 120000 {
		t.Fatalf("effective timeout = %d, want 120000", got)
	}
}
