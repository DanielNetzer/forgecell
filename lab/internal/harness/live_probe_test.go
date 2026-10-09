package harness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"testing"
	"time"
)

func TestLiveAnalysisProbe(t *testing.T) {
	executable := os.Getenv("FORGECELL_LIVE_PROBE")
	if executable == "" {
		t.Skip("explicit live probe only")
	}
	request := map[string]any{"kind": "ticket-analysis", "issue": map[string]any{"number": 1, "title": "Improve it", "body": "Make it better"}, "repository": map[string]any{"files": []any{}, "evidence": []any{}}}
	r := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: executable, Request: request, Dir: t.TempDir(), Timeout: 90 * time.Second})
	if !r.Process.OK {
		t.Fatalf("error=%s stderr=%s", r.Process.Error, r.Process.Stderr)
	}
	a, err := readiness.DecodeAnalysis([]byte(r.Text))
	if err != nil || len(a.Questions) == 0 || len(a.Scope) > 0 {
		t.Fatalf("ambiguous probe must return questions, no coding scope: %s %v", r.Text, err)
	}
	t.Log(r.Text)
}
