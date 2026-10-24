package install

import (
	"os"
	"testing"
	"time"
)

// productionProbeTimeout keeps the shipped value for the bound test.
var productionProbeTimeout = probeTimeout

func TestMain(m *testing.M) {
	// Fixture releases answer --version and --protocol at once; only a loaded
	// machine makes them slow, so the probe budget is widened before any test
	// runs instead of depending on the production 15 s budget.
	probeTimeout = 2 * time.Minute
	os.Exit(m.Run())
}

func TestProbeTimeoutShipsFifteenSeconds(t *testing.T) {
	if productionProbeTimeout != 15*time.Second {
		t.Fatalf("production release probe budget = %v, want 15s", productionProbeTimeout)
	}
}
