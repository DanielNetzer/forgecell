package readiness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"testing"
)

func TestPolicyUnknownNeverMeansNoRules(t *testing.T) {
	unavailable := func(context.Context, process.Options) process.Result {
		return process.Result{Error: "permission denied"}
	}
	got := ReadRemotePolicy(context.Background(), "owner/repo", "main", ".", unavailable)
	if got.Rules.Status != "unknown" || got.Protection.Status != "unknown" {
		t.Fatalf("%+v", got)
	}
	if err := got.ValidateRequirements([]string{"github-ruleset-sha256:" + "abc"}); err == nil {
		t.Fatal("unknown policy accepted")
	}
	if err := got.ValidateRequirements(nil); err != nil {
		t.Fatal("advisory unknown blocked")
	}
	good := func(_ context.Context, o process.Options) process.Result {
		return process.Result{OK: true, Stdout: "[]"}
	}
	got = ReadRemotePolicy(context.Background(), "owner/repo", "main", ".", good)
	if got.Rules.Status != "observed" || len(got.Rules.SHA256) != 64 {
		t.Fatalf("%+v", got)
	}
	if err := got.ValidateRequirements([]string{"github-ruleset-sha256:" + got.Rules.SHA256}); err != nil {
		t.Fatal(err)
	}
	if err := got.ValidateRequirements([]string{"unknown-rule"}); err == nil {
		t.Fatal("unsupported requirement accepted")
	}
}
