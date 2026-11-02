package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
)

// The Issues view shows the change request's current head: the last fix
// round's commit, otherwise the first published commit.
func TestIssuesPublishedCommitIsTheHead(t *testing.T) {
	lab := t.TempDir()
	hh := testHandler(t, Options{LabDir: lab}).(*handler)
	first, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	statuses := map[string]delivery.ReceiptStatus{
		"no-rounds": {State: "published", URL: "https://github.com/owner/repo/pull/7", MoleculeID: "no-rounds", Commit: first},
		"rounds":    {State: "published", URL: "https://github.com/owner/repo/pull/7", MoleculeID: "rounds", Commit: first, Head: head},
		"bad-head":  {State: "published", URL: "https://github.com/owner/repo/pull/7", MoleculeID: "bad-head", Commit: first, Head: "<b>"},
	}
	hh.receipts = func(_, id string) (delivery.ReceiptStatus, bool, error) {
		s, ok := statuses[id]
		return s, ok, nil
	}
	for id, want := range map[string]published{
		"no-rounds": {number: 7, commit: first},
		"rounds":    {number: 7, commit: head},
		"bad-head":  {number: 7},
	} {
		if got := hh.published(id, "owner/repo"); got != want {
			t.Errorf("%s: %+v, want %+v", id, got, want)
		}
	}
}

// The real receipt reader feeds the head from a recorded fix round.
func TestIssuesPublishedCommitReadsTheFixRoundHead(t *testing.T) {
	lab := t.TempDir()
	hh := testHandler(t, Options{LabDir: lab}).(*handler)
	first, head := strings.Repeat("a", 40), strings.Repeat("c", 40)
	fr, _ := json.Marshal(delivery.FixRound{Round: 1, Parent: first, Commit: head, Tree: strings.Repeat("d", 40), PlanDigest: strings.Repeat("e", 64), VerificationTree: strings.Repeat("f", 40), Findings: []string{}, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: head})
	raw := `{"state":"published","commit":"` + first + `","url":"https://github.com/owner/repo/pull/8","preview":{"moleculeId":"mol-head"},"fixRounds":[` + string(fr) + `]}`
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", "mol-head.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if got := hh.published("mol-head", "owner/repo"); got != (published{number: 8, commit: head}) {
		t.Fatalf("published %+v", got)
	}
}
