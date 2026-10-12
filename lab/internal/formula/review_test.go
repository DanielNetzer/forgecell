package formula

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const reviewAtom = "  - {id: review, type: review}\n"

func TestReviewAtomRoleParsesAndKeepsUnknownRolesRefused(t *testing.T) {
	good := "harnesses:\n  review: {binding: claude-code, command: [/bin/r, __adapter, claude-code, /bin/claude], timeoutMs: 700}\n"
	f, err := Parse([]byte(rolesFormula(good, rolesAtoms+reviewAtom)))
	if err != nil {
		t.Fatal(err)
	}
	if b, declared := f.RoleBinding(ReviewHarness); !declared || b.Binding != "claude-code" || b.TimeoutMS != 700 {
		t.Fatalf("%+v %v", b, declared)
	}
	for name, harnesses := range map[string]string{
		"similar unknown role":    "harnesses:\n  reviewer: {binding: x, command: [/bin/x]}\n",
		"extensions on review":    "extensions:\n  graphify: {manifest: m.json, sha256: " + strings.Repeat("a", 64) + "}\nharnesses:\n  review: {binding: x, command: [/bin/x], extensions: [graphify]}\n",
		"analysis wrapper review": "harnesses:\n  review: {binding: x, command: [/bin/x], analysisWrapper: {manifest: m.json, sha256: " + strings.Repeat("a", 64) + "}}\n",
		"policy in review role":   "harnesses:\n  review: {binding: x, command: [/bin/x], policy: auto}\n",
		"missing command":         "harnesses:\n  review: {binding: x}\n",
		"missing binding":         "harnesses:\n  review: {command: [/bin/x]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(rolesFormula(harnesses, rolesAtoms+reviewAtom))); err == nil {
				t.Fatal("invalid review role binding accepted")
			}
		})
	}
}

func TestReviewAtomRoleNeverFallsBackToTheCodingBinding(t *testing.T) {
	f, err := Parse([]byte(rolesFormula("", rolesAtoms+reviewAtom)))
	if err != nil {
		t.Fatal(err)
	}
	if b, declared := f.RoleBinding(ReviewHarness); declared || b.Binding != "" || len(b.Command) != 0 {
		t.Fatalf("review resolved to %+v (declared %v) without harnesses.review", b, declared)
	}
	coding, _ := f.RoleBinding(CodingHarness)
	if coding.Binding != "default" {
		t.Fatalf("coding role changed: %+v", coding)
	}
	inherit, err := Parse([]byte(rolesFormula("harnesses:\n  review: {binding: r, command: [/bin/r]}\n", rolesAtoms+reviewAtom)))
	if err != nil {
		t.Fatal(err)
	}
	if b, declared := inherit.RoleBinding(ReviewHarness); !declared || b.TimeoutMS != 1000 {
		t.Fatalf("a declared review role must inherit the default timeout: %+v", b)
	}
}

func TestReviewAtomValidation(t *testing.T) {
	role := "harnesses:\n  review: {binding: rev, command: [/bin/rev]}\n"
	cases := []struct {
		name, harnesses, atoms, want string
	}{
		{"atom only", "", rolesAtoms + reviewAtom, ""},
		{"atom and role", role, rolesAtoms + "  - {id: review, type: review, binding: rev}\n", ""},
		{"atom without binding and role", role, rolesAtoms + reviewAtom, ""},
		{"no atom and no role", "", rolesAtoms, ""},
		{"role without atom", role, rolesAtoms, "no review Atom"},
		{"atom with command", "", rolesAtoms + "  - {id: review, type: review, command: [/bin/x]}\n", "cannot declare a command"},
		{"atom binding conflicts with role", role, rolesAtoms + "  - {id: review, type: review, binding: other}\n", "conflicts"},
		{"two atoms", "", rolesAtoms + reviewAtom + "  - {id: review-2, type: review}\n", "only one review Atom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Parse([]byte(rolesFormula(c.harnesses, c.atoms)))
			if err != nil {
				t.Fatal(err)
			}
			err = f.ValidateReviewAtom()
			if c.want == "" && err != nil {
				t.Fatal(err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if c.want != "" {
				if err = f.ValidateHarnessRoles(); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("ValidateHarnessRoles error %v, want %q", err, c.want)
				}
			}
		})
	}
}

func TestReviewAtomAbsenceLeavesFormulaIdentityUnchanged(t *testing.T) {
	data := []byte(rolesFormula("", rolesAtoms))
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || f.Harnesses != nil {
		t.Fatalf("%+v", f)
	}
	if _, present, err := f.ReviewAtom(); present || err != nil {
		t.Fatalf("present=%v err=%v", present, err)
	}
	if err = f.ValidateHarnessRoles(); err != nil {
		t.Fatal(err)
	}
	for _, role := range HarnessRoles {
		if role == ReviewHarness {
			t.Fatal("the review role must not join HarnessRoles: it would change role evidence of every Formula")
		}
	}
}

func TestReviewAtomExecutionRolesPlacement(t *testing.T) {
	withReview := strings.Replace(rolesAtoms, "  - {id: gate,", reviewAtom+"  - {id: gate,", 1)
	learn := "  - {id: learn, type: learn}\n"
	for _, c := range []struct {
		name, atoms string
		wantReview  bool
		wantErr     bool
		wantOrder   string
	}{
		{"without review", rolesAtoms, false, false, ""},
		{"review after check", withReview, true, false, "intake,scope-approval,coding,verification,review,review-approval,publication,documentation"},
		{"review and trailing learn", withReview + learn, true, false, "intake,scope-approval,coding,verification,review,review-approval,publication,documentation,learning"},
		{"review at the end", rolesAtoms + reviewAtom, false, true, ""},
		{"review before check", strings.Replace(rolesAtoms, "  - {id: check,", reviewAtom+"  - {id: check,", 1), false, true, ""},
		{"review with a purpose", strings.Replace(withReview, "{id: review, type: review}", "{id: review, type: review, purpose: x}", 1), false, true, ""},
		{"two reviews", strings.Replace(withReview, "  - {id: gate,", "  - {id: review-2, type: review}\n  - {id: gate,", 1), false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := Parse([]byte(rolesFormula("", c.atoms)))
			if err != nil {
				t.Fatal(err)
			}
			roles, err := f.ExecutionRoles()
			if c.wantErr {
				if err == nil {
					t.Fatal("misplaced review Atom accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := roles[Review]; ok != c.wantReview {
				t.Fatalf("review role present=%v want %v", ok, c.wantReview)
			}
			if c.wantOrder != "" {
				var got []string
				for _, a := range f.Atoms {
					for role, declared := range roles {
						if declared.ID == a.ID {
							got = append(got, string(role))
						}
					}
				}
				if strings.Join(got, ",") != c.wantOrder {
					t.Fatalf("order %v", got)
				}
			}
		})
	}
}
