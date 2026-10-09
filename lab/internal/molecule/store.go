package molecule

import (
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func atomicWrite(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, file); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func save(r Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 5_000_000 {
		return fmt.Errorf("ledger exceeds 5 MB; state was not replaced; inspect retained verification evidence before recovery")
	}
	base := filepath.Join(r.LabDir, "ledgers", r.ID)
	if err = atomicWrite(base+".json", append(data, '\n')); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Molecule %s\n\n- Status: %s\n- Formula: %s\n- Formula SHA-256: %s\n- Issue: %s#%d\n- Workspace: %s\n- Branch: %s\n- Base commit: %s\n\n## Atoms\n", r.ID, r.Status, r.FormulaID, r.FormulaSnapshot.SHA256, r.Issue.Repo, r.Issue.Number, r.Workspace.Path, r.Workspace.Branch, r.Workspace.BaseCommit)
	if r.CheckoutStatus != "" {
		fmt.Fprintf(&b, "Checkout availability: %s\n", r.CheckoutStatus)
	}
	fmt.Fprintln(&b, "\n## Clarification history (informational; no inherited approval)")
	for _, id := range r.ClarificationPredecessors {
		fmt.Fprintf(&b, "- [%s](%s.md)\n", id, id)
	}
	if r.ClarificationHistoryTruncated {
		fmt.Fprintln(&b, "History discovery truncated by safety bounds; earlier records remain on disk.")
	}
	issueJSON, _ := json.MarshalIndent(r.Issue, "", "  ")
	fmt.Fprintf(&b, "\n## Exact Issue snapshot\n\n```json\n%s\n```\n", issueJSON)
	for _, attempt := range r.AnalysisAttempts {
		raw, _ := json.MarshalIndent(attempt, "", "  ")
		fmt.Fprintf(&b, "\n## Analysis attempt %d\n\n```json\n%s\n```\n", attempt.Ordinal, raw)
	}
	for _, a := range r.Atoms {
		fmt.Fprintf(&b, "\n### %s (%s) — %s\n\n%s\n", a.ID, a.Type, a.Status, a.Detail)
	}
	if r.Analysis != nil {
		fmt.Fprintf(&b, "\n## Intake assessment\n\n%s\n", r.Analysis.Summary)
		for _, q := range r.Analysis.Questions {
			fmt.Fprintf(&b, "\n- Question: %s\n  Reason: %s\n", q.Question, q.Reason)
		}
	}
	if r.Readiness != nil {
		for _, proposal := range r.Readiness.Plans {
			raw, _ := json.MarshalIndent(proposal.Plan, "", "  ")
			fmt.Fprintf(&b, "\n## Readiness revision %d\n\nApproval digest: `%s`\n\n```json\n%s\n```\n", proposal.Plan.Revision, proposal.Digest, raw)
		}
		fmt.Fprintln(&b, "\n## Decisions and attempts")
		for _, event := range r.Readiness.Events {
			fmt.Fprintf(&b, "\n- %s · %s · %s\n", event.At, event.Kind, event.Detail)
		}
	}
	for _, a := range r.HarnessAttempts {
		if a.Coding != nil {
			fmt.Fprintf(&b, "\nImplementation outcome: %s\nReason: %s\n", a.Coding.Outcome, a.Coding.Reason)
		} else {
			fmt.Fprintln(&b, "\nImplementation outcome: unknown (historical evidence missing)")
		}
		fmt.Fprintf(&b, "\n## Harness attempt %s\n\nPlan: `%s`\nProvider process OK: %t; exit: %d; timeout: %t; interruption: %t; overflow: %t\nReconciled: %t; uncertainty: %s\nCapture error: %s\nHuman recovery: %t\n", a.ID, a.PlanDigest, a.Result.OK, a.Result.Code, a.Result.TimedOut, a.Result.Interrupted, a.Result.Overflow, a.Result.Reconciled, a.Result.Uncertainty, a.CaptureError, a.Recovered)
		if a.Evidence != nil {
			fmt.Fprintf(&b, "Complete bounded result: `%s` (SHA-256 `%s`)\n", a.Evidence.Path, a.Evidence.SHA256)
		}
		if a.Capture != nil {
			fmt.Fprintf(&b, "Tree: `%s`; violations: %v\n", a.Capture.Tree, a.Capture.Violations)
		}
	}
	for _, a := range r.VerificationAttempts {
		fmt.Fprintf(&b, "\n## Verification attempt — %s\n\nPlan: `%s`\nTree: `%s`\nStarted: %s\nFinished: %s\n", a.Status, a.PlanDigest, a.SourceTree, a.StartedAt, a.FinishedAt)
		for _, ref := range a.Observations {
			fmt.Fprintf(&b, "- Observation: `%s` (SHA-256 `%s`)\n", ref.Path, ref.SHA256)
		}
		if a.Result != nil {
			fmt.Fprintf(&b, "- Complete result: `%s` (SHA-256 `%s`)\n", a.Result.Path, a.Result.SHA256)
		}
	}
	if r.Verification != nil {
		fmt.Fprintf(&b, "\n## Independent verification\n\nTree: `%s`\nRequired checks passed: %t\nIndependent acceptance verified: %t\n\n%s\n", r.Verification.SourceTree, r.Verification.RequiredChecksPassed, r.Verification.IndependentAcceptanceVerified, r.Verification.Error)
	}
	fmt.Fprintln(&b, "\n## Notes")
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "\n- %s\n", note)
	}
	return atomicWrite(base+".md", []byte(b.String()))
}

// History is advisory only. Bounds never broaden approval lookup or mutate predecessors.
func clarificationHistory(lab, repo string, number int64) ([]string, bool, error) {
	dir, err := os.Open(filepath.Join(lab, "ledgers"))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(4097)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	// No partial directory ordering: oversize inventories have no guessed links.
	if len(names) > 4096 {
		return nil, true, nil
	}
	sort.Strings(names)
	type predecessor struct {
		id string
		at time.Time
	}
	var matches []predecessor
	total, files := int64(0), 0
	truncated := false
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if files >= 256 {
			truncated = true
			break
		}
		files++
		path := filepath.Join(lab, "ledgers", name)
		info, e := os.Lstat(path)
		if e != nil {
			return nil, false, e
		}
		if !info.Mode().IsRegular() || info.Size() > 5_000_000 {
			truncated = true
			continue
		}
		if total+info.Size() > 20_000_000 {
			truncated = true
			break
		}
		f, e := os.Open(path)
		if e != nil {
			return nil, false, e
		}
		raw, e := io.ReadAll(io.LimitReader(f, 5_000_001))
		f.Close()
		if e != nil {
			return nil, false, e
		}
		total += int64(len(raw))
		if len(raw) > 5_000_000 {
			truncated = true
			continue
		}
		header, e := ledger.Decode(raw)
		if e != nil || header.ID+".json" != name {
			continue
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil || r.LabDir != lab || r.Mode != "ticket" ||
			!strings.EqualFold(r.Issue.Repo, repo) || r.Issue.Number != number ||
			!strings.EqualFold(r.Workspace.Repo, repo) {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, r.StartedAt)
		if e != nil {
			continue
		}
		matches = append(matches, predecessor{r.ID, at})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].at.Equal(matches[j].at) {
			return matches[i].id < matches[j].id
		}
		return matches[i].at.Before(matches[j].at)
	})
	ids := make([]string, 0, len(matches))
	for _, p := range matches {
		ids = append(ids, p.id)
	}
	return ids, truncated, nil
}
