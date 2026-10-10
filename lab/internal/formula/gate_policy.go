package formula

import (
	"fmt"
	"strings"
)

const (
	ModeHuman  = "human"
	ModePolicy = "policy"

	PolicyAutoWhenReady         = "auto-when-ready"
	PolicyDeliverWhenChecksPass = "deliver-when-checks-pass"
)

// gatePolicies is the reviewed allowlist of automatable gates: the exact gate
// purpose mapped to the only policy it may declare. Anything not listed here,
// including every Formula, binding, release or deploy decision and any action
// class added later, is never automated.
var gatePolicies = map[string]string{
	"scope":  PolicyAutoWhenReady,
	"review": PolicyDeliverWhenChecksPass,
}

// DeclaresPolicy returns the policy id only for an exact `mode: policy`.
// Absent, `human` and unrecognised modes are all human decisions.
func (a Atom) DeclaresPolicy() (string, bool) {
	if a.Mode == ModePolicy && a.Policy != "" {
		return a.Policy, true
	}
	return "", false
}

// validateGatePolicy rejects only the new `policy` key and `mode: policy`.
// Other `mode` values stay valid so Formulas approved earlier keep loading.
func validateGatePolicy(m map[string]any) error {
	mode, _ := m["mode"].(string)
	value, hasPolicy := m["policy"]
	if !hasPolicy {
		if mode == ModePolicy {
			return fmt.Errorf("mode: policy requires a policy id")
		}
		return nil
	}
	id, ok := value.(string)
	if !ok {
		return fmt.Errorf("policy must be a string policy id")
	}
	typ, _ := m["type"].(string)
	if strings.TrimSpace(typ) != "gate" {
		return fmt.Errorf("policy %q is only valid on a gate Atom, not a %s Atom", id, strings.TrimSpace(typ))
	}
	purpose, _ := m["purpose"].(string)
	want, ok := gatePolicies[purpose]
	if !ok {
		return fmt.Errorf("a gate with purpose %q cannot declare a policy; only scope and review gates can, and Formula, binding, release and deploy decisions always require a human", purpose)
	}
	if id != want {
		for _, known := range gatePolicies {
			if id == known {
				return fmt.Errorf("policy %q is not valid on a %s gate; use %q", id, purpose, want)
			}
		}
		return fmt.Errorf("unknown policy %q for a %s gate; use %q", id, purpose, want)
	}
	if mode != ModePolicy {
		return fmt.Errorf("policy %q requires mode: policy on the gate", id)
	}
	return nil
}
