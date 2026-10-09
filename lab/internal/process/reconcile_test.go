//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestReturnedProcessHasReconciliationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		code       int
		timeout    time.Duration
	}{
		{"success", "echo", 0, 30 * time.Second},
		{"nonzero-exit", "unicode", 7, 30 * time.Second},
		{"timeout", "wait", -1, 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run the test binary directly: Wait reaps the only process in its
			// managed group. A shell plus sleep can leave an observable descendant;
			// that uncertainty must remain a runtime failure, not be overridden.
			o := options(tc.mode)
			o.Timeout = tc.timeout
			r := Run(context.Background(), o)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var evidence map[string]any
			if err := json.Unmarshal(raw, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence["reconciled"] != true || r.Uncertainty != "" {
				t.Fatalf("missing durable stopped-process evidence: %s", raw)
			}
			if r.Code != tc.code || r.TimedOut != (tc.mode == "wait") || r.OK != (tc.code == 0) {
				t.Fatalf("unexpected process outcome: %s", raw)
			}
		})
	}
}

func TestLiveManagedDescendantMakesResultUncertain(t *testing.T) {
	r := Run(context.Background(), Options{Argv: []string{"/bin/sh", "-c", "sleep 2 >/dev/null 2>&1 & exit 0"}})
	if r.OK || r.Reconciled || r.Uncertainty == "" {
		t.Fatalf("live descendant accepted: %+v", r)
	}
}
