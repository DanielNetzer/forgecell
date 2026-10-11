package formula

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestAnalysisAndMetaOverridesInheritTheDefaultTimeout(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n  maxTimeoutMs: 3000\n  instructions: approved guidance\n"
	data := []byte(inheritFormula(defaults, "  analysis: {binding: analyst, command: [/bin/a]}\n  meta: {binding: meta-one, command: [/bin/m]}\n"))
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]Binding{}
	for name, b := range f.Harnesses {
		declared[name] = b
	}
	for _, role := range []HarnessRole{AnalysisHarness, MetaHarness} {
		b, override := f.RoleBinding(role)
		if !override || b.TimeoutMS != 1000 {
			t.Fatalf("%s did not inherit the default timeout: %+v override=%v", role, b, override)
		}
		if b.Instructions != "" || b.MaxTimeoutMS != 0 {
			t.Fatalf("%s inherited more than the timeout: %+v", role, b)
		}
	}
	for role, want := range map[ExecutionRole]int{IntakeRole: 1000, Learning: 1000, Coding: 1000} {
		if b, ok := f.ExecutionBinding(role); !ok || b.TimeoutMS != want {
			t.Fatalf("%s execution binding timeout = %+v ok=%v, want %d", role, b, ok, want)
		}
	}
	for name, before := range declared {
		if after := f.Harnesses[name]; after.TimeoutMS != before.TimeoutMS || after.TimeoutMS != 0 {
			t.Fatalf("resolving %s changed the parsed declaration: %+v", name, after)
		}
	}
	if f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || f.YAML != string(data) {
		t.Fatal("the Formula identity changed")
	}
}

func TestAnExplicitRoleTimeoutWinsOverTheDefault(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n"
	f, err := Parse([]byte(inheritFormula(defaults, "  analysis: {binding: analyst, command: [/bin/a], timeoutMs: 250}\n  meta: {binding: meta-one, command: [/bin/m], timeoutMs: 400}\n")))
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[HarnessRole]int{AnalysisHarness: 250, MetaHarness: 400} {
		if b, _ := f.RoleBinding(role); b.TimeoutMS != want {
			t.Fatalf("%s timeout = %d, want %d", role, b.TimeoutMS, want)
		}
	}
}

func TestACodingOverrideAndUndeclaredRolesResolveAsBefore(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n  maxTimeoutMs: 3000\n  instructions: approved guidance\n"
	f, err := Parse([]byte(inheritFormula(defaults, "  coding: {binding: coder, command: [/bin/coder]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.RoleBinding(CodingHarness); b.TimeoutMS != 1000 || b.MaxTimeoutMS != 3000 || b.Instructions != "approved guidance" {
		t.Fatalf("coding resolution changed: %+v", b)
	}
	for _, role := range []HarnessRole{AnalysisHarness, MetaHarness} {
		if b, override := f.RoleBinding(role); override || b.Binding != "default" || b.TimeoutMS != 1000 {
			t.Fatalf("undeclared %s no longer resolves to the default harness: %+v override=%v", role, b, override)
		}
	}
}

func TestAnOverrideInheritsNothingWhenTheDefaultDeclaresNoTimeout(t *testing.T) {
	f, err := Parse([]byte(inheritFormula("", "  analysis: {binding: analyst, command: [/bin/a]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.RoleBinding(AnalysisHarness); b.TimeoutMS != 0 {
		t.Fatalf("an unset default timeout must stay unset: %+v", b)
	}
}
