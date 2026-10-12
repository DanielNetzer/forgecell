package readiness

import (
	"strings"
	"testing"
)

// -failfast stops go test after the first failure, so later tests emit no events and
// a declared failing test would stand in for them.
func TestHardening129FailfastIsRejectedForDeltaDeclaringJSONChecks(t *testing.T) {
	for name, argv := range map[string][]string{
		"-failfast":           {"go", "test", "-json", "-failfast", "./..."},
		"--failfast":          {"go", "test", "-json", "--failfast", "./..."},
		"-test.failfast":      {"go", "test", "-json", "-test.failfast", "./..."},
		"-failfast=true":      {"go", "test", "-json", "-failfast=true", "./..."},
		"-test.failfast=true": {"go", "test", "-json", "-test.failfast=true", "./..."},
		"-failfast=1":         {"go", "test", "-json", "-failfast=1", "./..."},
		"before -json":        {"go", "test", "-failfast", "-json", "./..."},
		"after the packages":  {"go", "test", "-json", "./...", "-args", "-test.failfast"},
	} {
		t.Run(name, func(t *testing.T) {
			p := protectedDeltaPlan()
			p.Analysis.Checks[0].Argv = argv
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), "failfast") || !strings.Contains(err.Error(), "regression") {
				t.Fatalf("a delta-declaring check using failfast was accepted or the error does not name the check: %v", err)
			}
			if _, err = p.Digest(); err == nil {
				t.Fatal("an invalid plan produced a digest")
			}
		})
	}
}

// A shell, env or script wrapper can hide flags from the plan validator, so a
// delta-declaring -json check must be a direct go test argv when a plan is amended
// or approved. Validate itself keeps accepting an earlier wrapper plan without
// failfast, because the verification runner validates stored plans with it.
func TestHardening129DeltaDeclaringJSONChecksMustBeDirectGoTest(t *testing.T) {
	for name, argv := range map[string][]string{
		"sh -c":                   {"/bin/sh", "-c", "go test -json ./internal/..."},
		"sh -c with failfast":     {"/bin/sh", "-c", "go test -json -failfast ./internal/..."},
		"quoted failfast":         {"/bin/sh", "-c", "go test -json '-failfast' ./..."},
		"env assignment":          {"env", "GOFLAGS=-failfast", "go", "test", "-json", "./..."},
		"env without a flag":      {"env", "FOO=1", "go", "test", "-json", "./..."},
		"script wrapper":          {"sh", "check.sh", "-json"},
		"go -C is not direct":     {"go", "-C", "lab", "test", "-json", "./..."},
		"go vet":                  {"go", "vet", "-json", "./..."},
		"an absolute go":          {"/usr/local/go/bin/go", "test", "-json", "./..."},
		"tool that names go test": {"gotestsum", "--", "go", "test", "-json", "./..."},
	} {
		t.Run(name, func(t *testing.T) {
			p := protectedDeltaPlan()
			p.Analysis.Checks[0].Argv = argv
			if err := p.ValidateDirectDeltaChecks(); err == nil || !strings.Contains(err.Error(), "direct `go test`") || !strings.Contains(err.Error(), "regression") {
				t.Fatalf("a wrapped delta-declaring check was accepted or the error does not name the check: %v", err)
			}
			if err := p.ValidateForCoding(); err == nil {
				t.Fatal("approval must refuse a wrapped delta-declaring check")
			}
			if !requestsGoTestFailfast(argv) {
				if err := p.Validate(); err != nil {
					t.Fatalf("Validate must keep accepting an earlier wrapper plan: %v", err)
				}
				if err := p.ValidateForCoding(); err == nil || !strings.Contains(err.Error(), "direct `go test`") {
					t.Fatalf("approval must name the direct go test rule: %v", err)
				}
			}
		})
	}
	for name, mutate := range map[string]func(*Plan){
		"a wrapper without declared deltas": func(p *Plan) {
			p.ExpectedProtectedDeltas = nil
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "go test -json ./..."}
		},
		"a direct go test": func(p *Plan) {},
		"a wrapper that does not request json": func(p *Plan) {
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "go test ./..."}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := protectedDeltaPlan()
			mutate(&p)
			if err := p.ValidateDirectDeltaChecks(); err != nil {
				t.Fatalf("unaffected plan rejected: %v", err)
			}
		})
	}
}

func TestHardening129FailfastIsAllowedWhereItMasksNothing(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"explicitly false": func(p *Plan) { p.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "-failfast=false", "./..."} },
		"test.failfast=0": func(p *Plan) {
			p.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "-test.failfast=0", "./..."}
		},
		"no failfast":             func(p *Plan) {},
		"a package that names it": func(p *Plan) { p.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "./internal/failfast/..."} },
		"no declared deltas": func(p *Plan) {
			p.ExpectedProtectedDeltas = nil
			p.Analysis.Checks[0].Argv = []string{"go", "test", "-json", "-failfast", "./..."}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := protectedDeltaPlan()
			mutate(&p)
			if err := p.Validate(); err != nil {
				t.Fatalf("unaffected plan rejected: %v", err)
			}
		})
	}
}

func TestHardening129RequestsGoTestFailfastTokens(t *testing.T) {
	for argv, want := range map[string]bool{
		"-failfast": true, "--test.failfast": true, "-failfast=TRUE": true, "-failfast=false": false, "-failfast=F": false,
		"-run=Failfast": false, "failfast": false, "-failfastx": false, "GOFLAGS=-test.failfast=true": true,
	} {
		if got := requestsGoTestFailfast([]string{argv}); got != want {
			t.Fatalf("requestsGoTestFailfast(%q) = %t, want %t", argv, got, want)
		}
	}
}
