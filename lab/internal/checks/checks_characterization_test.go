package checks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const characterizationHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type characterizationRecorder struct {
	calls  []process.Options
	result process.Result
}

func (r *characterizationRecorder) run(_ context.Context, o process.Options) process.Result {
	r.calls = append(r.calls, o)
	return r.result
}

func characterizationFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "characterization", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func characterizationCollect(t *testing.T, stdout string, required ...string) (Snapshot, *characterizationRecorder) {
	t.Helper()
	rec := &characterizationRecorder{result: process.Result{OK: true, Stdout: stdout}}
	snap, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 42, Commit: characterizationHead, Required: required, Dir: "work", Runner: rec.run})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap, rec
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectArgvAndPassedRollup(t *testing.T) {
	snap, rec := characterizationCollect(t, characterizationFixture(t, "rollup-passed.json"), "go-core", "ci/legacy", "go-core")
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}
	call := rec.calls[0]
	wantArgv := []string{"gh", "pr", "view", "42", "--repo", "owner/repo", "--json", "headRefOid,statusCheckRollup"}
	if !reflect.DeepEqual(call.Argv, wantArgv) {
		t.Fatalf("argv = %q, want %q", call.Argv, wantArgv)
	}
	if call.Dir != "work" || call.Timeout != 30*time.Second || call.MaxOutputBytes != 1_000_000 || call.Stdin == nil || len(call.Stdin) != 0 || call.Input != nil {
		t.Fatalf("options = %+v", call)
	}
	want := []Check{
		{Name: "go-core", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://github.com/owner/repo/actions/runs/1"},
		{Name: "ci/legacy", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://ci.example/build/1"},
	}
	if snap.Status != "passed" || snap.Detail != "" || snap.ObservedHead != characterizationHead || snap.Repo != "owner/repo" || snap.PR != 42 || snap.Commit != characterizationHead {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !reflect.DeepEqual(snap.Checks, want) || !reflect.DeepEqual(snap.Required, []string{"go-core", "ci/legacy"}) {
		t.Fatalf("checks = %+v required = %q", snap.Checks, snap.Required)
	}
	if _, err := time.Parse(time.RFC3339Nano, snap.CheckedAt); err != nil {
		t.Fatalf("checkedAt %q: %v", snap.CheckedAt, err)
	}
	if err := Validate(snap); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectAggregates(t *testing.T) {
	failed, _ := characterizationCollect(t, characterizationFixture(t, "rollup-failed.json"), "go-core")
	if failed.Status != "failed" || failed.Checks[0].Conclusion != "FAILURE" || failed.Checks[1].Status != "COMPLETED" || failed.Checks[1].Conclusion != "SUCCESS" {
		t.Fatalf("failed = %+v", failed)
	}
	entry := func(head, entries string) string {
		return fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, head, entries)
	}
	pending, _ := characterizationCollect(t, entry(characterizationHead, `{"__typename":"StatusContext","context":"ci/legacy","state":"PENDING","targetUrl":"u"}`), "ci/legacy")
	if pending.Status != "pending" || !reflect.DeepEqual(pending.Checks, []Check{{Name: "ci/legacy", Status: "IN_PROGRESS", Conclusion: "PENDING", URL: "u"}}) {
		t.Fatalf("pending = %+v", pending)
	}
	expected, _ := characterizationCollect(t, entry(characterizationHead, `{"__typename":"StatusContext","context":"ci/legacy","state":"EXPECTED"}`), "ci/legacy")
	if expected.Status != "pending" || expected.Checks[0].Status != "IN_PROGRESS" {
		t.Fatalf("expected = %+v", expected)
	}
	missing, _ := characterizationCollect(t, characterizationFixture(t, "rollup-passed.json"), "go-core", "absent")
	if missing.Status != "missing" {
		t.Fatalf("missing = %+v", missing)
	}
	stale, _ := characterizationCollect(t, strings.Replace(characterizationFixture(t, "rollup-failed.json"), characterizationHead, strings.Repeat("b", 40), 1), "absent")
	if stale.Status != "stale" || stale.ObservedHead != strings.Repeat("b", 40) || len(stale.Checks) != 2 {
		t.Fatalf("stale = %+v", stale)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectBoundsAndFailures(t *testing.T) {
	entry := func(entries string) string {
		return fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, characterizationHead, entries)
	}
	run := func(n int, url string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"__typename":"CheckRun","name":"c%d","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":%q}`, i, url)
		}
		return entry(strings.Join(parts, ","))
	}
	cases := []struct {
		name, stdout, detail string
		result               *process.Result
	}{
		{"non-zero exit", "", "GitHub check lookup failed. No passing evidence recorded.", &process.Result{Code: 1, Stderr: "gh: not found"}},
		{"overflow", "", "GitHub check lookup failed. No passing evidence recorded.", &process.Result{OK: true, Overflow: true}},
		{"timed out", "", "GitHub check lookup failed. No passing evidence recorded.", &process.Result{OK: true, TimedOut: true}},
		{"malformed json", `{"headRefOid":`, "Invalid GitHub check response.", nil},
		{"bad head", `{"headRefOid":"HEAD","statusCheckRollup":[]}`, "Invalid GitHub check response.", nil},
		{"null rollup", fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":null}`, characterizationHead), "Check data unavailable.", nil},
		{"too many checks", run(MaxChecks+1, ""), "Check count exceeds bound.", nil},
		{"oversized name", entry(fmt.Sprintf(`{"__typename":"CheckRun","name":%q,"status":"COMPLETED","conclusion":"SUCCESS"}`, strings.Repeat("n", 257))), "Check fields missing or exceed bounds.", nil},
		{"oversized url", entry(fmt.Sprintf(`{"__typename":"CheckRun","name":"c","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":%q}`, strings.Repeat("u", 2049))), "Check fields missing or exceed bounds.", nil},
		{"unrecognized check status", entry(`{"__typename":"CheckRun","name":"c","status":"BROKEN","conclusion":"SUCCESS"}`), "Unrecognized check status.", nil},
		{"unrecognized commit status", entry(`{"__typename":"StatusContext","context":"c","state":"WEIRD"}`), "Unrecognized commit status.", nil},
		{"unrecognized typename", entry(`{"__typename":"Other","context":"c","targetUrl":"t"}`), "Unrecognized check data.", nil},
		{"snapshot exceeds bound", run(MaxChecks, strings.Repeat("u", 600)), "Snapshot exceeds bound.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &characterizationRecorder{result: process.Result{OK: true, Stdout: c.stdout}}
			if c.result != nil {
				rec.result = *c.result
			}
			snap, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 42, Commit: characterizationHead, Required: []string{"c"}, Runner: rec.run})
			if err != nil || snap.Status != "unknown" || snap.Detail != c.detail || len(rec.calls) != 1 {
				t.Fatalf("snapshot = %+v err = %v calls = %d", snap, err, len(rec.calls))
			}
			if c.name == "snapshot exceeds bound" && len(snap.Checks) != 0 {
				t.Fatalf("oversized snapshot kept %d checks", len(snap.Checks))
			}
			if err := Validate(snap); err != nil {
				t.Fatalf("unknown snapshot must persist: %v", err)
			}
		})
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectRejectsInvalidInputsWithoutRunning(t *testing.T) {
	valid := Options{Repo: "owner/repo", PR: 1, Commit: characterizationHead, Required: []string{"c"}}
	cases := map[string]func(*Options){
		"bad repo":       func(o *Options) { o.Repo = "owner" },
		"dot repo":       func(o *Options) { o.Repo = "owner/.." },
		"zero pr":        func(o *Options) { o.PR = 0 },
		"short commit":   func(o *Options) { o.Commit = "abc" },
		"no required":    func(o *Options) { o.Required = nil },
		"blank required": func(o *Options) { o.Required = []string{" "} },
		"many required":  func(o *Options) { o.Required = make([]string, 129) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &characterizationRecorder{}
			o := valid
			o.Runner = rec.run
			mutate(&o)
			snap, err := Collect(context.Background(), o)
			if err == nil || snap.Status != "unknown" || len(rec.calls) != 0 {
				t.Fatalf("snapshot = %+v err = %v calls = %d", snap, err, len(rec.calls))
			}
		})
	}
}
