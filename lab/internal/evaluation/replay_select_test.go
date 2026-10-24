package evaluation

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestReplaySelectOnlyOnGoTest(t *testing.T) {
	checks := replayChecks()
	for _, c := range checks {
		want := ""
		if c.Name == "go-test" {
			want = readiness.SelectAffectedPackages
		}
		if c.Select != want {
			t.Fatalf("check %s selects %q, want %q", c.Name, c.Select, want)
		}
		if err := validateCommandSelect(c, false); err != nil {
			t.Fatalf("check %s: %v", c.Name, err)
		}
	}
	plan := Plan{ReadinessVersion: 1, TargetBranch: "main", SchemaVersion: 1, Task: "issue.json", Acceptance: "acceptance.sh", BaseCommit: strings.Repeat("a", 40), AllowedFiles: []string{"lab/x.go"}, Checks: checks}
	if err := validatePlan(plan); err != nil {
		t.Fatal(err)
	}
}

func TestReplaySelectMapsIntoTheReadinessPlan(t *testing.T) {
	plan := Plan{ReadinessVersion: 1, TargetBranch: "main", SchemaVersion: 1, BaseCommit: strings.Repeat("a", 40), AllowedFiles: []string{"lab/x.go"}, Checks: replayChecks()}
	var o molecule.Options
	configureReadiness(&o, plan, []byte("{}"), []byte("#!/bin/sh\n"))
	a, err := o.Analyze(context.Background(), formula.Binding{}, molecule.Issue{}, readiness.RepositorySnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, c := range a.Checks {
		switch c.ID {
		case "go-test":
			seen++
			if c.Select != readiness.SelectAffectedPackages || c.Category != "regression" {
				t.Fatalf("%+v", c)
			}
			if err := readiness.ValidateCheckSelect(c); err != nil {
				t.Fatal(err)
			}
		case "acceptance":
			seen++
			// Hidden acceptance keeps its own -run filter and never selects.
			if c.Select != "" || c.Category != "independent-acceptance" {
				t.Fatalf("%+v", c)
			}
		default:
			if c.Select != "" {
				t.Fatalf("%s selects", c.ID)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("checks %+v", a.Checks)
	}
	// Even a hand-edited plan that marks acceptance as selecting maps it to none.
	edited := plan
	edited.Checks = replayChecks()
	edited.Checks[3].Select = readiness.SelectAffectedPackages
	var e molecule.Options
	configureReadiness(&e, edited, []byte("{}"), []byte("#!/bin/sh\n"))
	if a, err = e.Analyze(context.Background(), formula.Binding{}, molecule.Issue{}, readiness.RepositorySnapshot{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Checks {
		if c.ID == "acceptance" && c.Select != "" {
			t.Fatal("acceptance selected")
		}
	}
}

func TestReplaySelectRefusesInvalidCommands(t *testing.T) {
	goTest := Command{Name: "go-test", Dir: "lab", Argv: []string{"go", "test", "./..."}, TimeoutMS: 1000, Select: readiness.SelectAffectedPackages}
	cases := []struct {
		name  string
		c     Command
		setup bool
	}{
		{"acceptance", Command{Name: "acceptance", Dir: ".", Argv: []string{"go", "test", "./..."}, TimeoutMS: 1000, Select: readiness.SelectAffectedPackages}, false},
		{"setup", goTest, true},
		{"unknown value", Command{Name: "go-test", Argv: []string{"go", "test", "./..."}, Select: "all"}, false},
		{"go vet", Command{Name: "go-vet", Argv: []string{"go", "vet", "./..."}, Select: readiness.SelectAffectedPackages}, false},
		{"shell", Command{Name: "sh", Argv: []string{"sh", "-c", "go test ./..."}, Select: readiness.SelectAffectedPackages}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCommandSelect(tc.c, tc.setup); err == nil {
				t.Fatalf("accepted %+v", tc.c)
			}
		})
	}
	if err := validateCommandSelect(goTest, false); err != nil {
		t.Fatal(err)
	}
}
