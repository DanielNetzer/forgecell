package molecule

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// Evidence files are append-only. The ledger carries their hashes and lifecycle;
// large captured outputs are not duplicated into every subsequent plan revision.
type VerificationFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type VerificationAttempt struct {
	PlanDigest   string             `json:"planDigest"`
	SourceTree   string             `json:"sourceTree"`
	StartedAt    string             `json:"startedAt"`
	FinishedAt   string             `json:"finishedAt,omitempty"`
	Status       string             `json:"status"`
	Observations []VerificationFile `json:"observations,omitempty"`
	Result       *VerificationFile  `json:"result,omitempty"`
}

func statePhase(r Record) string {
	if r.Readiness == nil {
		return ""
	}
	return r.Readiness.Phase
}
func completedCoding(r Record) bool {
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		return false
	}
	coding := r.Atoms[roles.atoms[formula.Coding]]
	if r.Readiness == nil || coding.Status != "done" || r.Capture == nil || len(r.Capture.Violations) > 0 || (coding.ExitCode != nil && *coding.ExitCode != 0) {
		return false
	}
	if len(r.HarnessAttempts) == 0 || ValidateVerificationHistory(r) != nil {
		return false
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if a.Coding == nil || a.Evidence == nil || !a.Result.OK || !a.Result.Reconciled || a.Capture == nil || a.CaptureError != "" || a.Capture.Tree != r.Capture.Tree {
		return false
	}
	allowed := false
	for _, proposal := range r.Readiness.Plans {
		if proposal.Digest == a.PlanDigest && proposal.Plan.Continuation == "" {
			allowed = (a.Coding.Outcome == "completed" && len(a.Capture.Changes) > 0) || (a.Coding.Outcome == "no-change" && proposal.Plan.AllowNoChange && len(a.Capture.Changes) == 0)
		}
	}
	if !allowed {
		return false
	}
	// A later successful attempt cannot erase an unresolved violating write.
	// Retain the same exact-diff review boundary as final delivery, including
	// historical ledgers whose violation event did not yet store a tree.
	for _, e := range r.Readiness.Events {
		if e.Kind != "attempt-scope-violation" {
			continue
		}
		reviewed := false
		for _, decision := range r.Readiness.Events {
			if decision.Kind != "existing-diff-reviewed" || decision.Digest != e.Digest || decision.Tree == "" || (e.Tree != "" && decision.Tree != e.Tree) {
				continue
			}
			for _, proposal := range r.Readiness.Plans {
				if proposal.Plan.ParentDigest == e.Digest && proposal.Plan.PausedTree == decision.Tree {
					reviewed = true
				}
			}
		}
		if !reviewed {
			return false
		}
	}
	// Require an ordered, exact approval and coding start for the retained
	// completion. A done atom or completion event alone is not authorization.
	stages := map[string]int{}
	for _, proposal := range r.Readiness.Plans {
		if proposal.Plan.Continuation == "" {
			stages[proposal.Digest] = 0
		}
	}
	for _, e := range r.Readiness.Events {
		if _, knownCodingPlan := stages[e.Digest]; !knownCodingPlan {
			continue
		}
		switch e.Kind {
		case "approved":
			stages[e.Digest] = 1
		case "attempt-started":
			if stages[e.Digest] == 1 {
				stages[e.Digest] = 2
			} else {
				stages[e.Digest] = 0
			}
		case "attempt-check-pending":
			if stages[e.Digest] == 2 {
				stages[e.Digest] = 3
			} else {
				stages[e.Digest] = 0
			}
		case "attempt-failed", "attempt-blocked", "attempt-unknown", "attempt-scope-change", "attempt-scope-violation":
			stages[e.Digest] = 0
		}
	}
	// A coding interruption or failed coding attempt cannot be promoted by checks.
	for i := len(r.Readiness.Events) - 1; i >= 0; i-- {
		e := r.Readiness.Events[i]
		switch e.Kind {
		case "attempt-check-pending":
			return e.Digest == a.PlanDigest && stages[e.Digest] == 3
		case "attempt-started", "attempt-failed", "attempt-blocked", "attempt-unknown", "attempt-scope-change", "attempt-scope-violation":
			return false
		}
	}
	return false
}
func historyFile(r Record, name string, value any) (VerificationFile, error) {
	var ref VerificationFile
	raw, err := json.Marshal(value)
	if err != nil {
		return ref, err
	}
	if len(raw) > 64_000_000 {
		return ref, fmt.Errorf("verification evidence exceeds 64 MB limit")
	}
	rel := filepath.Join("verification-history", r.ID, name)
	file := filepath.Join(r.LabDir, rel)
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return ref, err
	}
	// Exclusive creation never replaces prior evidence, including crash leftovers.
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ref, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return ref, err
	}
	d, err := os.Open(filepath.Dir(file))
	if err != nil {
		return ref, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return ref, err
	}
	return VerificationFile{Path: filepath.ToSlash(rel), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}, nil
}
func beginVerification(r *Record, p readiness.Plan, tree string) error {
	// Bound storage before starting commands (each process captures at most 256 KB).
	if len(r.VerificationAttempts) >= 100 || len(p.Setup)+len(p.Analysis.Checks) > 16 {
		return fmt.Errorf("verification history capacity reached; no commands started")
	}
	digest, err := p.Digest()
	if err != nil {
		return err
	}
	r.VerificationAttempts = append(r.VerificationAttempts, VerificationAttempt{PlanDigest: digest, SourceTree: tree, StartedAt: stamp(), Status: "started"})
	return nil
}
func recordObservation(r *Record, obs verification.Observation) error {
	i := len(r.VerificationAttempts) - 1
	a := &r.VerificationAttempts[i]
	if len(a.Observations) >= 32 {
		return fmt.Errorf("verification observation limit reached")
	}
	ref, err := historyFile(*r, fmt.Sprintf("%03d-observation-%03d.json", i+1, len(a.Observations)+1), obs)
	if err != nil {
		return err
	}
	a.Observations = append(a.Observations, ref)
	return save(*r)
}
func completeVerification(r *Record, result verification.Result) error {
	i := len(r.VerificationAttempts) - 1
	ref, err := historyFile(*r, fmt.Sprintf("%03d-result.json", i+1), result)
	if err != nil {
		return err
	}
	a := &r.VerificationAttempts[i]
	a.Result = &ref
	a.FinishedAt = stamp()
	a.Status = "failed"
	if result.RequiredChecksPassed {
		a.Status = "passed"
	}
	return nil
}
func archiveLegacyVerification(r *Record) error {
	if r.Verification == nil {
		return nil
	}
	if len(r.VerificationAttempts) > 0 {
		return nil
	}
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	if err := beginVerification(r, p, r.Verification.SourceTree); err != nil {
		return err
	}
	r.VerificationAttempts[0].StartedAt = "" // Historical start time is unknown.
	return completeVerification(r, *r.Verification)
}

// The current projection retains commands, outcomes and artifact identities.
// Captured streams remain complete in immutable history files, not duplicated here.
func compactVerification(v verification.Result) verification.Result {
	raw, _ := json.Marshal(v)
	var out verification.Result
	json.Unmarshal(raw, &out)
	var compact func(*verification.Result)
	compact = func(r *verification.Result) {
		for i := range r.Setup {
			r.Setup[i].Result.Stdout = ""
			r.Setup[i].Result.Stderr = ""
			r.Setup[i].Result.RawStdout = ""
		}
		for i := range r.Checks {
			r.Checks[i].Result.Stdout = ""
			r.Checks[i].Result.Stderr = ""
			r.Checks[i].Result.RawStdout = ""
		}
		if r.Protected != nil {
			compact(r.Protected)
		}
	}
	compact(&out)
	return out
}

func ValidateVerificationHistory(r Record) error {
	if len(r.VerificationAttempts) > 100 || len(r.HarnessAttempts) > 100 {
		return fmt.Errorf("verification history limit exceeded")
	}
	outcomes := map[string]HarnessAttempt{}
	captures := map[string]HarnessAttempt{}
	attempts := append([]VerificationAttempt{}, r.VerificationAttempts...)
	for _, a := range r.HarnessAttempts {
		if a.Evidence != nil {
			expected := filepath.ToSlash(filepath.Join("verification-history", r.ID, "harness-"+a.ID+"-result.json"))
			if a.Evidence.Path != expected {
				return fmt.Errorf("harness outcome does not match attempt identity")
			}
			outcomes[a.Evidence.Path] = a
			if len(a.CaptureEvidence) > 0 {
				captures[a.CaptureEvidence[len(a.CaptureEvidence)-1].Path] = a
			}
			attempts = append(attempts, VerificationAttempt{Result: a.Evidence, Observations: a.CaptureEvidence})
		}
	}
	for _, a := range attempts {
		refs := append([]VerificationFile{}, a.Observations...)
		if a.Result != nil {
			refs = append(refs, *a.Result)
		}
		for _, ref := range refs {
			prefix := filepath.ToSlash(filepath.Join("verification-history", r.ID)) + "/"
			if !strings.HasPrefix(ref.Path, prefix) || filepath.ToSlash(filepath.Clean(ref.Path)) != ref.Path || strings.Contains(ref.Path, "..") {
				return fmt.Errorf("invalid evidence path")
			}
			file := filepath.Join(r.LabDir, filepath.FromSlash(ref.Path))
			st, err := os.Lstat(file)
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() || st.Size() > 64_000_000 {
				return fmt.Errorf("invalid verification evidence file")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.SHA256 {
				return fmt.Errorf("verification history changed: %s", ref.Path)
			}
			if a, ok := captures[ref.Path]; ok {
				var saved struct {
					verification.CaptureResult
					CaptureError string `json:"captureError,omitempty"`
				}
				if json.Unmarshal(raw, &saved) != nil || a.Capture == nil || !reflect.DeepEqual(saved.CaptureResult, *a.Capture) || saved.CaptureError != a.CaptureError {
					return fmt.Errorf("harness capture projection changed")
				}
			}
			if a, ok := outcomes[ref.Path]; ok {
				var saved struct {
					process.Result
					AttemptID  string `json:"attemptId"`
					PlanDigest string `json:"planDigest"`
				}
				if json.Unmarshal(raw, &saved) != nil || saved.AttemptID != a.ID || saved.PlanDigest != a.PlanDigest {
					return fmt.Errorf("harness outcome identity changed")
				}
				if a.Coding != nil && !reflect.DeepEqual(*a.Coding, harness.CodingOutcomeFromResult(saved.Result.Stdout)) {
					return fmt.Errorf("coding outcome projection changed")
				}
				saved.Result.Stdout, saved.Result.Stderr, saved.Result.RawStdout = "", "", ""
				if saved.Result != a.Result {
					return fmt.Errorf("harness outcome projection changed")
				}
			}

		}
	}
	return nil
}

// Learning receives complete selected attempt results within a bounded request,
// including interrupted attempts' completed observations. Nothing is truncated.
func LearningVerificationEvidence(r Record, remaining int) ([]json.RawMessage, int, error) {
	if err := ValidateVerificationHistory(r); err != nil {
		return nil, remaining, err
	}
	var values []json.RawMessage
	for _, a := range r.VerificationAttempts {
		refs := a.Observations
		if a.Result != nil {
			refs = []VerificationFile{*a.Result}
		}
		for _, ref := range refs {
			path := filepath.Join(r.LabDir, ref.Path)
			info, err := os.Stat(path)
			if err != nil {
				return nil, remaining, err
			}
			if info.Size() > int64(remaining) {
				return nil, remaining, fmt.Errorf("learning verification evidence exceeds 4 MB budget; select fewer Molecules")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, remaining, err
			}
			remaining -= len(raw)
			if remaining < 0 || fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.SHA256 || !json.Valid(raw) {
				return nil, remaining, fmt.Errorf("verification evidence changed while loading learning inputs")
			}
			values = append(values, json.RawMessage(raw))
		}
	}
	return values, remaining, nil
}

// failedTreeEligible keeps failed coding distinct from completedCoding. Missing
// durable outcome evidence (including legacy interrupted ledgers) fails closed.
func failedTreeEligible(r Record, attempt string) bool {
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		return false
	}
	coding := r.Atoms[roles.atoms[formula.Coding]]
	if r.Readiness == nil || len(r.HarnessAttempts) == 0 || coding.Status != "failed" {
		return false
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if a.ID != attempt || a.Evidence == nil || (a.Result.OK && (a.Coding == nil || (a.Coding.Outcome != "blocked" && a.Coding.Outcome != "unknown" && a.Coding.Outcome != "scope-change"))) || !a.Result.Reconciled || !a.Recovered || a.CaptureError != "" || a.Capture == nil || a.Capture.Tree == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(r.LabDir, a.Evidence.Path))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != a.Evidence.SHA256 {
		return false
	}
	var result struct {
		OK         bool   `json:"ok"`
		Reconciled bool   `json:"reconciled"`
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}
	if json.Unmarshal(raw, &result) != nil || result.OK != a.Result.OK || !result.Reconciled || result.AttemptID != a.ID || result.PlanDigest != a.PlanDigest {
		return false
	}
	if len(a.CaptureEvidence) == 0 {
		return false
	}
	captureRef := a.CaptureEvidence[len(a.CaptureEvidence)-1]
	raw, err = os.ReadFile(filepath.Join(r.LabDir, captureRef.Path))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != captureRef.SHA256 {
		return false
	}
	var captured verification.CaptureResult
	if json.Unmarshal(raw, &captured) != nil || captured.Tree != a.Capture.Tree || artifactHash(captured) != artifactHash(*a.Capture) {
		return false
	}
	stage := 0
	for _, e := range r.Readiness.Events {
		if e.Digest != a.PlanDigest {
			continue
		}
		switch e.Kind {
		case "approved":
			stage = 1
		case "attempt-started":
			if stage == 1 {
				stage = 2
			} else {
				return false
			}
		case "attempt-failed", "attempt-blocked", "attempt-unknown", "attempt-scope-change":
			if stage == 2 {
				stage = 3
			} else {
				return false
			}
		case "interrupted":
			if stage == 3 && e.Tree == a.Capture.Tree {
				stage = 4
			}
		}
	}
	return stage == 4
}

// FailedTreeAdopted is used by verification and delivery. Approval must bind the
// latest reconciled failed attempt, its exact tree and retained artifact bytes.
func FailedTreeAdopted(r Record) bool {
	if ValidateVerificationHistory(r) != nil {
		return false
	}
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 || r.Capture == nil {
		return false
	}
	proposal := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	p := proposal.Plan
	if p.Continuation != "adopt-failed-tree" || !failedTreeEligible(r, p.AdoptAttempt) {
		return false
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if p.PausedTree != a.Capture.Tree || r.Capture.Tree != p.PausedTree || len(r.Capture.Violations) > 0 || artifactHash(*r.Capture) != p.PausedArtifactsSHA256 || artifactHash(*a.Capture) != p.PausedArtifactsSHA256 {
		return false
	}
	reviewed, approved := false, false
	for _, e := range r.Readiness.Events {
		if e.Digest != proposal.Digest {
			continue
		}
		switch e.Kind {
		case "failed-tree-adopted":
			reviewed = e.Tree == p.PausedTree && e.Detail == p.AdoptAttempt
		case "approved":
			approved = reviewed
		case "checks-only-started":
			return approved
		case "attempt-started":
			return false
		}
	}
	return false
}

// A completed coding process says nothing about subsequent setup or checks.
// Started or missing results cannot prove that descendants have stopped. Require
// durable results for every retained verification attempt, including protected
// regressions; human confirmation cannot substitute for missing process evidence.
func confirmVerificationProcessesStopped(r Record) error {
	var stopped func(verification.Result) error
	stopped = func(v verification.Result) error {
		for _, obs := range append(append([]verification.Observation{}, v.Setup...), v.Checks...) {
			if !obs.Result.Reconciled {
				return fmt.Errorf("verification command %s lacks stopped-process evidence; recovery refused", obs.ID)
			}
		}
		if v.Protected != nil {
			return stopped(*v.Protected)
		}
		return nil
	}
	for _, a := range r.VerificationAttempts {
		if a.Status == "started" || a.Result == nil {
			return fmt.Errorf("verification attempt has missing durable process evidence; recovery refused")
		}
		raw, err := os.ReadFile(filepath.Join(r.LabDir, filepath.FromSlash(a.Result.Path)))
		if err != nil {
			return err
		}
		var result verification.Result
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if result.SchemaVersion != "v1" {
			return fmt.Errorf("invalid verification result; recovery refused")
		}
		if err = stopped(result); err != nil {
			return err
		}
	}
	if r.Verification != nil && len(r.VerificationAttempts) == 0 {
		return fmt.Errorf("legacy verification lacks durable process evidence; recovery refused")
	}
	return nil
}

// CompletedCoding requires immutable semantic evidence, including for historical delivery.
func CompletedCoding(r Record) bool { return completedCoding(r) }
