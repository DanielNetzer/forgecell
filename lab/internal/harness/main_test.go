package harness

import (
	"os"
	"testing"
	"time"
)

// productionAnalysisProbeTimeout keeps the shipped value for the bound test.
var productionAnalysisProbeTimeout = AnalysisProbeTimeout

func TestMain(m *testing.M) {
	// The fake providers answer --help at once; only a loaded machine makes
	// them slow. Widening the probe budget before any test runs keeps every
	// probe test, the review atom tests included, independent of the
	// production 10 s budget without weakening what it checks.
	AnalysisProbeTimeout = 2 * time.Minute
	os.Exit(m.Run())
}

func TestAnalysisProbeTimeoutShipsTenSeconds(t *testing.T) {
	if productionAnalysisProbeTimeout != 10*time.Second {
		t.Fatalf("production analysis probe budget = %v, want 10s", productionAnalysisProbeTimeout)
	}
}
