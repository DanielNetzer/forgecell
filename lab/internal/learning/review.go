// Package learning proposes changes to the software-making process, never applies them implicitly.
package learning

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Suggestion struct {
	// Readiness is recomputed from exact proposed bytes under current rules.
	ProposedReady    bool              `json:"proposedReady"`
	ReadinessError   string            `json:"readinessError,omitempty"`
	OriginApproval   formula.Approval  `json:"originApproval"`
	ConfigBefore     string            `json:"configBefore,omitempty"`
	EvidenceOutcomes []EvidenceOutcome `json:"evidenceOutcomes,omitempty"`
	ID               string            `json:"id"`
	FormulaID        string            `json:"formulaId"`
	MoleculeIDs      []string          `json:"moleculeIds"`
	CreatedAt        string            `json:"createdAt"`
	Status           string            `json:"status"`
	Rationale        string            `json:"rationale"`
	Summary          string            `json:"summary,omitempty"`
	ExpectedImpact   string            `json:"expectedImpact,omitempty"`
	Evaluation       string            `json:"evaluation,omitempty"`
	OriginalYAML     string            `json:"originalYaml"`
	OriginalHash     string            `json:"originalHash"`
	ProposedYAML     string            `json:"proposedYaml"`
	ProposedHash     string            `json:"proposedHash"`
	Diff             string            `json:"diff"`
	ReviewedAt       string            `json:"reviewedAt,omitempty"`
	ApplyingAt       string            `json:"applyingAt,omitempty"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func hash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func stamp() string        { return time.Now().UTC().Format(time.RFC3339Nano) }
func write(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".suggestion-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), file); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func save(dir string, p Suggestion) error {
	if !validID.MatchString(p.ID) {
		return fmt.Errorf("invalid suggestion id")
	}
	b, e := json.MarshalIndent(p, "", "  ")
	if e != nil {
		return e
	}
	return write(filepath.Join(dir, "suggestions", p.ID+".json"), append(b, '\n'))
}
func bounded(file string) ([]byte, error) {
	info, e := os.Lstat(file)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > 8_000_000 {
		return nil, fmt.Errorf("expected bounded regular file")
	}
	return os.ReadFile(file)
}
func Read(dir, id string) (Suggestion, error) {
	var p Suggestion
	if !validID.MatchString(id) {
		return p, fmt.Errorf("invalid suggestion id")
	}
	b, e := bounded(filepath.Join(dir, "suggestions", id+".json"))
	if e != nil {
		return p, e
	}
	if e = json.Unmarshal(b, &p); e != nil {
		return p, e
	}
	if p.ID != id || (p.FormulaID == "" || strings.ContainsAny(p.FormulaID, "/\\\x00")) || (p.Status != "pending" && p.Status != "approved" && p.Status != "dismissed") || hash(p.OriginalYAML) != p.OriginalHash || hash(p.ProposedYAML) != p.ProposedHash {
		return p, fmt.Errorf("invalid or modified Formula suggestion")
	}
	for _, s := range []string{p.OriginalYAML, p.ProposedYAML} {
		f, e := formula.Parse([]byte(s))
		if e != nil || f.ID != p.FormulaID {
			return p, fmt.Errorf("invalid suggestion Formula identity")
		}
	}
	p.assessReadiness()
	p.Diff, e = yamlDiff(context.Background(), p.OriginalYAML, p.ProposedYAML)
	if e != nil {
		return p, e
	}
	return p, nil
}
func (p *Suggestion) assessReadiness() {
	p.ProposedReady = false
	p.ReadinessError = ""
	f, e := formula.Parse([]byte(p.ProposedYAML))
	if e == nil {
		e = f.ValidateReadiness()
	}
	if e != nil {
		p.ReadinessError = e.Error()
		return
	}
	p.ProposedReady = true
}

func Review(dir, id, decision string) (Suggestion, error) {
	var p Suggestion
	if decision != "approve" && decision != "dismiss" {
		return p, fmt.Errorf("choose approve or dismiss")
	}
	lock := filepath.Join(dir, ".formula-write.lock")
	if e := os.Mkdir(lock, 0700); e != nil {
		return p, fmt.Errorf("Formula write lock unavailable; inspect interrupted writes before removing it: %w", e)
	}
	defer os.Remove(lock)
	p, e := Read(dir, id)
	if e != nil {
		return p, e
	}

	if decision == "approve" && !p.ProposedReady {
		return p, fmt.Errorf("proposed Formula is not runnable: %s", p.ReadinessError)
	}

	a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
	if p.Status == "approved" && decision == "approve" {
		if e = formula.ApplyActivation(dir, a); e != nil {
			return p, e
		}
		// A completed retry remains a duplicate review; recovery alone is permitted.
		loaded, e := formula.LoadForReview(dir, p.FormulaID, a.DecisionID)
		if e == nil && loaded.Approved {
			return p, fmt.Errorf("suggestion already reviewed")
		}
		return p, formula.CompleteActivation(dir, a)
	}
	if p.Status != "pending" {
		return p, fmt.Errorf("suggestion already reviewed")
	}
	started, e := formula.ActivationStarted(dir, a)
	if e != nil {
		return p, e
	}
	if (p.ApplyingAt != "" || started) && decision == "dismiss" {
		return p, fmt.Errorf("approval started; repeat approve to finish its record")
	}
	if decision == "dismiss" {
		p.Status = "dismissed"
		p.ReviewedAt = stamp()
		return p, save(dir, p)
	}
	if p.ApplyingAt == "" {
		loaded, e := formula.LoadForReview(dir, p.FormulaID, a.DecisionID)
		if e != nil {
			return p, e
		}
		if !loaded.Approved || loaded.Formula.SHA256 != p.OriginalHash || loaded.Approval != p.OriginApproval {
			return p, fmt.Errorf("Formula approval identity changed; refusing stale suggestion")
		}
		p.ConfigBefore, e = formula.FileHash(filepath.Join(dir, "lab.json"))
		if e != nil {
			return p, e
		}
		if e = formula.BeginActivation(dir, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); e != nil {
			return p, e
		}
		p.ApplyingAt = stamp()
		if e = save(dir, p); e != nil {
			return p, e
		}
	} else {
		if e = formula.BeginActivation(dir, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); e != nil {
			return p, e
		}
	}
	if e = formula.ApplyActivation(dir, a); e != nil {
		return p, e
	}
	p.Status = "approved"
	p.ReviewedAt = stamp()
	if e = save(dir, p); e != nil {
		return p, e
	}
	return p, formula.CompleteActivation(dir, a)
}
