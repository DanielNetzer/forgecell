package formula

import "testing"

func TestExecutionRoles(t *testing.T) {
	f := Formula{Atoms: []Atom{{ID: "ticket", Type: "intake"}, {ID: "approve", Type: "gate", Purpose: "scope"}, {ID: "code", Type: "harness"}, {ID: "verify", Type: "check"}, {ID: "review", Type: "gate", Purpose: "review"}, {ID: "publish", Type: "ship"}, {ID: "docs", Type: "document"}}}
	roles, err := f.ExecutionRoles()
	if err != nil || roles[Coding].ID != "code" {
		t.Fatalf("roles=%v error=%v", roles, err)
	}
	for _, kind := range []string{"duplicate", "missing", "ordering", "purpose", "blank", "padded", "type", "precheck", "learning-purpose"} {
		t.Run(kind, func(t *testing.T) {
			bad := f
			bad.Atoms = append([]Atom(nil), f.Atoms...)
			switch kind {
			case "duplicate":
				bad.Atoms[2].ID = bad.Atoms[0].ID
			case "missing":
				bad.Atoms[2].ID = ""
			case "ordering":
				bad.Atoms[1], bad.Atoms[2] = bad.Atoms[2], bad.Atoms[1]
			case "purpose":
				bad.Atoms[4].Purpose = "scope"
			case "blank":
				bad.Atoms[2].ID = " "
			case "padded":
				bad.Atoms[2].ID = " code "
			case "type":
				bad.Atoms[2].Type = "check"
			case "precheck":
				bad.Atoms = append(bad.Atoms[:2], append([]Atom{{ID: "precheck", Type: "check"}}, bad.Atoms[2:]...)...)
			case "learning-purpose":
				bad.Atoms = append(bad.Atoms, Atom{ID: "learn", Type: "learn", Purpose: "scope"})
			}
			if _, err := bad.ExecutionRoles(); err == nil {
				t.Fatal("invalid identity/order accepted")
			}
		})
	}
}

func TestGeneratedRolesAndHistoricalInspection(t *testing.T) {
	const yaml = `kind: formula
id: synthetic
atoms:
 - type: intake
 - type: gate
   purpose: scope
 - type: harness
 - type: check
 - type: gate
   purpose: review
 - type: ship
 - type: document
 - type: learn
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	roles, err := f.ExecutionRoles()
	if err != nil {
		t.Fatal(err)
	}
	if roles[Coding].ID != "harness-3" || roles[Learning].ID != "learn-8" {
		t.Fatal("generated identities changed")
	}
	legacy, err := Parse([]byte("kind: formula\nid: old\natoms:\n - type: harness\n"))
	if err != nil {
		t.Fatal("historical parsing rejected", err)
	}
	if _, err = legacy.ExecutionRoles(); err == nil {
		t.Fatal("historical recipe authorized execution")
	}
	if legacy.YAML != "kind: formula\nid: old\natoms:\n - type: harness\n" {
		t.Fatal("historical bytes changed")
	}
}
