package readiness

import "testing"

func TestVerificationInputsRejectAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"checkInputs":[],"checkInputs":[]}`, `{"unknown":true}`, `{} {}`} {
		if _, err := DecodeVerificationInputs([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
