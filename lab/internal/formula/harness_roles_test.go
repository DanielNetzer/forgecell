package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const rolesAtoms = "atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"

func rolesFormula(harnesses, atoms string) string {
	return "kind: formula\nid: roles\nharness:\n  binding: default\n  command: [/bin/default]\n  timeoutMs: 1000\n" + harnesses + atoms
}

func TestHarnessRolesSchemaValidation(t *testing.T) {
	good := "harnesses:\n  analysis: {binding: claude-code, command: [/bin/a, __adapter, claude-code, /bin/claude], timeoutMs: 500}\n  coding: {binding: codex, command: [/bin/c], instructions: guide}\n  meta: {binding: claude-code, command: [/bin/m]}\n"
	f, err := Parse([]byte(rolesFormula(good, rolesAtoms)))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Harnesses) != 3 || f.Harnesses["analysis"].Binding != "claude-code" || f.Harnesses["analysis"].TimeoutMS != 500 || f.Harnesses["coding"].Instructions != "guide" {
		t.Fatalf("%+v", f.Harnesses)
	}
	for name, harnesses := range map[string]string{
		"unknown role":    "harnesses:\n  reviewer: {binding: x, command: [/bin/x]}\n",
		"policy in role":  "harnesses:\n  analysis: {binding: x, command: [/bin/x], policy: auto}\n",
		"empty command":   "harnesses:\n  analysis: {binding: x, command: []}\n",
		"blank argument":  "harnesses:\n  analysis: {binding: x, command: [' ']}\n",
		"missing command": "harnesses:\n  analysis: {binding: x}\n",
		"missing binding": "harnesses:\n  analysis: {command: [/bin/x]}\n",
		"bare id":         "harnesses:\n  analysis: claude-code\n",
		"not a mapping":   "harnesses: [analysis]\n",
		"bad timeout":     "harnesses:\n  coding: {binding: x, command: [/bin/x], timeoutMs: 0}\n",
		"bad maximum":     "harnesses:\n  coding: {binding: x, command: [/bin/x], timeoutMs: 900, maxTimeoutMs: 100}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(rolesFormula(harnesses, rolesAtoms))); err == nil {
				t.Fatal("invalid role binding accepted")
			}
		})
	}
}

func TestHarnessRolesDefaultFallbackAndExecutionMapping(t *testing.T) {
	f, err := Parse([]byte(rolesFormula("harnesses:\n  analysis: {binding: analyst, command: [/bin/a]}\n", rolesAtoms)))
	if err != nil {
		t.Fatal(err)
	}
	if b, override := f.RoleBinding(AnalysisHarness); !override || b.Binding != "analyst" {
		t.Fatalf("analysis = %+v override=%v", b, override)
	}
	for _, role := range []HarnessRole{CodingHarness, MetaHarness} {
		if b, override := f.RoleBinding(role); override || b.Binding != "default" {
			t.Fatalf("%s = %+v override=%v", role, b, override)
		}
	}
	for role, want := range map[ExecutionRole]string{IntakeRole: "analyst", Coding: "default", Learning: "default"} {
		if b, ok := f.ExecutionBinding(role); !ok || b.Binding != want {
			t.Fatalf("%s = %+v ok=%v, want %s", role, b, ok, want)
		}
	}
	if _, ok := f.ExecutionBinding(Verification); ok {
		t.Fatal("a check is not harness-bound")
	}
}

func TestHarnessRolesAbsentLeavesFormulaIdentityUnchanged(t *testing.T) {
	data := []byte(rolesFormula("", rolesAtoms))
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Harnesses != nil || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatalf("%+v", f)
	}
	raw, err := json.Marshal(f.Harness)
	if err != nil {
		t.Fatal(err)
	}
	// The Binding is hashed as untagged JSON; any new field must be omitempty.
	if want := `{"Binding":"default","Command":["/bin/default"],"TimeoutMS":1000,"Instructions":""}`; string(raw) != want {
		t.Fatalf("binding JSON changed: %s", raw)
	}
	for _, role := range HarnessRoles {
		if b, override := f.RoleBinding(role); override || b.Binding != "default" {
			t.Fatalf("%s did not resolve to the default harness", role)
		}
	}
	if err = f.ValidateHarnessRoles(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessRolesAtomConflicts(t *testing.T) {
	learn := "  - {id: learn, type: learn, binding: meta-one, command: [/bin/meta-one], timeoutMs: 1000}\n"
	cases := []struct {
		name, harnesses, atoms, want string
	}{
		{"valid", "harnesses:\n  analysis: {binding: a, command: [/bin/a]}\n  coding: {binding: c, command: [/bin/c]}\n  meta: {binding: meta-one, command: [/bin/meta-one]}\n", strings.Replace(rolesAtoms, "{id: intake, type: intake}", "{id: intake, type: intake, binding: a}", 1) + learn, ""},
		{"analysis conflicts with intake Atom", "harnesses:\n  analysis: {binding: a, command: [/bin/a]}\n", strings.Replace(rolesAtoms, "{id: intake, type: intake}", "{id: intake, type: intake, binding: other}", 1), "conflicts"},
		{"coding conflicts with harness Atom", "harnesses:\n  coding: {binding: c, command: [/bin/c]}\n", strings.Replace(rolesAtoms, "{id: harness, type: harness}", "{id: harness, type: harness, binding: other}", 1), "conflicts"},
		{"meta without learn Atom", "harnesses:\n  meta: {binding: m, command: [/bin/m]}\n", rolesAtoms, "no Atom"},
		{"meta binding conflicts with learn Atom", "harnesses:\n  meta: {binding: m, command: [/bin/meta-one]}\n", rolesAtoms + learn, "conflicts"},
		{"meta command conflicts with learn Atom", "harnesses:\n  meta: {binding: meta-one, command: [/bin/different]}\n", rolesAtoms + learn, "command conflicts"},
		{"missing intake Atom", "harnesses:\n  analysis: {binding: a, command: [/bin/a]}\n", "atoms:\n  - {id: harness, type: harness}\n", "ticket readiness"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Parse([]byte(rolesFormula(c.harnesses, c.atoms)))
			if err != nil {
				t.Fatal(err)
			}
			err = f.ValidateHarnessRoles()
			if c.want == "" && err != nil {
				t.Fatal(err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
		})
	}
}

func inheritFormula(defaults, roles string) string {
	return "kind: formula\nid: roles\nharness:\n  binding: default\n  command: [/bin/default]\n" + defaults + "harnesses:\n" + roles + rolesAtoms
}

func TestCodingRoleInheritsOmittedInstructionsAndTimeouts(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n  maxTimeoutMs: 3000\n  instructions: approved guidance\n"
	cases := []struct {
		name         string
		coding       string
		instructions string
		timeoutMS    int
		maxTimeoutMS int
		ceilingMS    int
	}{
		{"omitted fields are inherited", "  coding: {binding: coder, command: [/bin/coder]}\n", "approved guidance", 1000, 3000, 3000},
		{"explicit fields win", "  coding: {binding: coder, command: [/bin/coder], instructions: own, timeoutMs: 400, maxTimeoutMs: 800}\n", "own", 400, 800, 800},
		{"an explicit smaller timeout is never widened by the default ceiling", "  coding: {binding: coder, command: [/bin/coder], timeoutMs: 400}\n", "approved guidance", 400, 0, 400},
		{"only instructions declared keeps the approved timeouts", "  coding: {binding: coder, command: [/bin/coder], instructions: own}\n", "own", 1000, 3000, 3000},
		{"a lone maximum bounds the inherited timeout", "  coding: {binding: coder, command: [/bin/coder], maxTimeoutMs: 800}\n", "approved guidance", 800, 800, 800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(inheritFormula(defaults, c.coding))
			f, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			declared := f.Harnesses["coding"]
			b, override := f.RoleBinding(CodingHarness)
			if !override || b.Binding != "coder" || len(b.Command) != 1 || b.Command[0] != "/bin/coder" {
				t.Fatalf("%+v override=%v", b, override)
			}
			if b.Instructions != c.instructions || b.TimeoutMS != c.timeoutMS || b.MaxTimeoutMS != c.maxTimeoutMS || b.TimeoutCeilingMS() != c.ceilingMS {
				t.Fatalf("resolved %+v, want instructions %q timeout %d maximum %d ceiling %d", b, c.instructions, c.timeoutMS, c.maxTimeoutMS, c.ceilingMS)
			}
			if eb, ok := f.ExecutionBinding(Coding); !ok || eb.Instructions != c.instructions || eb.TimeoutMS != c.timeoutMS {
				t.Fatalf("the execution binding does not follow the coding role: %+v", eb)
			}
			if after := f.Harnesses["coding"]; after.Instructions != declared.Instructions || after.TimeoutMS != declared.TimeoutMS || after.MaxTimeoutMS != declared.MaxTimeoutMS {
				t.Fatalf("resolving the role changed the parsed declaration: %+v", after)
			}
			if f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || f.YAML != string(data) {
				t.Fatal("the Formula identity changed")
			}
		})
	}
}

func TestOnlyTheCodingRoleInheritsFromTheDefaultHarness(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n  instructions: approved guidance\n"
	f, err := Parse([]byte(inheritFormula(defaults, "  analysis: {binding: analyst, command: [/bin/a]}\n  coding: {binding: coder, command: [/bin/coder]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.RoleBinding(AnalysisHarness); b.Instructions != "" || b.TimeoutMS != 0 {
		t.Fatalf("the analysis role inherited %+v", b)
	}
	if b, override := f.RoleBinding(MetaHarness); override || b.Instructions != "approved guidance" || b.TimeoutMS != 1000 {
		t.Fatalf("an undeclared role resolves to the default harness: %+v override=%v", b, override)
	}
	if b, _ := f.RoleBinding(CodingHarness); b.TimeoutCeilingMS() != 1000 {
		t.Fatalf("a default without a maximum grants none: %+v", b)
	}
}
