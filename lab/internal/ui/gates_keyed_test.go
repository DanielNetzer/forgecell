package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// keyedGateFixture is gateFixture's Molecule as a keyed one, waiting for scope
// approval of one exact plan, beside the numeric one.
func keyedGateFixture(t *testing.T) (string, molecule.Record, molecule.Record, string) {
	t.Helper()
	lab, numeric, digest := gateFixture(t)
	keyed := numeric
	keyed.ID = "mol-eng-7-abc"
	keyed.Issue.Key, keyed.Issue.Number, keyed.Issue.URL = "ENG-7", 0, "https://tracker.example/browse/ENG-7"
	writeRecord(t, lab, keyed)
	return lab, numeric, keyed, digest
}

// approvedRecord is r as the engine writes it once digest is approved.
func approvedRecord(r molecule.Record, digest string) molecule.Record {
	s := *r.Readiness
	s.Phase = "approved"
	s.Events = append(append([]readiness.Event{}, s.Events...), readiness.Event{Kind: "approved", Digest: digest, At: "2026-10-24T10:10:00Z"})
	r.Readiness = &s
	return r
}

func TestUIKeyedScopeApprovalResumesTheKeyedMoleculeAndBindsTheDigest(t *testing.T) {
	lab, numeric, keyed, digest := keyedGateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	engine := refuseEngine(t, h)
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`}
	runs := &keyedRuns{scope: func(_ context.Context, _ molecule.Options, _ string) error {
		return saveRecord(lab, approvedRecord(keyed, digest))
	}}
	path := "/api/molecules/" + keyed.ID
	scope := `{"digest":"` + digest + `"}`

	// A Lab whose Formula reads GitHub Issues never approves a keyed Molecule by
	// number: nothing starts.
	refusalOf(t, do(t, h, gatePost(path+"/scope", scope)), http.StatusConflict, "not-keyed")
	installKeyed(h, fake, runs)
	// Only the exact pending digest is approved, as in the CLI.
	refusalOf(t, do(t, h, gatePost(path+"/scope", `{"digest":"`+strings.Repeat("cd", 32)+`"}`)), http.StatusConflict, "stale")
	refusalOf(t, do(t, h, gatePost(path+"/scope", `{"digest":"`+digest+`","key":"ENG-7"}`)), http.StatusBadRequest, "invalid")
	if runs.calls() != 0 || engine.calls() != 0 {
		t.Fatalf("refused approvals reached the engine: %d keyed, %d numeric", runs.calls(), engine.calls())
	}

	v := jobOf(t, do(t, h, gatePost(path+"/scope", scope)), http.StatusAccepted)
	if v.Kind != "scope" || v.Digest != digest || v.State != "running" {
		t.Fatalf("accepted keyed approval %+v", v)
	}
	waitJob(t, h, keyed.ID)
	if job := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); job.State != "finished" || !job.Committed || !job.Canonical.Approved {
		t.Fatalf("keyed approval read-back %+v", job)
	}
	if runs.calls() != 1 || engine.calls() != 0 {
		t.Fatalf("keyed approval ran %d keyed and %d numeric engine calls", runs.calls(), engine.calls())
	}
	o, key := runs.scopes[0], runs.scoped[0]
	plan := keyed.Readiness.Plans[0].Plan
	if key != "ENG-7" || o.Approve != digest || !o.AllowKeyedApproval || o.ReadIssueKey == nil || o.PluginCalls == nil || o.Issue != "" || o.LabDir != lab || o.FormulaID != keyed.FormulaID || o.Base != plan.Inputs.BaseCommit || o.TargetBranch != plan.Inputs.TargetBranch {
		t.Fatalf("keyed approval options %+v key %q", o, key)
	}
	// Approving reads no keyPattern: the plugin is only prepared for the reader.
	if describes, _, prepared := fake.counts(); describes != 0 || prepared != 1 {
		t.Fatalf("approval described %d times, prepared %d trackers", describes, prepared)
	}

	// The numeric Molecule is still approved through molecule.Run by number.
	engine.mu.Lock()
	engine.scope = func(context.Context, molecule.Options) error { return nil }
	engine.mu.Unlock()
	jobOf(t, do(t, h, gatePost("/api/molecules/"+numeric.ID+"/scope", scope)), http.StatusAccepted)
	waitJob(t, h, numeric.ID)
	if engine.calls() != 1 || runs.calls() != 1 {
		t.Fatalf("numeric approval ran %d numeric and %d keyed engine calls", engine.calls(), runs.calls())
	}
	if o := engine.scopes[0]; o.Issue != "o/r#7" || o.Approve != digest || o.AllowKeyedApproval || o.ReadIssueKey != nil {
		t.Fatalf("numeric approval options %+v", o)
	}
}

func TestUIKeyedScopeApprovalRefusalsFailSafe(t *testing.T) {
	lab, _, keyed, digest := keyedGateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	engine := refuseEngine(t, h)
	runs := &keyedRuns{scope: func(context.Context, molecule.Options, string) error {
		return errors.New("tracker_get failed in /Users/someone/lab with ghp_secret")
	}}
	installKeyed(h, &keyedFake{pattern: `^ENG-[0-9]+$`}, runs)
	scope := `{"digest":"` + digest + `"}`

	// A ledger whose key is not a valid Issue key is never approved.
	hostile := keyed
	hostile.ID, hostile.Issue.Key = "mol-eng-8-bad", "ENG‮8"
	writeRecord(t, lab, hostile)
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+hostile.ID+"/scope", scope)), http.StatusUnprocessableEntity, "refused")

	// A tracker that cannot be prepared refuses with a fixed reason.
	hh := h.(*handler)
	trackerFor := hh.trackerFor
	hh.trackerFor = func(string, formula.Formula, mcpadapter.Options) (tracker.Keyed, bool, error) {
		return nil, true, errors.New("run root in /Users/someone/lab is not private")
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+keyed.ID+"/scope", scope)), http.StatusConflict, "tracker")
	if runs.calls() != 0 || engine.calls() != 0 {
		t.Fatalf("refused approvals reached the engine: %d keyed, %d numeric", runs.calls(), engine.calls())
	}

	// An engine refusal is served by category only, and never as committed.
	hh.trackerFor = trackerFor
	jobOf(t, do(t, h, gatePost("/api/molecules/"+keyed.ID+"/scope", scope)), http.StatusAccepted)
	waitJob(t, h, keyed.ID)
	w := do(t, h, authed("/api/molecules/"+keyed.ID+"/job"))
	if job := jobOf(t, w, http.StatusOK); job.State != "refused" || job.Committed || job.Canonical.Approved || job.Reason != categoryReasons["refused"] {
		t.Fatalf("refused keyed approval %+v", job)
	}
	noLeak(t, w.Body.String())
}
