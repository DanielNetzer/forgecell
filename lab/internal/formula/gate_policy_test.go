package formula

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const baseGate = "  - id: gate\n    type: gate\n"

func withGate(gate string) string { return strings.Replace(recipe, baseGate, gate, 1) }

func scopeGate(extra string) string {
	return "  - id: gate\n    type: gate\n    purpose: scope\n" + extra
}

func TestGatePolicyAcceptsDeclaredPolicies(t *testing.T) {
	for _, tc := range []struct{ name, purpose, policy string }{
		{"scope", "scope", "auto-when-ready"},
		{"review", "review", "deliver-when-checks-pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := withGate("  - id: gate\n    type: gate\n    purpose: " + tc.purpose + "\n    mode: policy\n    policy: " + tc.policy + "\n")
			f, err := Parse([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			gate := f.Atoms[2]
			if gate.Mode != "policy" || gate.Policy != tc.policy || gate.Purpose != tc.purpose {
				t.Fatalf("%+v", gate)
			}
			if id, ok := gate.DeclaresPolicy(); !ok || id != tc.policy {
				t.Fatalf("policy not declared: %q %v", id, ok)
			}
			if f.YAML != text || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
				t.Fatal("snapshot changed")
			}
		})
	}
}

func TestGatePolicyRejectsMisplacedPolicies(t *testing.T) {
	policyOn := func(typ, purpose, policy string) string {
		atom := "  - id: gate\n    type: " + typ + "\n"
		if purpose != "" {
			atom += "    purpose: " + purpose + "\n"
		}
		return withGate(atom + "    mode: policy\n    policy: " + policy + "\n")
	}
	const intakeAtom = "  - id: intake\n    type: intake\n"
	const workAtom = "  - id: work\n    type: harness\n"
	const policyLines = "    mode: policy\n    policy: auto-when-ready\n"
	for _, tc := range []struct{ name, text string }{
		{"intake", strings.Replace(recipe, intakeAtom, intakeAtom+policyLines, 1)},
		{"harness", strings.Replace(recipe, workAtom, workAtom+policyLines, 1)},
		{"check", policyOn("check", "scope", "auto-when-ready")},
		{"ship", policyOn("ship", "review", "deliver-when-checks-pass")},
		{"document", policyOn("document", "", "auto-when-ready")},
		{"learn", policyOn("learn", "", "auto-when-ready")},
		{"observe", policyOn("observe", "", "auto-when-ready")},
		{"formula gate", policyOn("gate", "formula", "auto-when-ready")},
		{"binding gate", policyOn("gate", "binding", "auto-when-ready")},
		{"release gate", policyOn("gate", "release", "auto-when-ready")},
		{"deploy gate", policyOn("gate", "deploy", "deliver-when-checks-pass")},
		{"empty purpose", policyOn("gate", "", "auto-when-ready")},
		{"unknown purpose", policyOn("gate", "merge", "auto-when-ready")},
		{"scope with review id", policyOn("gate", "scope", "deliver-when-checks-pass")},
		{"review with scope id", policyOn("gate", "review", "auto-when-ready")},
		{"unknown id", policyOn("gate", "scope", "always-approve")},
		{"non-string policy", withGate(scopeGate("    mode: policy\n    policy: [auto-when-ready]\n"))},
		{"numeric policy", withGate(scopeGate("    mode: policy\n    policy: 7\n"))},
		{"mode without policy", withGate(scopeGate("    mode: policy\n"))},
		{"human with policy", withGate(scopeGate("    mode: human\n    policy: auto-when-ready\n"))},
		{"policy without mode", withGate(scopeGate("    policy: auto-when-ready\n"))},
		{"policy with unrecognised mode", withGate(scopeGate("    mode: auto\n    policy: auto-when-ready\n"))},
		{"formula root", strings.Replace(recipe, "intake:\n", "policy: auto-when-ready\nintake:\n", 1)},
		{"harness mapping", strings.Replace(recipe, "  timeoutMs: 1000\n", "  timeoutMs: 1000\n  policy: auto-when-ready\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.text)); err == nil {
				t.Fatalf("accepted:\n%s", tc.text)
			}
		})
	}
}

func TestGatePolicyKeepsExistingFormulaBytes(t *testing.T) {
	for _, tc := range []struct{ name, gate string }{
		{"no mode", scopeGate("")},
		{"human", scopeGate("    mode: human\n")},
		{"unrecognised mode", "  - id: gate\n    type: gate\n    purpose: review\n    mode: manual\n"},
		{"mapping mode", "  - id: gate\n    type: gate\n    purpose: review\n    mode: {a: b}\n"},
		{"non-gate mode", "  - id: gate\n    type: check\n    mode: human\n"},
		{"purpose formula", "  - id: gate\n    type: gate\n    purpose: formula\n    mode: human\n"},
		{"purpose release", "  - id: gate\n    type: gate\n    purpose: release\n"},
		{"purpose deploy", "  - id: gate\n    type: gate\n    purpose: deploy\n    mode: human\n"},
		{"nested policy key", "  - id: gate\n    type: gate\n    mode: human\n    extra: {policy: auto-when-ready}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := withGate(tc.gate)
			f, err := Parse([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := f.Atoms[2].DeclaresPolicy(); ok {
				t.Fatalf("declared a policy: %+v", f.Atoms[2])
			}
			if f.YAML != text || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
				t.Fatal("bytes or hash changed")
			}
		})
	}
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.Atoms[2].Mode != "" || f.Atoms[2].Policy != "" {
		t.Fatalf("recipe gained a mode or policy: %+v", f.Atoms[2])
	}
}

func TestGatePolicyKeepsUnusualModeFormulaBytes(t *testing.T) {
	const intakeAtom = "  - id: intake\n    type: intake\n"
	const workAtom = "  - id: work\n    type: harness\n"
	modes := []struct{ name, line string }{
		{"integer", "    mode: 7\n"},
		{"float", "    mode: 1.5\n"},
		{"true", "    mode: true\n"},
		{"false", "    mode: false\n"},
		{"null", "    mode: null\n"},
		{"tilde", "    mode: ~\n"},
		{"blank scalar", "    mode:\n"},
		{"empty string", "    mode: \"\"\n"},
		{"flow sequence", "    mode: [policy]\n"},
		{"block sequence", "    mode:\n      - policy\n"},
		{"capitalised", "    mode: Policy\n"},
		{"upper case", "    mode: POLICY\n"},
		{"padded", "    mode: \"policy \"\n"},
		{"mixed with human", "    mode: [human, policy]\n"},
	}
	placements := []struct {
		name  string
		place func(line string) string
	}{
		{"scope gate", func(line string) string { return withGate(scopeGate(line)) }},
		{"review gate", func(line string) string {
			return withGate("  - id: gate\n    type: gate\n    purpose: review\n" + line)
		}},
		{"gate without purpose", func(line string) string { return withGate(baseGate + line) }},
		{"harness atom", func(line string) string { return strings.Replace(recipe, workAtom, workAtom+line, 1) }},
		{"intake atom", func(line string) string { return strings.Replace(recipe, intakeAtom, intakeAtom+line, 1) }},
	}
	for _, place := range placements {
		for _, mode := range modes {
			t.Run(place.name+" "+mode.name, func(t *testing.T) {
				text := place.place(mode.line)
				if text == recipe {
					t.Fatal("the placement did not change the Formula")
				}
				f, err := Parse([]byte(text))
				if err != nil {
					t.Fatal(err)
				}
				if f.YAML != text || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
					t.Fatal("bytes or hash changed")
				}
				for _, atom := range f.Atoms {
					if id, ok := atom.DeclaresPolicy(); ok {
						t.Fatalf("atom %s declared policy %q with mode %q", atom.ID, id, atom.Mode)
					}
				}
			})
		}
	}
}
