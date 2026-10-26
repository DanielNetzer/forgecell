package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"strings"
	"testing"
)

var (
	laneStatuses = []string{"running", "allocating", "blocked", "failed", "waiting", "completed", "interrupted", "future-status", ""}
	// Every readiness phase the engine defines, plus none and an unknown one.
	lanePhases = []string{"scope-waiting", "review-waiting", "approved", "coding", "check-pending", "dismissed", "scope-change", "scope-violation", "failed", "blocked", "unknown", "interrupted", "", "future-phase"}
	// Every receipt state and lifecycle, plus no receipt and unrecognised values.
	laneReceipts = []*DeliveryState{
		nil,
		{State: "prepared"}, {State: "committed"}, {State: "committed", Lifecycle: "OPEN"},
		{State: "published"}, {State: "published", Lifecycle: "OPEN"}, {State: "published", Lifecycle: "MERGED"},
		{State: "published", Lifecycle: "CLOSED"}, {State: "published", Lifecycle: "UNKNOWN"}, {State: "published", Lifecycle: "MISSING"}, {State: "published", Lifecycle: "AMBIGUOUS"}, {State: "published", Lifecycle: "FUTURE"},
		{State: "unreadable"},
	}
)

// wantLane restates the operator's order independently of Lane.
func wantLane(status, phase string, d *DeliveryState) string {
	published := d != nil && d.State == "published" && (d.Lifecycle == "" || d.Lifecycle == "OPEN" || d.Lifecycle == "MERGED")
	if status == "blocked" || status == "failed" {
		return LaneBlocked
	}
	// A published draft outranks review-waiting, never scope-waiting (2026-10-26).
	if phase == "scope-waiting" || phase == "review-waiting" && !published {
		return LaneDecision
	}
	if status == "running" || status == "allocating" || phase == "approved" || phase == "coding" || phase == "check-pending" {
		return LaneRunning
	}
	if published {
		return LaneFinished
	}
	return LaneOther
}

func TestUILaneOrderCoversEveryStatusPhaseAndReceipt(t *testing.T) {
	for _, status := range laneStatuses {
		for _, phase := range lanePhases {
			for _, d := range laneReceipts {
				got, want := Lane(status, phase, d), wantLane(status, phase, d)
				if got != want {
					t.Errorf("Lane(%q, %q, %+v) = %q, want %q", status, phase, d, got, want)
				}
				label := DeliveryLabel(d)
				if strings.Contains(strings.ToLower(label), "merge") {
					t.Errorf("delivery label %q claims a merge", label)
				}
				if d != nil && !d.Published() && !strings.HasPrefix(label, "Delivery in progress") {
					t.Errorf("unpublished receipt %+v labeled %q", d, label)
				}
				if d != nil && !d.Published() && !strings.Contains(label, d.State) {
					t.Errorf("label %q hides receipt state %q", label, d.State)
				}
			}
		}
	}
}

func TestUILaneExamplesFromTheOperatorDecision(t *testing.T) {
	published := &DeliveryState{State: "published", Lifecycle: "OPEN"}
	for _, c := range []struct {
		status, phase string
		d             *DeliveryState
		want          string
	}{
		{"blocked", "", published, LaneBlocked},
		{"failed", "coding", published, LaneBlocked},
		{"failed", "review-waiting", nil, LaneBlocked},
		{"blocked", "review-waiting", published, LaneBlocked},
		{"waiting", "scope-waiting", published, LaneDecision},
		{"running", "review-waiting", nil, LaneDecision},
		// forgecell deliver publishes without moving the phase: a delivered
		// Molecule is finished, not waiting for a decision.
		{"waiting", "review-waiting", published, LaneFinished},
		{"waiting", "review-waiting", &DeliveryState{State: "published", Lifecycle: "MERGED"}, LaneFinished},
		{"waiting", "review-waiting", &DeliveryState{State: "committed", Lifecycle: "OPEN"}, LaneDecision},
		{"waiting", "review-waiting", &DeliveryState{State: "published", Lifecycle: "CLOSED"}, LaneDecision},
		{"waiting", "review-waiting", nil, LaneDecision},
		{"running", "", nil, LaneRunning},
		{"allocating", "", nil, LaneRunning},
		{"waiting", "check-pending", published, LaneRunning},
		{"completed", "", published, LaneFinished},
		{"completed", "", &DeliveryState{State: "published", Lifecycle: "MERGED"}, LaneFinished},
		{"completed", "", &DeliveryState{State: "prepared"}, LaneOther},
		{"completed", "", &DeliveryState{State: "committed"}, LaneOther},
		{"completed", "", &DeliveryState{State: "published", Lifecycle: "CLOSED"}, LaneOther},
		{"completed", "", nil, LaneOther},
		{"waiting", "dismissed", nil, LaneOther},
		{"waiting", "future-phase", nil, LaneOther},
	} {
		if got := Lane(c.status, c.phase, c.d); got != c.want {
			t.Errorf("Lane(%q, %q, %+v) = %q, want %q", c.status, c.phase, c.d, got, c.want)
		}
	}
	if DeliveryLabel(published) != "Draft published" || DeliveryLabel(nil) != "" {
		t.Fatal("delivery labels")
	}
	if l := DeliveryLabel(&DeliveryState{State: "published", Lifecycle: "UNKNOWN"}); l != "Delivery in progress · receipt published · lifecycle UNKNOWN" {
		t.Fatalf("unresolved label %q", l)
	}
}

// Lane counts served by /api/molecules always reconcile with its own list, and
// the lanes plus Other sum to All.
func TestUILaneCountsReconcileWithMoleculeList(t *testing.T) {
	lab := t.TempDir()
	n := 0
	for _, status := range []string{"running", "allocating", "blocked", "failed", "waiting", "completed"} {
		for _, phase := range []string{"scope-waiting", "review-waiting", "coding", "dismissed", ""} {
			for _, receipt := range []string{"", "prepared", "committed", "published", "published-CLOSED"} {
				n++
				id := fmt.Sprintf("m%03d", n)
				r := molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: id, Status: status, FormulaID: "f", Mode: "ticket", StartedAt: fmt.Sprintf("2026-10-24T10:%02d:%02dZ", n/60, n%60), LabDir: lab, Issue: molecule.Issue{Repo: "o/r", Number: int64(n), Title: "t"}}
				if phase != "" {
					r.Readiness = readinessPhase(phase)
				}
				writeRecord(t, lab, r)
				switch receipt {
				case "":
				case "published-CLOSED":
					writeReceipt(t, lab, id, "published", "CLOSED")
				default:
					writeReceipt(t, lab, id, receipt, "")
				}
			}
		}
	}
	var list MoleculeList
	decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules")), &list)
	if !list.Complete || len(list.Molecules) != n || list.Counts["all"] != n {
		t.Fatalf("list of %d: complete %t, %d molecules, counts %v, %v", n, list.Complete, len(list.Molecules), list.Counts, list.Diagnostics)
	}
	tally := map[string]int{}
	for _, m := range list.Molecules {
		tally[m.Lane]++
	}
	sum := 0
	for _, lane := range Lanes {
		if list.Counts[lane] != tally[lane] {
			t.Errorf("count %s = %d, list has %d", lane, list.Counts[lane], tally[lane])
		}
		sum += list.Counts[lane]
	}
	if sum != list.Counts["all"] || len(tally) > len(Lanes) {
		t.Fatalf("lanes sum to %d of %d: %v", sum, list.Counts["all"], tally)
	}
	for _, lane := range Lanes {
		if list.Counts[lane] == 0 {
			t.Errorf("fixture never reached lane %s", lane)
		}
	}
	if _, ok := list.Counts["paused"]; ok {
		t.Fatal("a paused lane was counted")
	}
	for i := 1; i < len(list.Molecules); i++ {
		if list.Molecules[i-1].StartedAt > list.Molecules[i].StartedAt {
			t.Fatal("molecules are not ordered earliest first")
		}
	}
	// The selected-Molecule projection agrees with its summary.
	for _, m := range list.Molecules[:10] {
		var d Detail
		decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules/"+m.ID)), &d)
		if d.Lane != m.Lane {
			t.Errorf("%s: detail lane %q, summary %q", m.ID, d.Lane, m.Lane)
		}
	}
}

func readinessPhase(phase string) *readiness.State {
	return &readiness.State{SchemaVersion: "v1", Phase: phase, Plans: []readiness.Proposal{}, Events: []readiness.Event{}}
}
