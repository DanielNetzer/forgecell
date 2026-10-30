package delivery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const ciCharacterizationPassed = `{"headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`

type ciCharacterizationRecorder struct {
	calls  []process.Options
	result process.Result
}

func (r *ciCharacterizationRecorder) run(_ context.Context, o process.Options) process.Result {
	r.calls = append(r.calls, o)
	return r.result
}

func ciCharacterizationTarget(rec *ciCharacterizationRecorder) checks.Options {
	return checks.Options{Repo: "owner/repo", PR: 9, Commit: strings.Repeat("a", 40), Required: []string{"test"}, Runner: rec.run}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectCIArgvAndObservation(t *testing.T) {
	o, _ := ciFixture(t)
	rec := &ciCharacterizationRecorder{result: process.Result{OK: true, Stdout: ciCharacterizationPassed}}
	a, err := CollectCI(context.Background(), o, ciCharacterizationTarget(rec))
	if err != nil {
		t.Fatalf("CollectCI: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d", len(rec.calls))
	}
	wantArgv := []string{"gh", "pr", "view", "9", "--repo", "owner/repo", "--json", "headRefOid,statusCheckRollup"}
	if c := rec.calls[0]; !reflect.DeepEqual(c.Argv, wantArgv) || c.Timeout != 30*time.Second || c.MaxOutputBytes != 1_000_000 {
		t.Fatalf("call = %+v", c)
	}
	preview := PreviewResult{MoleculeID: "mol-ci", Repo: "owner/repo"}
	wantID := CIIdentity{MoleculeID: "mol-ci", PreviewDigest: digest(preview), Repo: "owner/repo", PR: 9, URL: "https://github.com/owner/repo/pull/9", Commit: strings.Repeat("a", 40)}
	if a.SchemaVersion != "v1" || a.Sequence != 1 || a.Previous != "" || a.Identity != wantID || a.SHA256 != ciHash(a) || len(a.SHA256) != 64 {
		t.Fatalf("observation = %+v", a)
	}
	if a.Snapshot.Status != "passed" || a.Snapshot.ObservedHead != wantID.Commit || !reflect.DeepEqual(a.Snapshot.Checks, []checks.Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}) {
		t.Fatalf("snapshot = %+v", a.Snapshot)
	}
	file := filepath.Join(o.LabDir, "deliveries", "mol-ci", "ci", fmt.Sprintf("%06d-%s.json", 1, a.SHA256))
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("observation file %v", err)
	}
	if _, err := os.Lstat(filepath.Join(o.LabDir, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatalf("lock kept: %v", err)
	}
	history, err := ReadCI(o)
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], a) {
		t.Fatalf("history = %+v %v", history, err)
	}
	if got := CIClassification(history); got != "passed" {
		t.Fatalf("classification = %q", got)
	}

	rec.result = process.Result{OK: true, Stdout: strings.Replace(ciCharacterizationPassed, `"SUCCESS"`, `"FAILURE"`, 1)}
	second, err := CollectCI(context.Background(), o, ciCharacterizationTarget(rec))
	if err != nil || second.Sequence != 2 || second.Previous != a.SHA256 || second.Snapshot.Status != "failed" {
		t.Fatalf("second = %+v %v", second, err)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectCIClassification(t *testing.T) {
	cases := []struct {
		name   string
		result process.Result
		status string
		class  string
	}{
		{"passed", process.Result{OK: true, Stdout: ciCharacterizationPassed}, "passed", "passed"},
		{"failed", process.Result{OK: true, Stdout: strings.Replace(ciCharacterizationPassed, `"SUCCESS"`, `"FAILURE"`, 1)}, "failed", "failure"},
		{"pending", process.Result{OK: true, Stdout: strings.Replace(ciCharacterizationPassed, `"COMPLETED"`, `"IN_PROGRESS"`, 1)}, "pending", "unknown"},
		{"missing", process.Result{OK: true, Stdout: strings.Replace(ciCharacterizationPassed, `"name":"test"`, `"name":"other"`, 1)}, "missing", "unknown"},
		{"stale", process.Result{OK: true, Stdout: strings.Replace(ciCharacterizationPassed, strings.Repeat("a", 40), strings.Repeat("b", 40), 1)}, "stale", "unknown"},
		{"non-zero exit", process.Result{Code: 1, Error: "exit status 1", Stderr: "not authenticated"}, "unknown", "unknown"},
		{"malformed json", process.Result{OK: true, Stdout: "{"}, "unknown", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := ciFixture(t)
			rec := &ciCharacterizationRecorder{result: c.result}
			a, err := CollectCI(context.Background(), o, ciCharacterizationTarget(rec))
			if err != nil || a.Sequence != 1 || a.Snapshot.Status != c.status || len(rec.calls) != 1 {
				t.Fatalf("observation = %+v %v calls = %d", a, err, len(rec.calls))
			}
			history, err := ReadCI(o)
			if err != nil {
				t.Fatal(err)
			}
			if got := CIClassification(history); got != c.class {
				t.Fatalf("classification = %q, want %q", got, c.class)
			}
		})
	}
	at := func(status, checkedAt string) CIObservation {
		return CIObservation{Snapshot: checks.Snapshot{Status: status, CheckedAt: checkedAt}}
	}
	ordered := map[string]struct {
		history []CIObservation
		want    string
	}{
		"empty":            {nil, "unknown"},
		"bad time":         {[]CIObservation{at("passed", "yesterday")}, "unknown"},
		"latest wins":      {[]CIObservation{at("failed", "2026-10-30T10:00:00Z"), at("passed", "2026-10-30T10:00:01Z")}, "passed"},
		"clock regression": {[]CIObservation{at("failed", "2026-10-30T10:00:01Z"), at("passed", "2026-10-30T10:00:00Z")}, "unknown"},
	}
	for name, c := range ordered {
		if got := CIClassification(c.history); got != c.want {
			t.Fatalf("%s: classification = %q, want %q", name, got, c.want)
		}
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationCollectCIRefusesBeforeLookup(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(o *Options, target *checks.Options)
		err    string
	}{
		{"other PR", func(_ *Options, target *checks.Options) { target.PR = 10 }, "CI target differs from delivery"},
		{"other commit", func(_ *Options, target *checks.Options) { target.Commit = strings.Repeat("b", 40) }, "CI target differs from delivery"},
		{"other repo", func(_ *Options, target *checks.Options) { target.Repo = "other/repo" }, "CI target differs from delivery"},
		{"invalid molecule id", func(o *Options, _ *checks.Options) { o.MoleculeID = "../mol" }, "invalid Molecule id"},
		{"lock held", func(o *Options, _ *checks.Options) {
			if err := os.Mkdir(filepath.Join(o.LabDir, ".delivery-lock"), 0700); err != nil {
				t.Fatal(err)
			}
		}, "delivery/CI collection conflict: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _ := ciFixture(t)
			rec := &ciCharacterizationRecorder{result: process.Result{OK: true, Stdout: ciCharacterizationPassed}}
			target := ciCharacterizationTarget(rec)
			c.mutate(&o, &target)
			_, err := CollectCI(context.Background(), o, target)
			if err == nil || !strings.HasPrefix(err.Error(), c.err) || len(rec.calls) != 0 {
				t.Fatalf("err = %v calls = %d", err, len(rec.calls))
			}
			o.MoleculeID = "mol-ci"
			if history, err := ReadCI(o); err != nil || len(history) != 0 {
				t.Fatalf("history = %+v %v", history, err)
			}
		})
	}
}
