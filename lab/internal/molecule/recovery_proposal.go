package molecule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	recoveryProposalDir       = "recovery-proposals"
	adoptPlanDir              = "adopt-plans"
	recoveryProposalVersion   = "v1"
	maxRecoveryProposalBytes  = 64 * 1024
	maxRecoveryProposalsShown = 200
)

// RecoveryStep is an ordered follow-up to a proposed recovery. Nothing runs it.
type RecoveryStep struct {
	Order        int    `json:"order"`
	Command      string `json:"command"`
	PlanFile     string `json:"planFile"`
	Continuation string `json:"continuation"`
	AdoptAttempt string `json:"adoptAttempt"`
	Note         string `json:"note"`
}

// RecoveryProposal is advice for a human about an interrupted Molecule whose
// owner is gone. The loop writes it and never runs the commands it names. The
// file holds no timestamp, so an unchanged proposal rewrites to identical bytes.
type RecoveryProposal struct {
	SchemaVersion string         `json:"schemaVersion"`
	MoleculeID    string         `json:"moleculeId"`
	Repository    string         `json:"repository"`
	Issue         int64          `json:"issue"`
	Status        string         `json:"status"`
	Phase         string         `json:"phase"`
	PlanDigest    string         `json:"planDigest"`
	Command       string         `json:"command"`
	Followup      []RecoveryStep `json:"followup,omitempty"`
	Path          string         `json:"path"`

	adoptPlan []byte
}

// LoopProposal is the loop report's view of a saved proposal. Existing marks one
// that an earlier pass already saved unchanged. Followup holds runnable commands.
type LoopProposal struct {
	Path     string
	Command  string
	Followup []string
	Existing bool
}

func recoveryProposalPath(lab, id string) string {
	return filepath.Join(lab, recoveryProposalDir, id+".json")
}

func adoptPlanPath(lab, id string) string {
	return filepath.Join(lab, recoveryProposalDir, adoptPlanDir, id+".json")
}

// buildRecoveryProposal reads the ledger and returns the proposal recover would
// accept, or the reason none applies. It mirrors Recover's refusals that a ledger
// read can predict: a plan to name and a reconciled harness attempt.
func buildRecoveryProposal(lab, id string) (RecoveryProposal, string) {
	var p RecoveryProposal
	if !safeLoopID(id) {
		return p, "the Molecule id is not valid"
	}
	r, err := readMolecule(lab, id)
	if err != nil {
		return p, "the ledger could not be read: " + oneLine(err.Error())
	}
	if r.ID != id || !safeLoopID(r.ID) {
		return p, "the ledger identity does not match the proposal"
	}
	if r.Status != "running" && r.Status != "allocating" {
		return p, "the ledger is no longer interrupted"
	}
	if r.Readiness == nil || r.Readiness.Phase == "" || len(r.Readiness.Plans) == 0 {
		return p, "the ledger holds no plan for forgecell recover to name"
	}
	if len(r.HarnessAttempts) == 0 {
		return p, "the ledger records no harness attempt, so forgecell recover would refuse for lack of durable stopped-process evidence"
	}
	attempt := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if attempt.Evidence == nil || !attempt.Result.Reconciled {
		return p, "the latest harness attempt has no reconciled stopped-process evidence, so forgecell recover would refuse"
	}
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	digest := latest.Digest
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return p, "the latest plan digest is not a SHA-256 digest"
	}
	p = RecoveryProposal{
		SchemaVersion: recoveryProposalVersion,
		MoleculeID:    r.ID,
		Repository:    r.Issue.Repo,
		Issue:         r.Issue.Number,
		Status:        r.Status,
		Phase:         statePhase(r),
		PlanDigest:    digest,
		Command:       fmt.Sprintf("forgecell recover %s --plan %s --confirm-stopped --lab %s", shellWord(r.ID), shellWord(digest), shellWord(lab)),
		Path:          recoveryProposalPath(lab, r.ID),
	}
	if attempt.Capture != nil && attempt.CaptureError == "" && attempt.Capture.Tree != "" && safeLoopID(attempt.ID) {
		plan := latest.Plan
		plan.Continuation, plan.AdoptAttempt = "adopt-failed-tree", attempt.ID
		raw, err := json.MarshalIndent(plan, "", "  ")
		if err == nil {
			file := adoptPlanPath(lab, r.ID)
			p.adoptPlan = append(raw, '\n')
			p.Followup = []RecoveryStep{{
				Order:        1,
				Command:      fmt.Sprintf("forgecell amend %s --parent %s --plan %s --lab %s", shellWord(r.ID), shellWord(digest), shellWord(file), shellWord(lab)),
				PlanFile:     file,
				Continuation: "adopt-failed-tree",
				AdoptAttempt: attempt.ID,
				Note:         "Run only after recover succeeds. The plan file is the latest plan with the adopt-failed-tree continuation and this attempt; review it before approving the amended plan it produces.",
			}}
		}
	}
	return p, ""
}

// saveRecoveryProposal writes the Molecule's single proposal file atomically, and
// the adopt plan file its follow-up names. Identical files already there are left
// untouched.
func saveRecoveryProposal(p RecoveryProposal) (existing bool, err error) {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	existing = true
	if len(p.adoptPlan) > 0 && len(p.Followup) > 0 {
		file := p.Followup[0].PlanFile
		if old, readErr := os.ReadFile(file); readErr != nil || !bytes.Equal(old, p.adoptPlan) {
			if err = atomicWrite(file, p.adoptPlan); err != nil {
				return false, err
			}
			existing = false
		}
	}
	if old, readErr := os.ReadFile(p.Path); readErr != nil || !bytes.Equal(old, raw) {
		if err = atomicWrite(p.Path, raw); err != nil {
			return false, err
		}
		existing = false
	}
	return existing, nil
}

// proposeRecovery saves a proposal for an interrupted Molecule the loop reported
// as stalled. It reads the ledger and writes files under the Lab's
// recovery-proposals directory; it takes no lock, starts no process and never
// runs recover or amend.
func (l *loopRun) proposeRecovery(item LoopItem) LoopItem {
	p, why := buildRecoveryProposal(l.lab, item.MoleculeID)
	if why != "" {
		item.Reason += "; no recovery proposal was made: " + why
		return item
	}
	existing, err := saveRecoveryProposal(p)
	if err != nil {
		item.Reason += "; no recovery proposal was saved: " + oneLine(err.Error())
		return item
	}
	proposal := &LoopProposal{Path: p.Path, Command: p.Command, Existing: existing}
	for _, step := range p.Followup {
		proposal.Followup = append(proposal.Followup, step.Command)
	}
	item.Proposal = proposal
	return item
}

// currentRecoveryProposal rebuilds the proposal for a Molecule from its ledger and
// returns it only while the loop's own criteria still hold: the ledger is still
// interrupted, has not been written for longer than the analysis timeout, and no
// live process owns its approval lock.
func currentRecoveryProposal(lab, id string, timeout time.Duration) (RecoveryProposal, bool) {
	p, why := buildRecoveryProposal(lab, id)
	if why != "" || !ledgerAbandoned(lab, id, timeout) {
		return RecoveryProposal{}, false
	}
	return p, true
}

func readProposalFile(file string, limit int64) ([]byte, bool) {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false
	}
	raw, err := os.ReadFile(file)
	return raw, err == nil
}

// ReadRecoveryProposals lists the saved proposals whose Molecule is still
// interrupted by the loop's own criteria, at the recorded plan digest. It writes
// nothing and rebuilds every command from the ledger, so an edited proposal file
// cannot put text of its own in front of a person. A follow-up is kept only while
// its plan file still holds the exact plan the ledger yields.
func ReadRecoveryProposals(lab string, timeout time.Duration) ([]RecoveryProposal, error) {
	entries, err := os.ReadDir(filepath.Join(lab, recoveryProposalDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RecoveryProposal
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !safeLoopID(id) {
			continue
		}
		if len(out) >= maxRecoveryProposalsShown {
			break
		}
		raw, ok := readProposalFile(filepath.Join(lab, recoveryProposalDir, entry.Name()), maxRecoveryProposalBytes)
		if !ok {
			continue
		}
		var saved RecoveryProposal
		if json.Unmarshal(raw, &saved) != nil || saved.MoleculeID != id {
			continue
		}
		current, ok := currentRecoveryProposal(lab, id, timeout)
		if !ok || current.PlanDigest != saved.PlanDigest {
			continue
		}
		if len(current.Followup) > 0 {
			if file, ok := readProposalFile(current.Followup[0].PlanFile, 1_000_001); !ok || !bytes.Equal(file, current.adoptPlan) {
				current.Followup = nil
			}
		}
		out = append(out, current)
	}
	return out, nil
}

// pruneRecoveryProposals removes the proposal files of Molecules whose ledger no
// longer shows an interrupted Molecule that the loop would propose recovery for.
// A ledger that merely cannot be read keeps its proposal.
func pruneRecoveryProposals(lab string, timeout time.Duration) {
	entries, err := os.ReadDir(filepath.Join(lab, recoveryProposalDir))
	if err != nil {
		return
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !safeLoopID(id) {
			continue
		}
		file := filepath.Join(lab, recoveryProposalDir, entry.Name())
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if _, ok := currentRecoveryProposal(lab, id, timeout); ok {
			continue
		}
		if _, err := readMolecule(lab, id); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		os.Remove(file)
		os.Remove(adoptPlanPath(lab, id))
	}
}
