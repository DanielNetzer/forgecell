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
