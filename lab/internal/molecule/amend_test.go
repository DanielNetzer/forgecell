package molecule

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// A caller that reviewed one plan, while another amend recorded a newer one
// before its engine call, is refused as stale under the lock and records nothing.
func TestAmendFromLatestRefusesStaleExpectedParent(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	seen := latestPlan(r).Digest
	// The ledger moves forward between the caller's check and its engine call.
	advanced, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AddAcceptance: []string{"Someone else's note"}})
	if err != nil {
		t.Fatal(err)
	}
	before := ledgerBytes(t, advanced)
	_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{ExpectParent: seen, AddAcceptance: []string{"My note"}})
	if !errors.Is(err, ErrStalePlan) {
		t.Fatalf("stale expected parent was not refused as stale: %v", err)
	}
	if !bytes.Equal(before, ledgerBytes(t, advanced)) {
		t.Fatal("a stale amend changed the ledger")
	}
	// The digest the caller now sees is accepted and becomes the parent.
	current := latestPlan(advanced).Digest
	next, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{ExpectParent: current, AddAcceptance: []string{"My note"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := latestPlan(next).Plan.ParentDigest; got != current {
		t.Fatalf("parent = %s, want %s", got, current)
	}
}
