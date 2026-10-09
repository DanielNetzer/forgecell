// Package evaluation validates controlled comparisons of human-approved process changes.
package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

type Approval struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	ReviewedAt   string `json:"reviewedAt"`
	OriginalYAML string `json:"originalYaml"`
	ProposedYAML string `json:"proposedYaml"`
	OriginalHash string `json:"originalHash"`
	ProposedHash string `json:"proposedHash"`
}
type Pair struct {
	Approval            Approval
	Baseline, Candidate formula.Formula
}

func Hash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

// ValidateApproval never writes a Formula. This first evaluator supports one instruction-only intervention.
func ValidateApproval(raw []byte) (p Pair, err error) {
	if len(raw) > 1_000_000 {
		return p, fmt.Errorf("approval exceeds size limit")
	}
	if err = json.Unmarshal(raw, &p.Approval); err != nil {
		return p, err
	}
	a := p.Approval
	if a.ID == "" || a.Status != "approved" {
		return p, fmt.Errorf("an explicitly approved suggestion is required")
	}
	if _, err = time.Parse(time.RFC3339Nano, a.ReviewedAt); err != nil {
		return p, fmt.Errorf("saved human decision timestamp is required")
	}
	if Hash([]byte(a.OriginalYAML)) != a.OriginalHash || Hash([]byte(a.ProposedYAML)) != a.ProposedHash {
		return p, fmt.Errorf("approval YAML hashes do not match")
	}
	p.Baseline, err = formula.Parse([]byte(a.OriginalYAML))
	if err != nil {
		return p, err
	}
	p.Candidate, err = formula.Parse([]byte(a.ProposedYAML))
	if err != nil {
		return p, err
	}
	if p.Baseline.Harness.Instructions == p.Candidate.Harness.Instructions {
		return p, fmt.Errorf("evaluation requires a changed process instruction")
	}
	// Compare the entire YAML, including unknown fields, not just currently executable structs.
	var before, after map[string]any
	if err = yaml.Unmarshal([]byte(a.OriginalYAML), &before); err != nil {
		return p, err
	}
	if err = yaml.Unmarshal([]byte(a.ProposedYAML), &after); err != nil {
		return p, err
	}
	bh, bok := before["harness"].(map[string]any)
	ah, aok := after["harness"].(map[string]any)
	if !bok || !aok {
		return p, fmt.Errorf("both variants require a harness")
	}
	delete(bh, "instructions")
	delete(ah, "instructions")
	if !reflect.DeepEqual(before, after) {
		return p, fmt.Errorf("only harness.instructions may differ; bindings, permissions, timeouts, Atoms and gates must remain identical")
	}
	return p, nil
}

// ActivationDigest binds a new execution decision to exact frozen inputs and destinations.
// Historical suggestion approval is evidence, never execution authorization.
func ActivationDigest(plan []byte, pair Pair, source, output string) string {
	raw, _ := json.Marshal([]any{"evaluation-activation-v1", Hash(plan), pair.Approval.ID, pair.Baseline.ID, pair.Baseline.SHA256, pair.Candidate.ID, pair.Candidate.SHA256, source, output})
	return Hash(raw)
}
