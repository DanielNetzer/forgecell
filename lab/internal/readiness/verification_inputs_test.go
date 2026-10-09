package readiness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestVerificationInputsRejectAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"checkInputs":[],"checkInputs":[]}`, `{"unknown":true}`, `{} {}`} {
		if _, err := DecodeVerificationInputs([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestReviewedInputRejectsAmbiguousPathsAndIDs(t *testing.T) {
	for _, kind := range []string{"duplicate", "alias", "scope", "hash", "reference"} {
		t.Run(kind, func(t *testing.T) {
			p := fixture()
			input := FileEvidence{Evidence: Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("true")))}, Content: "true"}
			p.CheckInputs = []FileEvidence{input}
			p.Evidence = append(p.Evidence, input.Evidence)
			switch kind {
			case "duplicate":
				p.CheckInputs = append(p.CheckInputs, input)
			case "alias":
				other := input
				other.ID = "alias"
				p.CheckInputs = append(p.CheckInputs, other)
				p.Evidence = append(p.Evidence, other.Evidence)
			case "scope":
				p.Analysis.Scope[0].Path = input.Path
			case "hash":
				p.CheckInputs[0].Content = "false"
			case "reference":
				p.CheckInputs[0].Path = "different.sh"
			}
			if err := p.Validate(); err == nil {
				t.Fatal("ambiguous or tampered human input accepted")
			}
		})
	}
}

func TestVerificationInputsFreezeBytesBeforePlanReview(t *testing.T) {
	input := FileEvidence{Evidence: Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("true")))}, Content: "true"}
	for _, kind := range []string{"valid", "bytes", "hash", "path", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			v := VerificationInputs{CheckInputs: []FileEvidence{input}}
			switch kind {
			case "bytes":
				v.CheckInputs[0].Content = "false"
			case "hash":
				v.CheckInputs[0].SHA256 = "bad"
			case "path":
				v.CheckInputs[0].Path = "../outside"
			case "duplicate":
				v.CheckInputs = append(v.CheckInputs, input)
			}
			raw, _ := json.Marshal(v)
			_, err := DecodeVerificationInputs(raw)
			if (err == nil) != (kind == "valid") {
				t.Fatal("wrong frozen input result", err)
			}
		})
	}
}
