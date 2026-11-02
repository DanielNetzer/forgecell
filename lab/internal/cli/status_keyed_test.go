package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// TestLoopStatusDoctorShowsAKeyedIntakeByItsKey: a tracker plugin's Issue in
// flight is listed by its key, never as Issue #0, and a numeric one as before.
func TestLoopStatusDoctorShowsAKeyedIntakeByItsKey(t *testing.T) {
	_, lab := doctorFixture(t)
	held := holdLoopMarker(t, lab, molecule.LoopStarterCLI, 0)
	held.TrackIntakeKey("ENG-1")
	held.TrackIntake(242)
	_, out := runDoctor(t, lab, "--json")
	loop, _ := decodeDoctor(t, out)["loop"].(map[string]any)
	status, _ := loop["status"].(map[string]any)
	if intakes, _ := status["intake"].([]any); len(intakes) != 2 || fmt.Sprint(intakes[0]) != "map[issue:242 phase:planning]" || fmt.Sprint(intakes[1]) != "map[key:ENG-1 phase:planning]" {
		t.Fatalf("doctor intake %v", status["intake"])
	}
	_, text := runDoctor(t, lab, "--verbose")
	if !strings.Contains(text, "2 in flight") || !strings.Contains(text, "  Issue #242 · planning\n  Issue ENG-1 · planning\n") || strings.Contains(text, "#0") {
		t.Fatalf("doctor text:\n%s", text)
	}
	held.UntrackIntakeKey("ENG-1")
	if _, text = runDoctor(t, lab, "--verbose"); strings.Contains(text, "ENG-1") {
		t.Fatalf("doctor text after untrack:\n%s", text)
	}
}

// TestRenderDoctorLoopFlattensAKeyedIntake: a keyed entry is shown through the
// same display flattening as every other loop field.
func TestRenderDoctorLoopFlattensAKeyedIntake(t *testing.T) {
	var out bytes.Buffer
	status := molecule.LoopStatus{Starter: molecule.LoopStarterCLI, StartedAt: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), Selector: "filter:123", MaxConcurrent: 1, InFlight: []string{},
		Intake: []molecule.LoopIntake{{Key: "ENG-1\x1b[2J", Phase: molecule.LoopIntakePlanning}}}
	renderDoctorLoop(&out, molecule.LoopMarkerState{Running: true, State: molecule.LoopMarkerRunning, Held: true, Status: &status})
	text := out.String()
	want := "  Issue " + flatten("ENG-1\x1b[2J") + " · planning\n"
	if strings.Contains(text, "\x1b") || strings.Contains(text, "#0") || !strings.Contains(text, want) {
		t.Fatalf("doctor text:\n%q", text)
	}
}
