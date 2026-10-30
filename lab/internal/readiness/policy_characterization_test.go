package readiness

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const (
	policyCharacterizationRulesEndpoint      = "repos/owner/repo/rules/branches/release%2Fv1?per_page=100"
	policyCharacterizationProtectionEndpoint = "repos/owner/repo/branches/release%2Fv1/protection"
	policyCharacterizationRulesCanonical     = `[{"parameters":{"required_approving_review_count":1},"ruleset_id":7,"type":"pull_request"}]`
	policyCharacterizationProtectCanonical   = `{"enforce_admins":{"enabled":true},"url":"https://api.github.com/repos/owner/repo/branches/release/v1/protection"}`
)

func policyCharacterizationFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "characterization", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// policyCharacterizationRead records every call and answers by endpoint.
func policyCharacterizationRead(t *testing.T, answers map[string]process.Result) (RemotePolicy, []process.Options) {
	t.Helper()
	var calls []process.Options
	runner := func(_ context.Context, o process.Options) process.Result {
		calls = append(calls, o)
		return answers[o.Argv[len(o.Argv)-1]]
	}
	return ReadRemotePolicy(context.Background(), "owner/repo", "release/v1", "work", runner), calls
}

func policyCharacterizationDigest(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadRemotePolicyArgvAndObservations(t *testing.T) {
	policy, calls := policyCharacterizationRead(t, map[string]process.Result{
		policyCharacterizationRulesEndpoint:      {OK: true, Stdout: policyCharacterizationFixture(t, "rules-branches.json")},
		policyCharacterizationProtectionEndpoint: {OK: true, Stdout: policyCharacterizationFixture(t, "branch-protection.json")},
	})
	wantArgv := [][]string{
		{"gh", "api", "--method", "GET", policyCharacterizationRulesEndpoint},
		{"gh", "api", "--method", "GET", policyCharacterizationProtectionEndpoint},
	}
	if len(calls) != len(wantArgv) {
		t.Fatalf("calls = %d, want %d", len(calls), len(wantArgv))
	}
	for i, call := range calls {
		if !reflect.DeepEqual(call.Argv, wantArgv[i]) {
			t.Fatalf("call %d argv = %q, want %q", i, call.Argv, wantArgv[i])
		}
		if call.Dir != "work" || call.Timeout != 15*time.Second || call.MaxOutputBytes != 256000 || call.Stdin == nil || len(call.Stdin) != 0 || call.Input != nil {
			t.Fatalf("call %d options = %+v", i, call)
		}
	}
	check := func(o PolicyObservation, source, canonical string) {
		t.Helper()
		if o.Source != source || o.Status != "observed" || o.Unknown != "" || string(o.Content) != canonical || o.SHA256 != policyCharacterizationDigest(canonical) {
			t.Fatalf("observation = %+v", o)
		}
		if _, err := time.Parse(time.RFC3339Nano, o.RetrievedAt); err != nil {
			t.Fatalf("retrievedAt %q: %v", o.RetrievedAt, err)
		}
	}
	check(policy.Rules, "https://api.github.com/"+policyCharacterizationRulesEndpoint, policyCharacterizationRulesCanonical)
	check(policy.Protection, "https://api.github.com/"+policyCharacterizationProtectionEndpoint, policyCharacterizationProtectCanonical)

	rules := "github-ruleset-sha256:" + policyCharacterizationDigest(policyCharacterizationRulesCanonical)
	protection := "github-protection-sha256:" + policyCharacterizationDigest(policyCharacterizationProtectCanonical)
	if err := policy.ValidateRequirements([]string{rules, protection}); err != nil {
		t.Fatalf("ValidateRequirements: %v", err)
	}
	if err := policy.ValidateRequirements(nil); err != nil {
		t.Fatalf("no requirements: %v", err)
	}
	refusals := map[string]string{
		"github-ruleset-sha256:" + strings.Repeat("0", 64):    "configured publication requirement cannot be verified or changed: github-ruleset-sha256",
		"github-protection-sha256:" + strings.Repeat("0", 64): "configured publication requirement cannot be verified or changed: github-protection-sha256",
		"github-ruleset-sha256:abc":                           "unsupported mandatory constraint: github-ruleset-sha256:abc",
		"no-separator":                                        "unsupported mandatory constraint: no-separator",
		"other-sha256:" + strings.Repeat("0", 64):             "unsupported mandatory constraint: other-sha256",
	}
	for requirement, want := range refusals {
		err := policy.ValidateRequirements([]string{rules, requirement})
		if err == nil || err.Error() != want {
			t.Fatalf("%s: err = %v, want %q", requirement, err, want)
		}
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadRemotePolicyUnknownStates(t *testing.T) {
	const unavailable = "Policy unavailable, unauthorized, timed out or exceeded bounds. No absence of protection is inferred."
	many := "[" + strings.TrimSuffix(strings.Repeat("{},", 100), ",") + "]"
	almost := "[" + strings.TrimSuffix(strings.Repeat("{},", 99), ",") + "]"
	cases := []struct {
		name                   string
		rules, protection      process.Result
		rulesWant, protWant    string
		rulesObserved, protObs bool
	}{
		{name: "non-zero exit", rules: process.Result{Code: 1, Stderr: "HTTP 404"}, protection: process.Result{Code: 1}, rulesWant: unavailable, protWant: unavailable},
		{name: "overflow and timeout", rules: process.Result{OK: true, Overflow: true}, protection: process.Result{OK: true, TimedOut: true}, rulesWant: unavailable, protWant: unavailable},
		{name: "interrupted", rules: process.Result{OK: true, Interrupted: true, Stdout: "[]"}, protection: process.Result{OK: true, Interrupted: true, Stdout: "{}"}, rulesWant: unavailable, protWant: unavailable},
		{name: "malformed json", rules: process.Result{OK: true, Stdout: "[{"}, protection: process.Result{OK: true, Stdout: "nope"}, rulesWant: "Invalid policy response", protWant: "Invalid policy response"},
		{name: "wrong shapes", rules: process.Result{OK: true, Stdout: "{}"}, protection: process.Result{OK: true, Stdout: "[]"}, rulesWant: "Expected a complete bounded rule array (100 or more rules requires explicit pagination support)", protWant: "Expected branch protection object"},
		{name: "100 rules", rules: process.Result{OK: true, Stdout: many}, protection: process.Result{OK: true, Stdout: "null"}, rulesWant: "Expected a complete bounded rule array (100 or more rules requires explicit pagination support)", protWant: "Expected branch protection object"},
		{name: "99 rules", rules: process.Result{OK: true, Stdout: almost}, protection: process.Result{OK: true, Stdout: "{}"}, rulesObserved: true, protObs: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy, calls := policyCharacterizationRead(t, map[string]process.Result{
				policyCharacterizationRulesEndpoint:      c.rules,
				policyCharacterizationProtectionEndpoint: c.protection,
			})
			if len(calls) != 2 {
				t.Fatalf("calls = %d, want both reads", len(calls))
			}
			check := func(o PolicyObservation, observed bool, unknown string) {
				t.Helper()
				if observed {
					if o.Status != "observed" || o.Unknown != "" || len(o.SHA256) != 64 {
						t.Fatalf("observation = %+v", o)
					}
					return
				}
				if o.Status != "unknown" || o.Unknown != unknown || o.SHA256 != "" || o.Content != nil {
					t.Fatalf("observation = %+v, want unknown %q", o, unknown)
				}
			}
			check(policy.Rules, c.rulesObserved, c.rulesWant)
			check(policy.Protection, c.protObs, c.protWant)
			if !c.rulesObserved {
				if err := policy.ValidateRequirements([]string{"github-ruleset-sha256:" + strings.Repeat("0", 64)}); err == nil {
					t.Fatal("an unknown policy satisfied a pinned requirement")
				}
			}
		})
	}
}
