package learning

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

type MotivatingEvidence struct {
	MoleculeID string `json:"moleculeId"`
	SHA256     string `json:"sha256"`
	Ledger     []byte `json:"ledger"`
}
type Comparison struct {
	Status      string           `json:"status"`
	Support     string           `json:"interventionSupport"`
	Reason      string           `json:"reason,omitempty"`
	Links       []ComparisonLink `json:"links"`
	Limitations []string         `json:"limitations"`
}
type ComparisonLink struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Status      string               `json:"status"`
	Reason      string               `json:"reason,omitempty"`
	Basis       string               `json:"comparabilityBasis,omitempty"`
	Evaluation  *evaluation.Evidence `json:"evaluation,omitempty"`
	Ledgers     []molecule.Record    `json:"ledgers,omitempty"`
	Outcomes    []EvidenceOutcome    `json:"outcomes,omitempty"`
	Differences []string             `json:"differences,omitempty"`
}
type LinkOptions struct {
	Evaluation                 string
	Parents                    []string
	Baseline, Candidate, Basis string
}
type comparisonRecord struct {
	Version   int                   `json:"version"`
	Identity  string                `json:"suggestionIdentity"`
	Kind      string                `json:"kind"`
	Basis     string                `json:"basis,omitempty"`
	Snapshots []evaluation.Snapshot `json:"snapshots"`
	Outcomes  []EvidenceOutcome     `json:"outcomes,omitempty"`
}

func comparisonIdentity(p Suggestion) string {
	raw, _ := json.Marshal([]any{"suggestion-comparison-v1", p.ID, p.FormulaID, p.OriginalHash, p.ProposedHash, p.OriginApproval})
	return hash(string(raw))
}
func interventionSupport(p Suggestion) string {
	raw, _ := json.Marshal(evaluation.Approval{ID: p.ID, Status: "approved", ReviewedAt: "2026-01-01T00:00:00Z", OriginalYAML: p.OriginalYAML, ProposedYAML: p.ProposedYAML, OriginalHash: p.OriginalHash, ProposedHash: p.ProposedHash})
	if _, err := evaluation.ValidateApproval(raw); err != nil {
		return "unsupported: " + err.Error()
	}
	return "instruction-only; supported by controlled evaluator"
}

// LinkComparison adds evidence under the shared write lock, with no Formula
// activation and no execution. The content-addressed record is append-only.
func LinkComparison(dir, id string, o LinkOptions) (Suggestion, error) {
	var p Suggestion
	if !validID.MatchString(id) {
		return p, fmt.Errorf("invalid suggestion id")
	}
	if _, err := evaluation.ReadLocal(dir, filepath.Join("suggestions", id+".json")); err != nil {
		return p, err
	}
	controlled := o.Evaluation != ""
	manual := o.Baseline != "" || o.Candidate != "" || o.Basis != ""
	if controlled == manual || (!controlled && len(o.Parents) > 0) {
		return p, fmt.Errorf("choose one evaluation report directory or a manual ledger pair")
	}
	lock := filepath.Join(dir, ".formula-write.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return p, fmt.Errorf("Formula write lock unavailable: %w", err)
	}
	defer os.Remove(lock)
	p, err := Read(dir, id)
	if err != nil {
		return p, err
	}
	record := comparisonRecord{Version: 1, Identity: comparisonIdentity(p)}
	if controlled {
		e, err := evaluation.ReadEvidence(o.Evaluation, o.Parents)
		if err != nil {
			return p, err
		}
		record.Kind = "controlled"
		record.Snapshots = e.Snapshots
	} else {
		if !validID.MatchString(o.Baseline) || !validID.MatchString(o.Candidate) || o.Baseline == o.Candidate || strings.TrimSpace(o.Basis) == "" || utf8.RuneCountInString(o.Basis) > 8000 {
			return p, fmt.Errorf("manual comparison requires distinct Molecule ids and a human comparability basis (1–8000 characters)")
		}
		record.Kind = "manual"
		record.Basis = o.Basis
		for i, id := range []string{o.Baseline, o.Candidate} {
			s, out, err := snapshotLedger(dir, id)
			if err != nil {
				return p, err
			}
			var r molecule.Record
			json.Unmarshal(s.Files["ledger.json"], &r)
			expected := p.OriginalHash
			if i == 1 {
				expected = p.ProposedHash
			}
			if r.FormulaID != p.FormulaID || r.FormulaSnapshot.SHA256 != expected {
				return p, fmt.Errorf("manual ledger Formula differs from exact suggestion variant")
			}
			record.Snapshots = append(record.Snapshots, s)
			record.Outcomes = append(record.Outcomes, out)
		}
	}
	if err := evaluation.ValidateSnapshots(record.Snapshots); err != nil {
		return p, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return p, err
	}
	if len(raw) > evaluation.EvidenceFileLimit {
		return p, fmt.Errorf("serialized comparison exceeds 8 MB limit; select smaller evidence")
	}
	root := filepath.Join(dir, "comparisons", p.ID)
	if err := comparisonDirectory(dir, p.ID); err != nil {
		return p, err
	}
	names, err := comparisonNames(root)
	if err != nil {
		return p, err
	}
	name := hash(string(raw)) + ".json"
	for _, existing := range names {
		if existing == name {
			before, err := evaluation.ReadLocal(root, name)
			if err != nil {
				return p, err
			}
			if string(before) != string(raw) {
				return p, fmt.Errorf("existing comparison bytes differ")
			}
			return Read(dir, id)
		}
	}
	total := len(raw)
	for _, existing := range names {
		b, e := evaluation.ReadLocal(root, existing)
		if e != nil {
			return p, e
		}
		total += len(b)
		if total > evaluation.EvidenceTotalLimit {
			return p, fmt.Errorf("comparison storage byte limit exceeded")
		}
	}
	if len(names) >= 64 {
		return p, fmt.Errorf("comparison reference limit reached")
	}
	temp, err := os.CreateTemp(root, ".comparison-")
	if err != nil {
		return p, err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(raw); err != nil {
		temp.Close()
		return p, err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return p, err
	}
	if err = temp.Close(); err != nil {
		return p, err
	}
	// A hard link publishes a complete file without replacing an existing record.
	if err = os.Link(temp.Name(), filepath.Join(root, name)); err != nil {
		return p, err
	}
	folder, err := os.Open(root)
	if err != nil {
		return p, err
	}
	err = folder.Sync()
	folder.Close()
	if err != nil {
		return p, err
	}
	return Read(dir, id)
}
func comparisonDirectory(dir, id string) error {
	path := dir
	for _, part := range []string{"comparisons", id} {
		path = filepath.Join(path, part)
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid comparison directory")
		}
	}
	return nil
}
func comparisonNames(root string) ([]string, error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(129)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > 128 {
		return nil, fmt.Errorf("comparison directory entry limit exceeded")
	}
	var result []string
	for _, name := range names {
		if strings.HasSuffix(name, ".json") {
			result = append(result, name)
		}
	}
	if len(result) > 64 {
		return nil, fmt.Errorf("comparison reference limit exceeded")
	}
	sort.Strings(result)
	return result, nil
}
func inspectComparisons(dir string, p Suggestion) Comparison {
	c := Comparison{Status: "missing", Support: interventionSupport(p), Links: []ComparisonLink{}, Limitations: []string{"Evidence links do not activate or revert a Formula. Human approval remains required.", "Manual comparability is a human assertion, not a controlled pair. Local checks do not establish production quality; binding names do not establish model identity or cost."}}
	for _, m := range p.MotivatingEvidence {
		if hash(string(m.Ledger)) != m.SHA256 {
			c.Reason = "motivating ledger bytes differ"
			c.Status = "stale"
		}
	}
	root := filepath.Join(dir, "comparisons", p.ID)
	// Optional comparison corruption must never make the suggestion unreadable.
	for _, path := range []string{filepath.Join(dir, "comparisons"), root} {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return c
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			c.Status = "stale"
			c.Reason = "invalid comparison directory"
			return c
		}
	}
	names, err := comparisonNames(root)
	if err != nil {
		c.Status = "incomplete"
		c.Reason = err.Error()
		return c
	}
	signatures := map[string]string{}
	total := 0
	for _, name := range names {
		link := ComparisonLink{ID: strings.TrimSuffix(name, ".json"), Status: "stale"}
		raw, err := evaluation.ReadLocal(root, name)
		total += len(raw)
		if total > evaluation.EvidenceTotalLimit {
			c.Status = "incomplete"
			c.Reason = "comparison storage byte limit exceeded"
			return c
		}
		var record comparisonRecord
		if err != nil || hash(string(raw))+".json" != name || json.Unmarshal(raw, &record) != nil || record.Version != 1 {
			link.Reason = "missing, modified or invalid comparison record"
		} else {
			link.Kind = record.Kind
			link.Basis = record.Basis
			if record.Identity != comparisonIdentity(p) {
				link.Reason = "suggestion or originating approval identity differs"
			} else if err := evaluation.ValidateSnapshots(record.Snapshots); err != nil {
				link.Status = "incomplete"
				link.Reason = err.Error()
			} else if record.Kind == "controlled" {
				e := evaluation.InspectEvidence(record.Snapshots)
				link.Evaluation = &e
				link.Status = e.Status
				link.Reason = e.Reason
				var a struct {
					ID, OriginalYAML, ProposedYAML, OriginalHash, ProposedHash string
					OriginApproval                                             formula.Approval
				}
				if len(e.Approval) == 0 {
					// Preserve the validator's missing-input or missing-parent diagnosis.
				} else if json.Unmarshal(e.Approval, &a) != nil {
					link.Status = "stale"
					link.Reason = "invalid originating approval identity"
				} else {
					if a.ID != p.ID || a.OriginalYAML != p.OriginalYAML || a.ProposedYAML != p.ProposedYAML || a.OriginalHash != p.OriginalHash || a.ProposedHash != p.ProposedHash || a.OriginApproval != p.OriginApproval {
						link.Status = "stale"
						link.Reason = "evaluation suggestion, Formula or originating approval identity differs"
					}
				}
				if link.Status == "comparable" {
					report := e.Reports[0]
					signatureRaw, _ := json.Marshal([]any{e.VerificationPlans[len(e.VerificationPlans)-1], report.TaskSHA256})
					signature := hash(string(signatureRaw))
					outcomes, _ := json.Marshal([]bool{report.Attempts[0].Correct, report.Attempts[1].Correct})
					if prior, ok := signatures[signature]; ok && prior != string(outcomes) {
						c.Status = "conflicting"
						c.Reason = "comparable reports record conflicting local outcomes; inspect each link"
					}
					signatures[signature] = string(outcomes)
				}
			} else if record.Kind == "manual" {
				link = inspectManual(record, p, link)
			} else {
				link.Status = "unsupported"
				link.Reason = "unknown comparison kind"
			}
		}
		c.Links = append(c.Links, link)
	}
	if c.Status != "conflicting" && c.Reason == "" {
		// Never hide an invalid link behind another valid link.
		rank := map[string]int{"missing": 0, "manual": 1, "comparable": 2, "unsupported": 3, "incomplete": 4, "stale": 5}
		for _, l := range c.Links {
			if rank[l.Status] > rank[c.Status] {
				c.Status = l.Status
				c.Reason = l.Reason
			}
		}
	}
	return c
}
func ledgerRefs(r molecule.Record) []molecule.VerificationFile {
	var refs []molecule.VerificationFile
	for _, a := range r.VerificationAttempts {
		refs = append(refs, a.Observations...)
		if a.Result != nil {
			refs = append(refs, *a.Result)
		}
	}
	for _, a := range r.HarnessAttempts {
		if a.Evidence != nil {
			refs = append(refs, *a.Evidence)
		}
		refs = append(refs, a.CaptureEvidence...)
	}
	return refs
}
func snapshotLedger(dir, id string) (evaluation.Snapshot, EvidenceOutcome, error) {
	s := evaluation.Snapshot{Files: map[string][]byte{}}
	raw, err := evaluation.ReadLocal(dir, filepath.Join("ledgers", id+".json"))
	if err != nil {
		return s, EvidenceOutcome{}, err
	}
	s.Files["ledger.json"] = raw
	var r molecule.Record
	if err = json.Unmarshal(raw, &r); err != nil {
		return s, EvidenceOutcome{}, err
	}
	if r.ID != id || r.Kind != "molecule" || r.Mode != "ticket" || r.SchemaVersion != "v0" || r.FinishedAt == "" || !r.FormulaApproved || hash(r.FormulaSnapshot.YAML) != r.FormulaSnapshot.SHA256 {
		return s, EvidenceOutcome{}, fmt.Errorf("manual comparison requires finished intact approved ticket ledgers")
	}
	// A ledger's LabDir is a reference root, not authority to read arbitrary paths.
	// Restrict history to the selected Lab and validate every reference before the
	// existing classifier reads it. Preserve exact ledger bytes in the snapshot.
	if len(ledgerRefs(r)) > 0 && filepath.Clean(r.LabDir) != filepath.Clean(dir) {
		return s, EvidenceOutcome{}, fmt.Errorf("ledger verification history is outside selected Lab")
	}
	if len(ledgerRefs(r)) > evaluation.EvidenceReferenceLimit {
		return s, EvidenceOutcome{}, fmt.Errorf("ledger history reference limit exceeded")
	}
	for _, ref := range ledgerRefs(r) {
		if !strings.HasPrefix(ref.Path, "verification-history/"+id+"/") {
			return s, EvidenceOutcome{}, fmt.Errorf("invalid ledger history reference")
		}
		b, err := evaluation.ReadLocal(dir, ref.Path)
		if err != nil {
			return s, EvidenceOutcome{}, err
		}
		if hash(string(b)) != ref.SHA256 {
			return s, EvidenceOutcome{}, fmt.Errorf("stale ledger verification evidence")
		}
		s.Files[ref.Path] = b
		if err := evaluation.ValidateSnapshots([]evaluation.Snapshot{s}); err != nil {
			return s, EvidenceOutcome{}, err
		}
	}
	receipt := filepath.Join("deliveries", id+".json")
	b, err := evaluation.ReadLocal(dir, receipt)
	if err == nil {
		s.Files[receipt] = b
	} else if !os.IsNotExist(err) {
		return s, EvidenceOutcome{}, err
	}
	out, err := classifyEvidence(dir, r)
	if err != nil {
		return s, out, err
	}
	// Refuse a source that changed during classification.
	for name, b := range s.Files {
		source := name
		if name == "ledger.json" {
			source = filepath.Join("ledgers", id+".json")
		}
		current, err := evaluation.ReadLocal(dir, source)
		if err != nil || string(current) != string(b) {
			return s, out, fmt.Errorf("ledger evidence changed while linking")
		}
	}
	return s, out, evaluation.ValidateSnapshots([]evaluation.Snapshot{s})
}
func inspectManual(record comparisonRecord, p Suggestion, link ComparisonLink) ComparisonLink {
	link.Status = "incomplete"
	if len(record.Snapshots) != 2 || len(record.Outcomes) != 2 || strings.TrimSpace(record.Basis) == "" {
		link.Reason = "missing manual pair or human comparability basis"
		return link
	}
	for i, s := range record.Snapshots {
		var r molecule.Record
		if json.Unmarshal(s.Files["ledger.json"], &r) != nil {
			link.Reason = "invalid retained ledger"
			return link
		}
		expected := p.OriginalHash
		if i == 1 {
			expected = p.ProposedHash
		}
		if r.FormulaID != p.FormulaID || r.FormulaSnapshot.SHA256 != expected || hash(r.FormulaSnapshot.YAML) != expected || record.Outcomes[i].MoleculeID != r.ID {
			link.Status = "stale"
			link.Reason = "manual Formula or ledger identity differs"
			return link
		}
		for _, ref := range ledgerRefs(r) {
			if hash(string(s.Files[ref.Path])) != ref.SHA256 {
				link.Status = "stale"
				link.Reason = "retained verification history differs"
				return link
			}
		}
		link.Ledgers = append(link.Ledgers, r)
	}
	link.Outcomes = record.Outcomes
	link.Status = "manual"
	link.Reason = "Human comparability assertion; no controlled causality. Exact task, check and verification history are retained in the ledgers."
	a, b := link.Ledgers[0], link.Ledgers[1]
	if !reflect.DeepEqual(a.Issue, b.Issue) {
		link.Differences = append(link.Differences, "frozen task/issue differs")
	}
	if a.Workspace.BaseCommit != b.Workspace.BaseCommit {
		link.Differences = append(link.Differences, "base commit differs")
	}
	if !reflect.DeepEqual(a.Readiness, b.Readiness) {
		link.Differences = append(link.Differences, "frozen readiness plans, task/check inputs or approval history differ; inspect both ledgers")
	}
	if !reflect.DeepEqual(a.VerificationAttempts, b.VerificationAttempts) || !reflect.DeepEqual(a.Verification, b.Verification) {
		link.Differences = append(link.Differences, "verification observations/history differ")
	}
	return link
}
