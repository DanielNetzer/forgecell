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

// QueueProjection is optional historical evidence; all extension fields remain intact.
type QueueProjection struct {
	Issue struct {
		URL    string `json:"url"`
		Repo   string `json:"repo"`
		Number int64  `json:"number"`
	} `json:"issue"`
	Workspace *struct {
		Repo string `json:"repo"`
	} `json:"workspace"`
	StartedAt string `json:"startedAt"`
	Readiness *struct {
		Phase string `json:"phase"`
	} `json:"readiness"`
}

func (l Ledger) QueueProjection() (QueueProjection, error) {
	var p QueueProjection
	if l.fields == nil {
		return p, fmt.Errorf("ledger was not decoded")
	}
	// Decode only queue fields: indentation of arbitrary historical extensions
	// could expand them far beyond the bounded file size.
	for _, field := range []struct {
		name   string
		target any
	}{{"issue", &p.Issue}, {"workspace", &p.Workspace}, {"startedAt", &p.StartedAt}, {"readiness", &p.Readiness}} {
		if raw, ok := l.fields[field.name]; ok {
			if err := json.Unmarshal(raw, field.target); err != nil {
				return p, fmt.Errorf("%s: %w", field.name, err)
			}
		}
	}
	return p, nil
}
