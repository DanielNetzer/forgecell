// Package ledger reads versioned Molecule records without discarding extension fields.
package ledger

import (
	"encoding/json"
	"fmt"
	"regexp"
)

type Ledger struct {
	ID        string `json:"id"`
	FormulaID string `json:"formulaId"`
	Status    string `json:"status"`
	fields    map[string]json.RawMessage
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Decode(data []byte) (Ledger, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Ledger{}, fmt.Errorf("ledger must be a JSON object")
	}
	var header struct {
		SchemaVersion string `json:"schemaVersion"`
		Kind          string `json:"kind"`
		ID            string `json:"id"`
		FormulaID     string `json:"formulaId"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Ledger{}, err
	}
	if header.SchemaVersion != "v0" || header.Kind != "molecule" {
		return Ledger{}, fmt.Errorf("unsupported ledger schema or kind")
	}
	if !idPattern.MatchString(header.ID) || header.FormulaID == "" || header.Status == "" {
		return Ledger{}, fmt.Errorf("ledger identity, Formula and status are required")
	}
	return Ledger{ID: header.ID, FormulaID: header.FormulaID, Status: header.Status, fields: fields}, nil
}

// Encode preserves original JSON fields. Runtime mutation is deliberately separate.
func (l Ledger) Encode() ([]byte, error) {
	if l.fields == nil {
		return nil, fmt.Errorf("ledger was not decoded")
	}
	return json.MarshalIndent(l.fields, "", "  ")
}
