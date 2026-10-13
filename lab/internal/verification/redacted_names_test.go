package verification

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const secretShapedTest = "TestToken_ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"

func TestRedactedFailureNamesUndeclaredFailure(t *testing.T) {
	failed := []TestID{{Package: "p", Test: secretShapedTest}}
	declared := []readiness.ProtectedDelta{{Package: "p", Test: "TestOther"}}
	_, err := matchDeltas(declared, false, failed)
	if err == nil {
		t.Fatal("an undeclared failure must be refused")
	}
	if strings.Contains(err.Error(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("a secret-shaped test name reached the error text: %v", err)
	}
	if !strings.Contains(err.Error(), "undeclared protected regression failures") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRedactedFailureNamesUnfinishedTest(t *testing.T) {
	stream := jsonEvents(t,
		map[string]string{"Action": "run", "Package": "p", "Test": secretShapedTest},
		map[string]string{"Action": "fail", "Package": "p"},
	)
	_, err := parseFailedTests(stream)
	if err == nil || strings.Contains(err.Error(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("expected a redacted refusal, got %v", err)
	}
}

func TestRedactedFailureNamesKeepOriginalsForMatching(t *testing.T) {
	stream := failingTestStream(t, secretShapedTest, "--- FAIL\n")
	failed, err := parseFailedTests(stream)
	if err != nil || len(failed) != 1 || failed[0].Test != secretShapedTest {
		t.Fatalf("the parsed set must keep the original name: %v %v", failed, err)
	}
	declared := []readiness.ProtectedDelta{{Package: "p", Test: secretShapedTest}}
	e, err := matchDeltas(declared, false, failed)
	if err != nil || len(e.Accepted) != 1 {
		t.Fatalf("a declared secret-shaped name must still match in memory: %+v %v", e, err)
	}
}
