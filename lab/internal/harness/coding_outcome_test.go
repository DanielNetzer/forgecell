package harness

import (
	"context"
	"strings"
	"testing"
)

func TestStrictCodingOutcome(t *testing.T) {
	valid := `{"schemaVersion":"v1","outcome":"completed","reason":"Implemented and tested","paths":[]}`
	for _, raw := range []string{valid, strings.Replace(valid, "completed", "blocked", 1), strings.Replace(valid, "completed", "no-change", 1), `{"schemaVersion":"v1","outcome":"scope-change","reason":"helper needed","paths":["lab/helper.go"]}`} {
		if _, err := DecodeCodingOutcome([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"", "blocked", "null", valid + valid, strings.Replace(valid, `"outcome"`, `"Outcome"`, 1), strings.Replace(valid, `"paths":[]`, `"paths":null`, 1), strings.Replace(valid, `"paths":[]`, `"paths":[],"extra":1`, 1), strings.Replace(valid, `"outcome":"completed"`, `"outcome":"completed","outcome":"blocked"`, 1), strings.Replace(valid, "completed", "unknown", 1), strings.Repeat(" ", MaxCodingOutcomeBytes+1)} {
		if _, err := DecodeCodingOutcome([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw[:min(len(raw), 100)])
		}
	}
}

func TestCodexMalformedFinalPreservesTransport(t *testing.T) {
	executable := fake(t, `while [ "$#" -gt 0 ]; do
 if [ "$1" = "--output-last-message" ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
printf 'blocked by unwritable Go cache' > "$out"
`)
	r := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}})
	if !r.Process.OK || r.Process.Code != 0 || !r.Process.Reconciled || r.Coding == nil || r.Coding.Outcome != "unknown" || r.Raw != "blocked by unwritable Go cache" || r.Process.RawStdout != r.Raw {
		t.Fatalf("lost transport evidence: %+v", r)
	}
}
