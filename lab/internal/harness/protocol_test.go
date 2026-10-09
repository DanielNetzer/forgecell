package harness

import (
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAdapterCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Kind, Input string
		Args            []string
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.ID+"/"+c.Kind, func(t *testing.T) {
			got, err := BuildInvocation(c.ID, map[string]any{"kind": c.Kind}, "/fixture/result")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Args, c.Args) || got.Input != c.Input {
				t.Fatalf("adapter contract changed: %#v", got)
			}
		})
	}
}
func TestResultsRejectErrorsAndPermissionDenials(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `not json`, `{"result":""}`, `{"is_error":true,"result":"failed"}`, `{"result":"done","permission_denials":[{"tool":"Bash"}]}`} {
		if _, err := ReadResult("claude-code", raw, false); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	result, err := ReadResult("cursor", `{"result":"done"}`, false)
	if err != nil || result != "done" {
		t.Fatalf("%s %v", result, err)
	}
	if _, err := BuildInvocation("other", map[string]any{"kind": "molecule"}, "output"); err == nil {
		t.Fatal("unknown adapter accepted")
	}
	if _, err := BuildInvocation("codex", map[string]any{"kind": "other"}, "output"); err == nil {
		t.Fatal("unknown request accepted")
	}
}

func TestAnalysisEvidenceRequestContract(t *testing.T) {
	invocation, err := BuildInvocation("codex", map[string]any{"kind": "ticket-analysis"}, "output")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"evidenceRequests", "one read-only supplemental pass", "Tree entries alone", "never grants scope approval", "Do not execute commands"} {
		if !strings.Contains(invocation.Input, phrase) {
			t.Fatalf("missing contract %s", phrase)
		}
	}
	var schema map[string]any
	if err = json.Unmarshal([]byte(readiness.AnalysisSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	if props["evidenceRequests"] == nil {
		t.Fatal("request schema absent")
	}
}

func TestCapabilitiesMatchInvocationSupport(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		c := AdapterCapabilities(id)
		for _, op := range []struct {
			kind       string
			capability Capability
		}{{"molecule", c.Coding}, {"ticket-analysis", c.Analysis}, {"formula-improvement", c.Meta}} {
			_, err := BuildInvocation(id, map[string]any{"kind": op.kind}, "/fixture/output")
			if (err == nil) != (op.capability.State == "supported") {
				t.Fatalf("%s %s: %+v %v", id, op.kind, op.capability, err)
			}
			if err != nil && op.capability.Reason != err.Error() {
				t.Fatal("refusal reason differs")
			}
		}
		if c.ModelAccess.State != "unknown" || c.AnalysisControls.State != "unknown" {
			t.Fatal(c)
		}
	}
	if AdapterCapabilities("custom").Coding.State != "unknown" {
		t.Fatal("invented custom support")
	}
}
