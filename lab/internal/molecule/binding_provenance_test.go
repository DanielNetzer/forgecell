package molecule

import (
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"strings"
	"testing"
)

func TestHarnessAttemptProvenanceIsAdditiveInTheLedger(t *testing.T) {
	harness.SetRunningVersion("0.3.0")
	t.Cleanup(func() { harness.SetRunningVersion("") })
	provenance := harness.RecordProvenance([]string{"/missing/forgecell", "__adapter", "codex", "/codex"})
	raw, err := json.Marshal(HarnessAttempt{ID: "a1", PlanDigest: "d", Forgecell: &provenance})
	if err != nil {
		t.Fatal(err)
	}
	var decoded HarnessAttempt
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Forgecell == nil {
		t.Fatalf("%v %s", err, raw)
	}
	got := *decoded.Forgecell
	if got.ForgecellVersion != "0.3.0" || len(got.ExecutableSHA256) != 64 || got.ProtocolVersion != harness.AdapterProtocolVersion || got.SavedLauncher != "/missing/forgecell" || got.LauncherClass != harness.LauncherMissing {
		t.Fatalf("%+v", got)
	}
}

func TestOlderHarnessAttemptsWithoutProvenanceStillRoundTrip(t *testing.T) {
	legacy := `{"id":"a0","planDigest":"d","result":{"ok":true,"code":0}}`
	var attempt HarnessAttempt
	if err := json.Unmarshal([]byte(legacy), &attempt); err != nil || attempt.Forgecell != nil || attempt.ID != "a0" {
		t.Fatalf("%v %+v", err, attempt)
	}
	raw, err := json.Marshal(attempt)
	if err != nil || strings.Contains(string(raw), "forgecell") {
		t.Fatalf("an older attempt gained a provenance field: %v %s", err, raw)
	}
}
