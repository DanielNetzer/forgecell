package readiness

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	deltaFixturePath = "lab/internal/harness/testdata/contracts.json"
	deltaPackage     = "github.com/owner/repo/lab/internal/harness"
	deltaTest        = "TestAdapterCompatibility/claude-code/molecule"
)

func protectedDeltaPlan() Plan {
	p := fixture()
	p.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "./..."}
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: deltaFixturePath, Reason: "Pinned contract fixture", Evidence: []string{"manifest"}})
	p.ExpectedProtectedDeltas = []ProtectedDelta{{CheckID: "regression", Package: deltaPackage, Test: deltaTest, ScopePath: deltaFixturePath, Reason: "The approved change alters the pinned argv", Evidence: []string{"manifest"}}}
	return p
}

func TestProtectedDeltaValidation(t *testing.T) {
	if err := protectedDeltaPlan().Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"unknown check", func(p *Plan) { p.ExpectedProtectedDeltas[0].CheckID = "missing" }},
		{"candidate check", func(p *Plan) {
			p.Analysis.Checks = append(p.Analysis.Checks, Check{ID: "cand", Category: "candidate", Argv: []string{"go", "test", "-json", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{"manifest"}, Reason: "Candidate run", Evidence: []string{"manifest"}})
			p.ExpectedProtectedDeltas[0].CheckID = "cand"
		}},
		{"optional check", func(p *Plan) {
			p.Analysis.Checks = append(p.Analysis.Checks, Check{ID: "cand", Category: "candidate", Argv: []string{"go", "vet", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{"manifest"}, Reason: "Candidate run", Evidence: []string{"manifest"}})
			p.Analysis.Checks[0].Required = false
		}},
		{"no json argv", func(p *Plan) { p.Analysis.Checks[0].Argv = []string{"go", "test", "./..."} }},
		{"empty package", func(p *Plan) { p.ExpectedProtectedDeltas[0].Package = "" }},
		{"wildcard package", func(p *Plan) { p.ExpectedProtectedDeltas[0].Package = "github.com/owner/repo/..." }},
		{"relative package", func(p *Plan) { p.ExpectedProtectedDeltas[0].Package = "./lab" }},
		{"empty test", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "" }},
		{"wildcard test", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "TestAdapter*" }},
		{"question wildcard", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "TestAdapter?" }},
		{"empty subtest segment", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "TestAdapter//molecule" }},
		{"trailing slash", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "TestAdapter/" }},
		{"whitespace", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "TestAdapter molecule" }},
		{"not a test function", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test = "helper/sub" }},
		{"duplicate", func(p *Plan) {
			p.ExpectedProtectedDeltas = append(p.ExpectedProtectedDeltas, p.ExpectedProtectedDeltas[0])
		}},
		{"too many", func(p *Plan) {
			for i := 0; i < MaxProtectedDeltas; i++ {
				d := p.ExpectedProtectedDeltas[0]
				d.Test = fmt.Sprintf("TestMany%d", i)
				p.ExpectedProtectedDeltas = append(p.ExpectedProtectedDeltas, d)
			}
		}},
		{"scope path not approved", func(p *Plan) { p.ExpectedProtectedDeltas[0].ScopePath = "lab/other_test.go" }},
		{"scope path not a test source", func(p *Plan) { p.ExpectedProtectedDeltas[0].ScopePath = "lab/parser.go" }},
		{"empty reason", func(p *Plan) { p.ExpectedProtectedDeltas[0].Reason = " " }},
		{"missing evidence", func(p *Plan) { p.ExpectedProtectedDeltas[0].Evidence = nil }},
		{"unknown evidence", func(p *Plan) { p.ExpectedProtectedDeltas[0].Evidence = []string{"nope"} }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			p := protectedDeltaPlan()
			tt.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("invalid expected protected delta was accepted")
			}
			if _, err := p.Digest(); err == nil {
				t.Fatal("invalid plan produced a digest")
			}
		})
	}
}

func TestProtectedDeltaAcceptsDefinitionPathsShellJSONAndSubtests(t *testing.T) {
	p := protectedDeltaPlan()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/go.mod", Reason: "Check definition", Evidence: []string{"manifest"}})
	p.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "go test -json ./internal/..."}
	p.ExpectedProtectedDeltas[0].ScopePath = "lab/go.mod"
	p.ExpectedProtectedDeltas = append(p.ExpectedProtectedDeltas, ProtectedDelta{CheckID: "regression", Package: deltaPackage, Test: "TestExample", ScopePath: deltaFixturePath, Reason: "Pinned output", Evidence: []string{"issue"}})
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func protectedDeltaDigestPlan() Plan {
	p := protectedDeltaPlan()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/go.mod", Reason: "Check definition", Evidence: []string{"manifest"}})
	return p
}

func TestProtectedDeltaDigestCoversDeclaration(t *testing.T) {
	base := protectedDeltaDigestPlan()
	digest, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"removed", func(p *Plan) { p.ExpectedProtectedDeltas = nil }},
		{"added", func(p *Plan) {
			d := p.ExpectedProtectedDeltas[0]
			d.Test = "TestSecond"
			p.ExpectedProtectedDeltas = append(p.ExpectedProtectedDeltas, d)
		}},
		{"package", func(p *Plan) { p.ExpectedProtectedDeltas[0].Package += "/other" }},
		{"test", func(p *Plan) { p.ExpectedProtectedDeltas[0].Test += "/other" }},
		{"scope path", func(p *Plan) { p.ExpectedProtectedDeltas[0].ScopePath = "lab/go.mod" }},
		{"reason", func(p *Plan) { p.ExpectedProtectedDeltas[0].Reason += " also" }},
		{"evidence", func(p *Plan) { p.ExpectedProtectedDeltas[0].Evidence = []string{"issue"} }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			p := protectedDeltaDigestPlan()
			tt.mutate(&p)
			got, err := p.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if got == digest {
				t.Fatal("changed declaration retained the digest")
			}
		})
	}
	plain := fixture()
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "expectedProtectedDeltas") {
		t.Fatalf("plans without deltas must keep their earlier encoding: %s", raw)
	}
	raw, err = json.Marshal(base)
	if err != nil || !strings.Contains(string(raw), `"expectedProtectedDeltas"`) {
		t.Fatalf("declaration missing from the digested encoding: %v %s", err, raw)
	}
}

func TestProtectedDeltaApprovalNeedsTheExactAmendedDigest(t *testing.T) {
	first := fixture()
	first.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "./..."}
	first.Analysis.Scope = append(first.Analysis.Scope, ScopedPath{Path: deltaFixturePath, Reason: "Pinned contract fixture", Evidence: []string{"manifest"}})
	state, err := NewState(first, "t0")
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := state.Plans[0].Digest
	next := protectedDeltaPlan()
	next.Revision = 2
	next.ParentDigest = oldDigest
	next.PausedTree = strings.Repeat("a", 40)
	state, err = state.Amend(next, "t1")
	if err != nil {
		t.Fatal(err)
	}
	proposal := state.Plans[len(state.Plans)-1]
	if proposal.Digest == oldDigest || len(proposal.Plan.ExpectedProtectedDeltas) != 1 {
		t.Fatalf("amendment lost or did not bind the declaration: %+v", proposal)
	}
	if _, err = state.Approve(oldDigest, next.Inputs, "t2"); err == nil {
		t.Fatal("approval of the previous digest accepted new deltas")
	}
	tampered := cloneState(state)
	tampered.Plans[1].Plan.ExpectedProtectedDeltas[0].Test = "TestSomethingElse"
	if _, err = tampered.Approve(proposal.Digest, next.Inputs, "t2"); err == nil {
		t.Fatal("approval accepted a declaration that no longer matches its digest")
	}
	approved, err := state.Approve(proposal.Digest, next.Inputs, "t2")
	if err != nil || approved.Phase != "approved" {
		t.Fatalf("exact approval failed: %v %+v", err, approved)
	}
}

func TestProtectedDeltaAdvisoryIsLabelledAndNeverDeclares(t *testing.T) {
	p := fixture()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: deltaFixturePath, Reason: "Pinned contract fixture", Evidence: []string{"manifest"}})
	advisory := p.ProtectedDeltaAdvisory()
	for _, want := range []string{"proposed for review; never auto-approved", deltaFixturePath, "expectedProtectedDeltas"} {
		if !strings.Contains(advisory, want) {
			t.Fatalf("advisory missing %q: %s", want, advisory)
		}
	}
	if len(p.ExpectedProtectedDeltas) != 0 {
		t.Fatal("advisory declared a delta")
	}
	if got := fixture().ProtectedDeltaAdvisory(); got != "" {
		t.Fatalf("advisory without a fixture path: %s", got)
	}
	if got := protectedDeltaPlan().ProtectedDeltaAdvisory(); got != "" {
		t.Fatalf("advisory shown although deltas are declared: %s", got)
	}
	p.Analysis.Checks[0].Category = "candidate"
	if got := p.ProtectedDeltaAdvisory(); got != "" {
		t.Fatalf("advisory without a protected regression check: %s", got)
	}
}

func TestProtectedDeltaFixturePathsAndLookup(t *testing.T) {
	for file, want := range map[string]bool{deltaFixturePath: true, "Fixtures/a.json": true, "lab/parser.go": false, "lab/testdata.go": false} {
		if IsFixturePath(file) != want {
			t.Fatalf("IsFixturePath(%q) != %t", file, want)
		}
	}
	p := protectedDeltaPlan()
	if got := p.FixtureScopePaths(); len(got) != 1 || got[0] != deltaFixturePath {
		t.Fatalf("fixture paths: %v", got)
	}
	if len(p.DeltasForCheck("regression")) != 1 || len(p.DeltasForCheck("other")) != 0 {
		t.Fatal("lookup by check identity is wrong")
	}
}
