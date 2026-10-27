package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// markerLoop stubs the engine loop with what RunLoop does with its marker: take
// it once the Formula has loaded, before listing, and keep it for the caller to
// release. It records the marker each pass received.
func markerLoop(t *testing.T) *[]*molecule.LoopMarker {
	t.Helper()
	seen := &[]*molecule.LoopMarker{}
	stubLoop(t, func(_ context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		*seen = append(*seen, o.Marker)
		if o.Marker == nil {
			return molecule.LoopReport{}, errors.New("the loop received no marker")
		}
		if err := o.Marker.Acquire(o.Template.LabDir, o.Select, o.MaxConcurrent); err != nil {
			return molecule.LoopReport{}, err
		}
		return molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{}}}, nil
	})
	return seen
}

func holdLoopMarker(t *testing.T, lab, starter string, watch time.Duration) *molecule.LoopMarker {
	t.Helper()
	m := molecule.NewLoopMarker(starter, watch)
	if err := m.Acquire(lab, intake.Selector{Kind: "milestone", Value: "v1"}, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Release)
	return m
}

func TestLoopMarkerSecondCLILoopIsRefused(t *testing.T) {
	lab := t.TempDir()
	seen := markerLoop(t)
	other := holdLoopMarker(t, lab, molecule.LoopStarterUI, 0)
	code, out, stderr := runLoopCLI("--lab", lab, "--once")
	if code != 1 || out != "" || !strings.Contains(stderr, "another loop is running in this Lab") || !strings.Contains(stderr, "Lab UI") || strings.Contains(stderr, lab) {
		t.Fatalf("exit %d, output %q, stderr %q", code, out, stderr)
	}
	code, out, stderr = triggersWatch(t, context.Background(), func(time.Duration) bool {
		t.Error("a refused watch waited for another pass")
		return false
	}, "--lab", lab, "--watch", "--interval", "2m")
	if code != 1 || strings.Contains(out, "Lab loop for") || !strings.Contains(stderr, "One loop runs per Lab") {
		t.Fatalf("watch: exit %d, output %q, stderr %q", code, out, stderr)
	}
	if got := molecule.ReadLoopStatus(lab); !got.Running || got.Status.Starter != molecule.LoopStarterUI || got.Status.Token != other.Token() {
		t.Fatalf("a refused loop changed the holder's status: %+v", got)
	}
	other.Release()
	if code, _, stderr = runLoopCLI("--lab", lab, "--once"); code != 0 {
		t.Fatalf("exit %d after the other loop ended: %s", code, stderr)
	}
	if len(*seen) != 3 || (*seen)[2] == nil {
		t.Fatalf("passes %d", len(*seen))
	}
	if got := molecule.ReadLoopStatus(lab); got.Running || got.State != molecule.LoopMarkerNotRunning {
		t.Fatalf("the CLI loop did not release its marker: %+v", got)
	}
}

func TestLoopMarkerWatchHoldsTheMarkerAcrossPasses(t *testing.T) {
	lab := t.TempDir()
	seen := markerLoop(t)
	waits := 0
	code, _, stderr := triggersWatch(t, context.Background(), func(d time.Duration) bool {
		waits++
		got := molecule.ReadLoopStatus(lab)
		if !got.Running || got.Status.Starter != molecule.LoopStarterCLI || got.Status.WatchIntervalSeconds != 120 || got.Status.Selector != "label:ready" || got.Status.MaxConcurrent != 2 {
			t.Errorf("between passes the marker is not held: %+v", got)
		}
		if err := molecule.NewLoopMarker(molecule.LoopStarterUI, 0).Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1); !errors.Is(err, molecule.ErrLoopRunning) {
			t.Errorf("another loop slipped in between passes: %v", err)
		}
		return waits < 2
	}, "--lab", lab, "--watch", "--interval", "2m", "--select", "label:ready", "--max-concurrent", "2")
	if code != 0 || waits != 2 {
		t.Fatalf("exit %d after %d waits: %s", code, waits, stderr)
	}
	if len(*seen) != 2 || (*seen)[0] == nil || (*seen)[0] != (*seen)[1] {
		t.Fatalf("the passes did not share one marker: %v", *seen)
	}
	if got := molecule.ReadLoopStatus(lab); got.Running || got.State != molecule.LoopMarkerNotRunning {
		t.Fatalf("the watch did not release its marker: %+v", got)
	}
}

func TestIssueLockCLIRunUsesTheLockedIntakeAndPrintsItsRefusal(t *testing.T) {
	lab := t.TempDir()
	previous := runIssue
	t.Cleanup(func() { runIssue = previous })
	var got []molecule.Options
	runIssue = func(_ context.Context, o molecule.Options) (molecule.Record, error) {
		got = append(got, o)
		return molecule.Record{}, fmt.Errorf("%w: a running loop (forgecell loop or a Lab UI) holds the start lock for Issue #12; wait for it to finish, then run this again. Nothing was started", molecule.ErrIssueBusy)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"run", "12", "--lab", lab}, nil, &out, &stderr, "test")
	if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "running loop") || !strings.Contains(stderr.String(), "Nothing was started") {
		t.Fatalf("exit %d, output %q, stderr %q", code, out.String(), stderr.String())
	}
	if len(got) != 1 || got[0].Issue != "12" || got[0].Approve != "" || got[0].LabDir != lab {
		t.Fatalf("run options %+v", got)
	}
	// The real entry point runs Run's own checks before taking the lock.
	runIssue = previous
	out.Reset()
	stderr.Reset()
	if code = Run(context.Background(), []string{"run", "12", "--lab", lab}, nil, &out, &stderr, "test"); code != 1 || !strings.Contains(stderr.String(), "approve a Formula") {
		t.Fatalf("an unapproved Lab: exit %d, stderr %q", code, stderr.String())
	}
}

func TestLoopStatusDoctorShowsTheLoop(t *testing.T) {
	_, lab := doctorFixture(t)
	idle, _ := runDoctor(t, lab, "--json")
	held := holdLoopMarker(t, lab, molecule.LoopStarterCLI, 2*time.Minute)
	code, out := runDoctor(t, lab, "--json")
	if code != idle {
		t.Fatalf("the loop status changed the readiness verdict: %d, was %d", code, idle)
	}
	loop, _ := decodeDoctor(t, out)["loop"].(map[string]any)
	status, _ := loop["status"].(map[string]any)
	if loop["running"] != true || loop["state"] != molecule.LoopMarkerRunning || status["starter"] != molecule.LoopStarterCLI || status["selector"] != "milestone:v1" || status["maxConcurrent"] != float64(2) || status["watchIntervalSeconds"] != float64(120) {
		t.Fatalf("doctor loop %v", loop)
	}
	raw, err := json.Marshal(loop)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), lab) || strings.Contains(out, held.Token()) {
		t.Fatalf("the loop status carries a path or the token: %s", raw)
	}
	if _, text := runDoctor(t, lab, "--verbose"); !strings.Contains(text, "Loop: running · started in a terminal") || !strings.Contains(text, "select milestone:v1 · max concurrent 2 · 0 in flight") || !strings.Contains(text, "Watching every 2m0s") {
		t.Fatalf("doctor text:\n%s", text)
	}
	// Intake in flight is listed by Issue until Run names its Molecule.
	held.TrackIntake(242)
	_, out = runDoctor(t, lab, "--json")
	loop, _ = decodeDoctor(t, out)["loop"].(map[string]any)
	status, _ = loop["status"].(map[string]any)
	if intakes, _ := status["intake"].([]any); len(intakes) != 1 || fmt.Sprint(intakes[0]) != "map[issue:242 phase:planning]" {
		t.Fatalf("doctor intake %v", status["intake"])
	}
	if _, text := runDoctor(t, lab, "--verbose"); !strings.Contains(text, "1 in flight") || !strings.Contains(text, "  Issue #242 · planning\n") {
		t.Fatalf("doctor text:\n%s", text)
	}
	held.Track("mol-242-a")
	held.UntrackIntake(242)
	if _, text := runDoctor(t, lab, "--verbose"); strings.Contains(text, "Issue #242") || !strings.Contains(text, "  Molecule mol-242-a · in flight\n") {
		t.Fatalf("doctor text:\n%s", text)
	}
	held.Release()
	_, out = runDoctor(t, lab, "--json")
	if loop, _ = decodeDoctor(t, out)["loop"].(map[string]any); loop["running"] != false || loop["state"] != molecule.LoopMarkerNotRunning {
		t.Fatalf("doctor loop after the loop ended %v", loop)
	}
	if _, text := runDoctor(t, lab, "--verbose"); !strings.Contains(text, "\nLoop: not running\n") {
		t.Fatalf("doctor text:\n%s", text)
	}
}
