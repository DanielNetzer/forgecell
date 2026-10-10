package learning

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// These are evidence labels, not new primitives or correctness claims.
type EvidenceOutcome struct {
	CIClassification      string                         `json:"ciClassification"`
	CIObservations        []delivery.CIObservation       `json:"ciObservations"`
	Lifecycle             *delivery.LifecycleObservation `json:"lifecycle,omitempty"`
	MoleculeID            string                         `json:"moleculeId"`
	Outcome               string                         `json:"outcome"`
	IndependentAcceptance bool                           `json:"independentAcceptance"`
	Commit                string                         `json:"commit,omitempty"`
	URL                   string                         `json:"url,omitempty"`
}

func classifyEvidence(lab string, r molecule.Record) (EvidenceOutcome, error) {
	if err := molecule.ValidateVerificationHistory(r); err != nil {
		return EvidenceOutcome{}, err
	}
	e := EvidenceOutcome{MoleculeID: r.ID, Outcome: "historical-unknown", CIClassification: "unknown", CIObservations: []delivery.CIObservation{}}
	if r.Readiness != nil {
		switch r.Readiness.Phase {
		case "scope-waiting", "approved", "coding", "check-pending":
			return e, fmt.Errorf("learning requires a stopped or verified attempt; %s is pending", r.ID)
		case "review-waiting":
			if _, err := r.Readiness.ValidateForDelivery(); err != nil {
				return e, err
			}
			if r.Verification == nil || !r.Verification.RequiredChecksPassed || r.Verification.Error != "" {
				return e, fmt.Errorf("missing successful verification evidence")
			}
			e.Outcome = "verified-undelivered"
			e.IndependentAcceptance = r.Verification.IndependentAcceptanceVerified
		case "failed", "scope-violation", "scope-change", "interrupted", "dismissed":
			e.Outcome = "failed"
		default:
			return e, fmt.Errorf("unknown readiness outcome")
		}
	} else {
		if r.Status == "running" || r.Status == "pending" || r.Status == "waiting" || r.FinishedAt == "" {
			return e, fmt.Errorf("unfinished historical Molecule")
		}
		if r.Status == "failed" || r.Status == "blocked" {
			e.Outcome = "failed"
		}
	}
	path := filepath.Join(lab, "deliveries", r.ID+".json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return e, nil
	} else if err != nil {
		return e, err
	}
	raw, err := bounded(path)
	if err != nil {
		return e, err
	}
	var receipt delivery.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		return e, err
	}
	if receipt.State != "published" {
		return e, fmt.Errorf("publication is pending or uncertain; reconcile delivery before learning")
	}
	if e.Outcome != "verified-undelivered" || !delivery.MatchesPublishedEvidence(receipt, r) {
		return e, fmt.Errorf("published receipt does not match verified Molecule")
	}
	e.Outcome = "published-draft-status-unknown"
	if receipt.DraftAtPublication {
		e.Outcome = "draft-published"
	}
	e.Lifecycle = receipt.Lifecycle
	e.Commit = receipt.Commit
	e.URL = receipt.URL
	e.CIObservations, err = delivery.ReadCI(delivery.Options{LabDir: lab, MoleculeID: r.ID})
	if err != nil {
		return e, err
	}
	e.CIClassification = delivery.CIClassification(e.CIObservations)
	return e, nil
}
