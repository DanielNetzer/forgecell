package readiness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// VerificationInputs supplies explicit human-authored evidence and proposed
// commands to intake. Loading it does not approve or execute any command.
type VerificationInputs struct {
	CheckInputs []FileEvidence   `json:"checkInputs"`
	Checks      []Check          `json:"checks"`
	Setup       []Check          `json:"setup"`
	Artifacts   []ArtifactRoot   `json:"artifacts"`
	Policy      *ExecutionPolicy `json:"policy,omitempty"`
}

func DecodeVerificationInputs(raw []byte) (VerificationInputs, error) {
	var v VerificationInputs
	if len(raw) > 1000000 {
		return v, fmt.Errorf("verification inputs exceed 1 MB")
	}
	if err := uniqueJSON(raw); err != nil {
		return v, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return v, fmt.Errorf("expected one verification input object")
	}
	if len(v.CheckInputs) == 0 || len(v.CheckInputs) > 32 || len(v.Checks) > 64 || len(v.Setup) > 32 {
		return v, fmt.Errorf("verification inputs require bounded frozen evidence")
	}
	return v, nil
}
