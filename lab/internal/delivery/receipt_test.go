package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestReceipt(t *testing.T, lab, id, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

// The list-mode reader keeps only state, lifecycle, URL and Molecule id, and a
// multi-megabyte diff is walked past, never returned.
func TestReceiptStatusIgnoresALargeDiff(t *testing.T) {
	lab := t.TempDir()
	diff := strings.Repeat("+line of a large reviewed diff\\n", 200_000)
	writeTestReceipt(t, lab, "m1", `{"title":"t","preview":{"files":["a","b"],"diff":"`+diff+`","moleculeId":"m1","nested":{"x":[1,{"y":null}]}},"lifecycle":{"state":"OPEN","observedAt":"x"},"state":"published","url":"https://github.com/o/r/pull/5","policy":[true,false]}`)
	s, found, err := ReadReceiptStatus(lab, "m1")
	if err != nil || !found {
		t.Fatalf("status: %v %t", err, found)
	}
	if s != (ReceiptStatus{State: "published", Lifecycle: "OPEN", URL: "https://github.com/o/r/pull/5", MoleculeID: "m1"}) {
		t.Fatalf("status %+v", s)
	}
}

func TestReceiptStatusAppliesTheReceiptRules(t *testing.T) {
	lab := t.TempDir()
	if _, found, err := ReadReceiptStatus(lab, "absent"); found || err != nil {
		t.Fatalf("absent deliveries: %t %v", found, err)
	}
	if _, _, err := ReadReceiptStatus(lab, "../x"); err == nil {
		t.Fatal("an unsafe id was read")
	}
	for name, raw := range map[string]string{
		"other molecule": `{"state":"published","preview":{"moleculeId":"m2"}}`,
		"unknown state":  `{"state":"merged","preview":{"moleculeId":"m1"}}`,
		"malformed":      `{"state":"published","preview":{"moleculeId":"m1"}`,
		"trailing data":  `{"state":"published","preview":{"moleculeId":"m1"}} {}`,
		"not an object":  `["published"]`,
		"bad lifecycle":  `{"state":"published","lifecycle":"OPEN","preview":{"moleculeId":"m1"}}`,
	} {
		writeTestReceipt(t, lab, "m1", raw)
		if _, found, err := ReadReceiptStatus(lab, "m1"); err == nil || !found {
			t.Errorf("%s: accepted (%t)", name, found)
		}
	}
	writeTestReceipt(t, lab, "m1", `{"state":"prepared","lifecycle":null,"preview":{"moleculeId":"m1"}}`)
	if s, _, err := ReadReceiptStatus(lab, "m1"); err != nil || s.State != "prepared" || s.Lifecycle != "" {
		t.Fatalf("null lifecycle: %+v %v", s, err)
	}
	if err := os.Remove(filepath.Join(lab, "deliveries", "m1.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(lab, "deliveries", "m1.json")); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ReadReceiptStatus(lab, "m1"); err == nil || !found {
		t.Fatalf("a symlinked receipt was read: %t %v", found, err)
	}
}
