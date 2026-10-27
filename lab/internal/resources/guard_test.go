package resources

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// scripted returns the readings in order and then repeats the last one.
type scripted struct {
	sync.Mutex
	readings []Readings
	calls    int
}

func (s *scripted) Read() Readings {
	s.Lock()
	defer s.Unlock()
	r := s.readings[min(s.calls, len(s.readings)-1)]
	s.calls++
	return r
}

func reading(freeDisk, memory uint64) Readings {
	return Readings{Volumes: []Volume{{Path: "/lab", Free: freeDisk, Total: 100 * GiB}}, Memory: memory, MemoryKnown: true}
}

// TestVerificationSlotsGuarded: extra verification slots are allowed above every
// soft floor, bounded by memory per slot, and refused under low disk or memory; the
// first slot is never refused here.
func TestVerificationSlotsGuarded(t *testing.T) {
	t.Parallel()
	f := DefaultFloors().Clamp()
	cases := []struct {
		name       string
		configured int
		r          Readings
		want       int
		reason     string
	}{
		{"plenty", 2, reading(50*GiB, 16*GiB), 2, ""},
		{"memory unknown", 3, Readings{Volumes: []Volume{{Path: "/lab", Free: 50 * GiB, Total: 100 * GiB}}}, 3, ""},
		{"low disk", 2, reading(10*GiB, 16*GiB), 1, "low disk"},
		{"low memory", 2, reading(50*GiB, 2*GiB), 1, "low memory"},
		{"memory per slot", 4, reading(50*GiB, 9*GiB), 2, "allows 2 verification slot(s)"},
		{"one configured", 1, reading(10*GiB, 2*GiB), 1, ""},
		{"over the maximum", 99, reading(50*GiB, 1024*GiB), MaxVerificationSlots, ""},
	}
	for _, c := range cases {
		got, reason := f.SlotsAllowed(c.configured, c.r)
		if got != c.want || (c.reason == "") != (reason == "") || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: %d %q, want %d %q", c.name, got, reason, c.want, c.reason)
		}
	}
	g := &Guard{Floors: f, Reader: ReaderFunc(func() Readings { return reading(10*GiB, 16*GiB) })}
	if n, reason := g.VerificationSlots(2); n != 1 || !strings.Contains(reason, "extra verification slots are refused") {
		t.Fatalf("guard: %d %q", n, reason)
	}
}

// TestConcurrentViewsGuarded: two views run at once only above every soft floor with
// memory for both; the context decision defaults to one after the other.
func TestConcurrentViewsGuarded(t *testing.T) {
	t.Parallel()
	f := DefaultFloors().Clamp()
	if ok, reason := f.ConcurrentViews(reading(50*GiB, 16*GiB)); !ok || reason != "" {
		t.Fatalf("plenty: %v %q", ok, reason)
	}
	for _, r := range []Readings{reading(10*GiB, 16*GiB), reading(50*GiB, 2*GiB), reading(50*GiB, 6*GiB)} {
		if ok, reason := f.ConcurrentViews(r); ok || !strings.Contains(reason, "one after the other") {
			t.Fatalf("%+v: %v %q", r, ok, reason)
		}
	}
	if ok, _ := ViewConcurrency(context.Background()); ok {
		t.Fatal("views run concurrently without a decision")
	}
	ctx := WithViewConcurrency(context.Background(), func() (bool, string) { return true, "" })
	if ok, _ := ViewConcurrency(ctx); !ok {
		t.Fatal("the carried decision was ignored")
	}
}

// TestLoadVerificationSlots: the default, a configured value, and refusals that name
// only the config's base name.
func TestLoadVerificationSlots(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	// A Lab without the setting keeps today's single slot.
	if n, set, err := LoadVerificationSlotsSetting(lab); err != nil || n != 1 || set {
		t.Fatalf("default: %d %v %v", n, set, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(lab, ConfigFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"diskSoftFloorGiB": 20}`)
	if n, set, err := LoadVerificationSlotsSetting(lab); err != nil || n != 1 || set {
		t.Fatalf("floors only: %d %v %v", n, set, err)
	}
	write(`{"verificationSlots": 3, "diskSoftFloorGiB": 20}`)
	if n, set, err := LoadVerificationSlotsSetting(lab); err != nil || n != 3 || !set {
		t.Fatalf("configured: %d %v %v", n, set, err)
	}
	if _, err := LoadFloors(lab); err != nil {
		t.Fatalf("the floors reader refused the slot field: %v", err)
	}
	for _, body := range []string{`{"verificationSlots": 0}`, `{"verificationSlots": 9}`, `{"verificationSlots": "2"}`, `{"slots": 2}`} {
		write(body)
		n, err := LoadVerificationSlots(lab)
		if err == nil || n != 1 || strings.Contains(err.Error(), lab) {
			t.Fatalf("%s: %d %v", body, n, err)
		}
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(lab, ConfigFile), filepath.Join(link, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if n, err := LoadVerificationSlots(link); err == nil || n != 1 {
		t.Fatalf("a symbolic link config was read: %d %v", n, err)
	}
}

// TestWriteInitialConfig: a new Lab gets RecommendedVerificationSlots; an existing
// config or a symbolic link in its place is never overwritten or followed.
func TestWriteInitialConfig(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	if err := WriteInitialConfig(lab); err != nil {
		t.Fatal(err)
	}
	if n, set, err := LoadVerificationSlotsSetting(lab); err != nil || n != 2 || !set {
		t.Fatalf("new Lab: %d %v %v", n, set, err)
	}
	if _, err := LoadFloors(lab); err != nil {
		t.Fatalf("the floors reader refused the initial config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lab, ConfigFile), []byte(`{"verificationSlots": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteInitialConfig(lab); err != nil {
		t.Fatal(err)
	}
	if n, _ := LoadVerificationSlots(lab); n != 1 {
		t.Fatalf("an existing config was overwritten: %d", n)
	}
	link, target := t.TempDir(), filepath.Join(t.TempDir(), "target.json")
	if err := os.Symlink(target, filepath.Join(link, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if err := WriteInitialConfig(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("a symbolic link config was followed: %v", err)
	}
	if err := WriteInitialConfig(""); err == nil {
		t.Fatal("an empty Lab directory was accepted")
	}
}

func TestDefaultFloorsAndDiskPercentages(t *testing.T) {
	f := DefaultFloors().Clamp()
	if soft, hard := f.DiskFloors(100 * GiB); soft != 15*GiB || hard != 5*GiB {
		t.Fatalf("100 GiB volume floors = %s / %s, want 15 / 5 GiB", Format(soft), Format(hard))
	}
	// 10% and 3% of a 200 GiB volume exceed the fixed floors and stay under the caps.
	soft, hard := f.DiskFloors(200 * GiB)
	if soft != 20*GiB || hard != 6*GiB {
		t.Fatalf("200 GiB volume floors = %s / %s, want 20 / 6 GiB", Format(soft), Format(hard))
	}
	// On a 460 GiB volume the percentages are capped at 30 and 10 GiB.
	if soft, hard := f.DiskFloors(460 * GiB); soft != 30*GiB || hard != 10*GiB {
		t.Fatalf("460 GiB volume floors = %s / %s, want 30 / 10 GiB", Format(soft), Format(hard))
	}
	// A 2 TB disk never demands more than the caps: 200 GB (10%) and 60 GB (3%) would
	// block an ordinary machine with plenty of room.
	twoTB := uint64(2_000_000_000_000)
	if soft, hard := f.DiskFloors(twoTB); soft != 30*GiB || hard != 10*GiB {
		t.Fatalf("2 TB volume floors = %s / %s, want 30 / 10 GiB", Format(soft), Format(hard))
	}
	twoTBVolume := Readings{Volumes: []Volume{{Path: "/big", Free: 40 * GiB, Total: twoTB}}, Memory: 8 * GiB, MemoryKnown: true}
	if level, reason := f.Evaluate(twoTBVolume); level != OK {
		t.Fatalf("40 GiB free on a 2 TB disk = %v %q, want OK under the capped floors", level, reason)
	}
	twoTBVolume.Volumes[0].Free = 20 * GiB
	if level, _ := f.Evaluate(twoTBVolume); level != BelowSoft {
		t.Fatalf("20 GiB free on a 2 TB disk = %v, want below the 30 GiB soft floor", level)
	}
	if f.MemorySoft != 3*GiB || f.MemoryHard != 3*GiB/2 {
		t.Fatalf("memory floors = %s / %s, want 3 / 1.5 GiB", Format(f.MemorySoft), Format(f.MemoryHard))
	}
}

func TestConfiguredFloorsNeverGoBelowMinimums(t *testing.T) {
	lab := t.TempDir()
	if err := os.WriteFile(filepath.Join(lab, ConfigFile), []byte(`{"diskSoftFloorGiB": 0.5, "diskHardFloorGiB": 0.1, "memorySoftFloorGiB": 0.25, "memoryHardFloorGiB": 0.1}`), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFloors(lab)
	if err != nil {
		t.Fatal(err)
	}
	if f.DiskSoft != MinDiskFloor || f.DiskHard != MinDiskFloor || f.MemorySoft != MinMemoryFloor || f.MemoryHard != MinMemoryFloor {
		t.Fatalf("floors = %+v, want every floor raised to its minimum", f)
	}
	if soft, _ := f.DiskFloors(1000 * GiB); soft != MinDiskFloor {
		t.Fatalf("a configured disk floor replaces the percentage, got soft %s", Format(soft))
	}
	if err := os.WriteFile(filepath.Join(lab, ConfigFile), []byte(`{"diskSoftFloorGiB": 40}`), 0600); err != nil {
		t.Fatal(err)
	}
	if f, err = LoadFloors(lab); err != nil {
		t.Fatal(err)
	}
	if soft, _ := f.DiskFloors(2000 * GiB); soft != 40*GiB {
		t.Fatalf("a configured disk floor replaces the percentage, got soft %s", Format(soft))
	}
	if err := os.WriteFile(filepath.Join(lab, ConfigFile), []byte(`{"diskSoftFloor": 1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFloors(lab); err == nil {
		t.Fatal("a misspelled floor must be an error, never a silently ignored setting")
	}
}

func TestEvaluateNamesTheResource(t *testing.T) {
	f := DefaultFloors().Clamp()
	if level, reason := f.Evaluate(reading(10*GiB, 8*GiB)); level != BelowSoft || !strings.HasPrefix(reason, "resources: low disk") {
		t.Fatalf("got %v %q", level, reason)
	}
	if level, reason := f.Evaluate(reading(50*GiB, 2*GiB)); level != BelowSoft || !strings.HasPrefix(reason, "resources: low memory") {
		t.Fatalf("got %v %q", level, reason)
	}
	if level, reason := f.Evaluate(reading(50*GiB, GiB)); level != BelowHard || !strings.HasPrefix(reason, "resources: low memory") {
		t.Fatalf("got %v %q", level, reason)
	}
	unknown := reading(50*GiB, 0)
	unknown.MemoryKnown = false
	if level, _ := f.Evaluate(unknown); level != OK {
		t.Fatalf("unknown memory must not trip a floor, got %v", level)
	}
}

func TestGuardRefusesBelowSoftFloorThenStartsOnceReadingsRecover(t *testing.T) {
	low, ok := reading(10*GiB, 8*GiB), reading(50*GiB, 8*GiB)
	reader := &scripted{readings: []Readings{low, low, low, ok}}
	var slept []time.Duration
	g := &Guard{Floors: DefaultFloors().Clamp(), Reader: reader, Sleep: func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}}
	if err := g.WaitForCapacity(context.Background()); err != nil {
		t.Fatalf("guard must start once readings recover: %v", err)
	}
	if reader.calls != 4 || len(slept) != 3 {
		t.Fatalf("checks = %d, waits = %d; want 4 checks and 3 waits", reader.calls, len(slept))
	}
	for _, d := range slept {
		if d != 30*time.Second {
			t.Fatalf("soft-floor re-check interval = %s, want 30s", d)
		}
	}
}

func TestGuardStopsWithReasonAfterTenMinutesBelowSoftFloor(t *testing.T) {
	reader := &scripted{readings: []Readings{reading(10*GiB, 8*GiB)}}
	waited := time.Duration(0)
	g := &Guard{Floors: DefaultFloors().Clamp(), Reader: reader, Sleep: func(_ context.Context, d time.Duration) bool {
		waited += d
		return true
	}}
	err := g.WaitForCapacity(context.Background())
	reason, low := AsLow(err)
	if !low || !strings.HasPrefix(reason, "resources: low disk") {
		t.Fatalf("got %v, want a resources: low disk refusal", err)
	}
	if waited != 10*time.Minute || reader.calls != 21 {
		t.Fatalf("waited %s over %d checks; want 10m over 21 checks", waited, reader.calls)
	}
}

func TestHardFloorCancelsInFlightProcessAndRecordsReason(t *testing.T) {
	reader := &scripted{readings: []Readings{reading(50*GiB, 8*GiB), reading(50*GiB, GiB)}}
	var intervals []time.Duration
	var mu sync.Mutex
	g := &Guard{Floors: DefaultFloors().Clamp(), Reader: reader, Sleep: func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		intervals = append(intervals, d)
		mu.Unlock()
		select {
		case <-time.After(50 * time.Millisecond):
			return true
		case <-ctx.Done():
			return false
		}
	}}
	ctx, stop := g.Monitor(context.Background())
	defer stop()
	started := time.Now()
	r := process.Run(ctx, process.Options{Argv: []string{"sleep", "30"}, Stdin: []byte{}, Timeout: time.Minute})
	if time.Since(started) > 20*time.Second {
		t.Fatal("the hard floor did not cancel the in-flight process")
	}
	if r.OK || !r.Interrupted {
		t.Fatalf("result = %+v, want an interrupted run", r)
	}
	reason, exhausted := Exhausted(ctx)
	if !exhausted || !strings.HasPrefix(reason, "resources-exhausted: low memory") {
		t.Fatalf("recorded reason %q, want resources-exhausted: low memory", reason)
	}
	if !strings.Contains(r.Error, "resources-exhausted") {
		t.Fatalf("process error %q does not name the cause", r.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(intervals) == 0 || intervals[0] != 10*time.Second {
		t.Fatalf("hard-floor sampling intervals %v, want 10s", intervals)
	}
}

func TestMonitorStopsCleanlyAboveFloors(t *testing.T) {
	g := &Guard{Floors: DefaultFloors().Clamp(), Reader: &scripted{readings: []Readings{reading(50*GiB, 8*GiB)}}}
	ctx, stop := g.Monitor(context.Background())
	stop()
	if _, exhausted := Exhausted(ctx); exhausted {
		t.Fatal("stopping a monitor above the floors must not record exhaustion")
	}
}

// TestPruneLabTempKeepsPerViewGoTempDirs: the named per-view GOTMPDIRs under gotmp are
// never listed, however old, since a running verification uses them; their own old
// children are pruned like gotmp's.
func TestPruneLabTempKeepsPerViewGoTempDirs(t *testing.T) {
	isolateSystemTemp(t)
	tmp, err := LabTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	gotmp := mkdir(t, filepath.Join(tmp, LabGoTempName))
	leaked := mkdir(t, filepath.Join(gotmp, "go-build123"))
	views := []string{}
	viewLeaks := []string{}
	for _, view := range []string{"candidate", "protected"} {
		dir := mkdir(t, filepath.Join(gotmp, view))
		leak := mkdir(t, filepath.Join(dir, "TestOld1"))
		recent := mkdir(t, filepath.Join(dir, "TestRunning2"))
		for _, p := range []string{leak, dir} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		views = append(views, dir, recent)
		viewLeaks = append(viewLeaks, leak)
	}
	for _, p := range []string{leaked, gotmp} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	want := append([]string{leaked}, viewLeaks...)
	stale, err := StaleLabTempEntries(tmp, now)
	if err != nil || !slices.Equal(stale, want) {
		t.Fatalf("stale %v %v, want %v", stale, err, want)
	}
	if removed, err := PruneLabTemp(tmp, now); err != nil || !slices.Equal(removed, want) {
		t.Fatalf("removed %v %v", removed, err)
	}
	for _, p := range append([]string{gotmp}, views...) {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be kept: %v", p, err)
		}
	}
	// A per-view name that is a symbolic link is neither listed nor followed.
	if err := os.RemoveAll(filepath.Join(gotmp, "protected")); err != nil {
		t.Fatal(err)
	}
	target := mkdir(t, filepath.Join(t.TempDir(), "elsewhere"))
	victim := mkdir(t, filepath.Join(target, "TestVictim"))
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(gotmp, "protected")); err != nil {
		t.Fatal(err)
	}
	if stale, err := StaleLabTempEntries(tmp, now); err != nil || len(stale) != 0 {
		t.Fatalf("through a linked view: %v %v", stale, err)
	}
}
