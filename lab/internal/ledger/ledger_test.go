package ledger

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestV0LedgerRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/v0.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "mol-12-fixture" || got.FormulaID != "formula-fixture" || got.Status != "waiting" {
		t.Fatalf("unexpected ledger: %+v", got)
	}
	encoded, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	if err = json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration lost ledger fields")
	}
}
func TestRejectInvalidLedger(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{}`, `{"schemaVersion":"v99","kind":"molecule"}`, `{"schemaVersion":"v0","kind":"formula"}`, `{"schemaVersion":"v0","kind":"molecule","id":"../escape"}`} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestAtomProvenanceRoundTripPreservesUnknownAndExtensions(t *testing.T) {
	for _, atom := range []string{
		`{"id":"old","type":"ship","status":"done"}`,
		`{"id":"custom","provenance":{"declaredSource":"declared","declaredBinding":"recipe","declaredWorkflows":["workflow"],"actions":[{"attemptId":"original","resolvedSource":"bound-harness:custom","plannedAction":"invoke-coding-harness","observed":{"action":"invoke-coding-harness","result":"process-succeeded","evidence":[{"path":"result.json","sha256":"hash"}],"futureObservation":{"preserve":true}}}],"futureProvenance":"preserve"}}`,
	} {
		raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-provenance","formulaId":"f","status":"waiting","atoms":[` + atom + `],"futureRecord":{"keep":true}}`)
		l, err := Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := l.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var before, after any
		if err = json.Unmarshal(raw, &before); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(encoded, &after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("lost provenance, extension, or historical absence")
		}
	}
}

func TestCIReferenceExtensionsRoundTrip(t *testing.T) {
	raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-ci","formulaId":"f","status":"waiting","ciReferences":[{"sequence":1,"sha256":"original","status":"failed","commit":"original-head"},{"sequence":2,"sha256":"later","previous":"original","status":"stale"}]}`)
	l, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := l.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	json.Unmarshal(raw, &before)
	json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("CI references changed during ledger round trip")
	}
}

func TestIssueQueueHistoricalProjection(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":"v0","kind":"molecule","id":"old","formulaId":"f","status":"future","extra":{"retain":true}}`,
		`{"schemaVersion":"v0","kind":"molecule","id":"old","formulaId":"f","status":"waiting","issue":{"repo":"o/r","number":1},"startedAt":"bad","readiness":{"phase":"new-phase"}}`,
	} {
		l, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		p, err := l.QueueProjection()
		if err != nil {
			t.Fatal(err)
		}
		if l.Status == "future" && (p.Issue.Number != 0 || p.Readiness != nil) {
			t.Fatal(p)
		}
		if l.Status == "waiting" && (p.StartedAt != "bad" || p.Readiness.Phase != "new-phase") {
			t.Fatal(p)
		}
		encoded, err := l.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var before, after any
		json.Unmarshal([]byte(raw), &before)
		json.Unmarshal(encoded, &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("projection mutated evidence")
		}
	}
	l, err := Decode([]byte(`{"schemaVersion":"v0","kind":"molecule","id":"old","formulaId":"f","status":"waiting","issue":{"number":"bad"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.QueueProjection(); err == nil {
		t.Fatal("malformed projection")
	}
}
