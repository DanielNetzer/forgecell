package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalSuggestionCannotActivateEvaluation(t *testing.T) {
	o, _ := evaluationFixture(t)
	_, err := Run(context.Background(), o)
	if err == nil {
		t.Fatal("historical suggestion authorized execution")
	}
	if _, err = os.Stat(o.Output); !os.IsNotExist(err) {
		t.Fatal("missing activation mutated output", err)
	}
}

func TestStaleEvaluationActivationHasNoOutputMutation(t *testing.T) {
	for _, kind := range []string{"plan", "variant", "destination"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := evaluationFixture(t)
			approveEvaluation(t, &o)
			switch kind {
			case "plan":
				b, _ := os.ReadFile(o.Inputs)
				os.WriteFile(o.Inputs, append(b, '\n'), 0600)
			case "variant":
				var p Plan
				b, _ := os.ReadFile(o.Inputs)
				json.Unmarshal(b, &p)
				file := filepath.Join(filepath.Dir(o.Inputs), p.Approval)
				raw, _ := os.ReadFile(file)
				var a Approval
				json.Unmarshal(raw, &a)
				a.OriginalYAML += "# extra bytes\n"
				a.OriginalHash = Hash([]byte(a.OriginalYAML))
				raw, _ = json.Marshal(a)
				os.WriteFile(file, raw, 0600)
				p.ApprovalSHA256 = Hash(raw)
				b, _ = json.Marshal(p)
				os.WriteFile(o.Inputs, b, 0600)
			case "destination":
				o.Output += "-different"
			}
			if _, e := Run(context.Background(), o); e == nil {
				t.Fatal("stale digest executed")
			}
			if _, e := os.Stat(o.Output); !os.IsNotExist(e) {
				t.Fatal("stale approval mutated output", e)
			}
		})
	}
}

func TestUnapprovedEvaluationPerformsNoSetupCheckOrModelCalls(t *testing.T) {
	o, _ := evaluationFixture(t)
	dir := filepath.Dir(o.Inputs)
	marker := filepath.Join(dir, "calls")
	var p Plan
	raw, _ := os.ReadFile(o.Inputs)
	json.Unmarshal(raw, &p)
	command := Command{Name: "setup", Dir: ".", Argv: []string{"/bin/sh", "-c", "echo setup >> " + marker}, TimeoutMS: 5000}
	p.Setup = []Command{command}
	p.Checks[0].Argv = []string{"/bin/sh", "-c", "echo check >> " + marker}
	raw, _ = json.Marshal(p)
	os.WriteFile(o.Inputs, raw, 0600)
	harness := filepath.Join(filepath.Dir(dir), "harness")
	os.WriteFile(harness, []byte("#!/bin/sh\necho model >> "+marker+"\n"), 0700)
	if _, e := Run(context.Background(), o); e == nil {
		t.Fatal("missing exact decision accepted")
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("unapproved evaluation executed a command", e)
	}
	if _, e := os.Stat(o.Output); !os.IsNotExist(e) {
		t.Fatal("unapproved evaluation created output", e)
	}
}
