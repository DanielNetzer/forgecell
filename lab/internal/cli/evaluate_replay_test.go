package cli

import (
	"context"
	"strings"
	"testing"
)

func TestEvaluateReplayRequiresFlags(t *testing.T) {
	cases := [][]string{
		{"evaluate", "replay", "--pr", "7", "--out", "d"},                           // no --repo
		{"evaluate", "replay", "--repo", "o/r", "--pr", "7"},                        // no --out
		{"evaluate", "replay", "--repo", "o/r", "--out", "d"},                       // no --pr
		{"evaluate", "replay", "--repo", "o/r", "--pr", "7", "--out", "d", "extra"}, // trailing arg
	}
	for _, args := range cases {
		var out, stderr strings.Builder
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test")
		if code != 1 {
			t.Fatalf("args %v: code = %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "evaluate replay requires") {
			t.Fatalf("args %v: stderr = %q", args, stderr.String())
		}
		if out.Len() != 0 {
			t.Fatalf("args %v: unexpected stdout %q", args, out.String())
		}
	}
}

func TestEvaluateReplayRejectsBadPR(t *testing.T) {
	var out, stderr strings.Builder
	code := Run(context.Background(), []string{"evaluate", "replay", "--repo", "o/r", "--pr", "abc", "--out", "d"}, strings.NewReader(""), &out, &stderr, "test")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid --pr") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
