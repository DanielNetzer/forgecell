package readiness

import (
	"bytes"
	"crypto/sha256"
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
	if err := validateCheckInputs(v.CheckInputs); err != nil {
		return v, err
	}
	return v, nil
}

func validateCheckInputs(inputs []FileEvidence) error {
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, input := range inputs {
		if !identifier.MatchString(input.ID) || ids[input.ID] || paths[input.Path] || len(input.Content) > 64000 || !hex256.MatchString(input.SHA256) || fmt.Sprintf("%x", sha256.Sum256([]byte(input.Content))) != input.SHA256 {
			return fmt.Errorf("invalid, ambiguous or tampered reviewed check input: %s", input.ID)
		}
		if err := ValidatePath(input.Path); err != nil {
			return err
		}
		ids[input.ID], paths[input.Path] = true, true
	}
	return nil
}
