package onboarding

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"go.yaml.in/yaml/v4"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// BindingReport is saved evidence for the exact coding command, not a fresh probe.
type BindingReport struct {
	Workflow   harness.Capability `json:"workflow"`
	Command    []string           `json:"command"`
	Candidate  Candidate          `json:"candidate"`
	LearnBound bool               `json:"learnBound"`
	Meta       *MetaReport        `json:"meta,omitempty"`
}
type Proposal struct {
	CapabilityVersion  string         `json:"capabilityVersion,omitempty"`
	CapabilityEvidence *BindingReport `json:"capabilityEvidence,omitempty"`

	Version          string `json:"version,omitempty"`
	EvidencePath     string `json:"evidencePath,omitempty"`
	EvidenceHash     string `json:"evidenceHash,omitempty"`
	ActiveIDBefore   string `json:"activeIdBefore,omitempty"`
	ActiveHashBefore string `json:"activeHashBefore,omitempty"`
	ConfigBefore     string `json:"configBefore"`
	ID               string `json:"id"`
	YAML             string `json:"yaml"`
	YAMLHash         string `json:"yamlHash"`
	Baseline         string `json:"baseline"`
	TargetBefore     string `json:"targetBefore"`
	CreatedAt        string `json:"createdAt"`
	Status           string `json:"status"`
	Applying         bool   `json:"applying,omitempty"`
	ReviewedAt       string `json:"reviewedAt,omitempty"`
}

func hash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func fingerprint(dir string) (string, error) {
	cfg, err := os.ReadFile(filepath.Join(dir, "lab.json"))
	if os.IsNotExist(err) {
		return hash([]byte("no-active-formula")), nil
	}
	if err != nil {
		return "", err
	}
	active, err := formula.Inspect(dir, "")
	if err != nil {
		return "", err
	}
	return hash(append(append(cfg, '\n'), []byte(active.Formula.YAML)...)), nil
}
func proposalID(p Proposal) string {
	identity := []string{p.YAML, p.Baseline, p.TargetBefore, p.CreatedAt, p.ConfigBefore}
	// Preserve the identities of historical proposals for inspection.
	if p.ActiveIDBefore != "" || p.ActiveHashBefore != "" {
		identity = append(identity, p.ActiveIDBefore, p.ActiveHashBefore)
	}
	if p.Version != "" || p.EvidencePath != "" || p.EvidenceHash != "" {
		identity = append(identity, p.Version, p.EvidencePath, p.EvidenceHash)
	}
	if p.CapabilityVersion != "" || p.CapabilityEvidence != nil {
		evidence, _ := json.Marshal(p.CapabilityEvidence)
		identity = append(identity, p.CapabilityVersion, string(evidence))
	}
	data, _ := json.Marshal(identity)
	return "assay-" + hash(data)
}
func targetFile(dir, id string) (string, error) {
	if strings.ContainsAny(id, "/\\\x00") {
		return "", fmt.Errorf("invalid Formula id")
	}
	name := regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(strings.ToLower(id), "-")
	name = strings.Trim(name, "-")
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid Formula id")
	}
	return filepath.Join(dir, "formulas", name+".yaml"), nil
}
func fileHash(file string) (string, error) {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	return hash(data), nil
}
func write(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-*")
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
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func saveProposal(dir string, p Proposal) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return write(filepath.Join(dir, "assays", p.ID+".json"), append(data, '\n'))
}
func SaveProposal(dir, yaml string) (Proposal, error) {
	return saveWithEvidence(dir, yaml, nil)
}
func saveWithEvidence(dir, yaml string, evidence *Repository, reports ...*BindingReport) (Proposal, error) {
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		return Proposal{}, err
	}
	baseline, err := fingerprint(dir)
	if err != nil {
		return Proposal{}, err
	}
	target, err := targetFile(dir, f.ID)
	if err != nil {
		return Proposal{}, err
	}
	before, err := fileHash(target)
	if err != nil {
		return Proposal{}, err
	}
	cfgHash, err := fileHash(filepath.Join(dir, "lab.json"))
	if err != nil {
		return Proposal{}, err
	}
	p := Proposal{ConfigBefore: cfgHash, YAML: yaml, YAMLHash: f.SHA256, Baseline: baseline, TargetBefore: before, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "pending"}
	if cfgHash != "absent" {
		active, e := formula.Inspect(dir, "")
		if e != nil {
			return Proposal{}, e
		}
		p.ActiveIDBefore = active.Formula.ID
		p.ActiveHashBefore = active.Formula.SHA256
	}
	if evidence != nil {
		data, e := json.Marshal(evidence)
		if e != nil {
			return Proposal{}, e
		}
		if len(data) > 16_000_000 {
			return Proposal{}, fmt.Errorf("review evidence exceeds 16 MB")
		}
		if e = validateEvidence(*evidence); e != nil {
			return Proposal{}, e
		}
		if e = matchEvidence(yaml, *evidence); e != nil {
			return Proposal{}, e
		}
		p.Version = "v1"
		p.EvidenceHash = hash(data)
		p.EvidencePath = "assays/evidence/" + p.EvidenceHash + ".json"
		if e = safeParents(dir, p.EvidencePath, true); e != nil {
			return Proposal{}, e
		}
		if e = write(filepath.Join(dir, p.EvidencePath), data); e != nil {
			return Proposal{}, e
		}
	}
	if len(reports) > 0 && reports[0] != nil {
		p.CapabilityVersion = "v1"
		if reports[0].Meta != nil {
			p.CapabilityVersion = "v2"
		}
		p.CapabilityEvidence = reports[0]
		p.CapabilityEvidence.Candidate = p.CapabilityEvidence.Candidate.CompleteUnknowns()
		p.CapabilityEvidence.Workflow = p.CapabilityEvidence.Candidate.WorkflowCapability()
		if !reflect.DeepEqual(p.CapabilityEvidence.Command, f.Harness.Command) || p.CapabilityEvidence.Candidate.ID != f.Harness.Binding {
			return Proposal{}, fmt.Errorf("capability binding mismatch")
		}
	}
	if err := validateMetaReport(f, p); err != nil {
		return Proposal{}, err
	}
	p.ID = proposalID(p)
	if err = safeParents(dir, "assays/"+p.ID+".json", true); err != nil {
		return Proposal{}, err
	}
	return p, saveProposal(dir, p)
}
func Review(dir, id, decision string) (p Proposal, err error) {
	if !regexp.MustCompile(`^assay-[a-f0-9]{64}$`).MatchString(id) {
		return p, fmt.Errorf("invalid assay id")
	}
	if decision != "approve" && decision != "dismiss" {
		return p, fmt.Errorf("invalid decision")
	}
	lock := filepath.Join(dir, ".formula-write.lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return p, fmt.Errorf("another Formula write is active; inspect lock before recovery")
	}
	defer os.Remove(lock)
	p, err = InspectProposal(dir, id)
	if err != nil {
		return p, err
	}

	f, err := formula.Parse([]byte(p.YAML))
	if err != nil {
		return p, err
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
	if p.Status == "approved" && decision == "approve" {
		if err = formula.ApplyActivation(dir, a); err != nil {
			return p, err
		}
		return p, formula.CompleteActivation(dir, a)
	}
	if p.Status == "dismissed" && decision == "dismiss" {
		return p, nil
	}
	if p.Status != "pending" {
		return p, fmt.Errorf("proposal already reviewed")
	}
	started, e := formula.ActivationStarted(dir, a)
	if e != nil {
		return p, e
	}
	if (p.Applying || started) && decision == "dismiss" {
		return p, fmt.Errorf("approval started; resume approve before further decisions")
	}
	if decision == "dismiss" {
		p.Status = "dismissed"
		p.ReviewedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return p, saveProposal(dir, p)
	}
	if p.ActiveIDBefore != "" && p.ActiveIDBefore != f.ID {
		active, e := formula.Inspect(dir, p.ActiveIDBefore)
		if e != nil || active.Formula.SHA256 != p.ActiveHashBefore {
			return p, fmt.Errorf("original active Formula changed; stale approval refused; review a new proposal")
		}
	}
	// Applying alone cannot bypass the baseline: older proposals may lack
	// a separately saved original snapshot or a persisted activation intent.
	if !p.Applying || !started {
		current, e := fingerprint(dir)
		if e != nil || current != p.Baseline {
			return p, fmt.Errorf("Formula changed since proposal; stale approval refused")
		}
	}
	if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
		return p, err
	}
	p.Applying = true
	if err = saveProposal(dir, p); err != nil {
		return p, err
	}
	if err = formula.ApplyActivation(dir, a); err != nil {
		return p, err
	}
	p.Status = "approved"
	p.ReviewedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err = saveProposal(dir, p); err != nil {
		return p, err
	}
	return p, formula.CompleteActivation(dir, a)
}

// safeParents refuses traversal and symlinks, including artifact directories.
func safeParents(dir, relative string, create bool) error {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("unsafe review path")
	}
	current := filepath.Clean(dir)
	// Permit only the OS's standard temporary-directory aliases outside the Lab.
	for ancestor := current; ; ancestor = filepath.Dir(ancestor) {
		st, err := os.Lstat(ancestor)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(ancestor)
			systemAlias := (ancestor == "/var" && target == "private/var") || (ancestor == "/tmp" && target == "private/tmp")
			if e != nil || ancestor == current || !systemAlias {
				return fmt.Errorf("symlink in review path")
			}
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) && create && i < len(parts)-1 {
			if err = os.MkdirAll(current, 0700); err != nil {
				return err
			}
			continue
		}
		if os.IsNotExist(err) && create {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !st.IsDir()) || (i == len(parts)-1 && !st.Mode().IsRegular()) {
			return fmt.Errorf("unsafe review artifact")
		}
	}
	return nil
}
func readArtifact(dir, relative string) ([]byte, error) {
	if err := safeParents(dir, relative, false); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, relative))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16_000_001))
	if len(data) > 16_000_000 {
		return nil, fmt.Errorf("review evidence exceeds 16 MB")
	}
	return data, err
}
func validateEvidence(r Repository) error {
	if r.Repo == "" || r.Revision == "" {
		return fmt.Errorf("missing evidence repository/revision")
	}
	seen := map[string]bool{}
	total := 0
	for _, e := range r.Evidence {
		if e.Path == "" || filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path || strings.HasPrefix(e.Path, "../") || strings.ContainsAny(e.Path, "\\\x00") || seen[e.Path] || hash([]byte(e.Content)) != e.SHA256 {
			return fmt.Errorf("invalid evidence path/hash: %s", e.Path)
		}
		seen[e.Path] = true
		total += len(e.Content)
	}
	if total > 12_000_000 {
		return fmt.Errorf("evidence exceeds 12 MB")
	}
	return nil
}

// InspectProposal performs no discovery, writes, locks or activation.
func InspectProposal(dir, id string) (p Proposal, err error) {
	if !regexp.MustCompile(`^assay-[a-f0-9]{64}$`).MatchString(id) {
		return p, fmt.Errorf("invalid assay id")
	}
	data, err := readArtifact(dir, "assays/"+id+".json")
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if p.ID != id || proposalID(p) != id || hash([]byte(p.YAML)) != p.YAMLHash {
		return p, fmt.Errorf("proposal changed since review")
	}
	if _, err = formula.Parse([]byte(p.YAML)); err != nil {
		return p, err
	}
	if p.CapabilityVersion != "" || p.CapabilityEvidence != nil {
		f, e := formula.Parse([]byte(p.YAML))
		if e != nil || (p.CapabilityVersion != "v1" && p.CapabilityVersion != "v2") || p.CapabilityEvidence == nil || !reflect.DeepEqual(p.CapabilityEvidence.Command, f.Harness.Command) || p.CapabilityEvidence.Candidate.ID != f.Harness.Binding {
			return p, fmt.Errorf("invalid saved capability binding")
		}
	}
	f, _ := formula.Parse([]byte(p.YAML))
	if err = validateMetaReport(f, p); err != nil {
		return p, err
	}
	if p.Version == "" && p.EvidencePath == "" && p.EvidenceHash == "" {
		return p, nil
	}
	if p.Version != "v1" || p.EvidencePath != "assays/evidence/"+p.EvidenceHash+".json" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.EvidenceHash) {
		return p, fmt.Errorf("invalid evidence binding")
	}
	data, err = readArtifact(dir, p.EvidencePath)
	if err != nil {
		return p, err
	}
	if hash(data) != p.EvidenceHash {
		return p, fmt.Errorf("review evidence hash mismatch")
	}
	var r Repository
	if err = json.Unmarshal(data, &r); err != nil {
		return p, err
	}
	if err = validateEvidence(r); err != nil {
		return p, err
	}
	if err = matchEvidence(p.YAML, r); err != nil {
		return p, err
	}
	return p, nil
}

func matchEvidence(text string, r Repository) error {
	var context struct {
		RepositoryContext struct {
			Repo           string `yaml:"repo"`
			Revision       string `yaml:"revision"`
			EvidenceSHA256 string `yaml:"evidenceSha256"`
			Evidence       []struct {
				Path   string `yaml:"path"`
				SHA256 string `yaml:"sha256"`
			} `yaml:"evidence"`
		} `yaml:"repositoryContext"`
	}
	if err := yaml.Unmarshal([]byte(text), &context); err != nil {
		return err
	}
	f, err := formula.Parse([]byte(text))
	if err != nil {
		return err
	}
	c := context.RepositoryContext
	if !strings.EqualFold(f.Intake.Repo, r.Repo) || c.Repo != r.Repo || c.Revision != r.Revision || c.EvidenceSHA256 != r.EvidenceSHA256 || len(c.Evidence) != len(r.Evidence) {
		return fmt.Errorf("evidence repository/revision mismatch")
	}
	for i, e := range r.Evidence {
		if c.Evidence[i].Path != e.Path || c.Evidence[i].SHA256 != e.SHA256 {
			return fmt.Errorf("evidence reference mismatch")
		}
	}
	return nil
}

// ProposalActivationIncomplete inspects saved progress without changing the Lab.
// Applying is retained as a recovery hint for historical records without intent.
func ProposalActivationIncomplete(dir string, p Proposal) (bool, error) {
	if !regexp.MustCompile(`^assay-[a-f0-9]{64}$`).MatchString(p.ID) {
		return false, fmt.Errorf("invalid assay id")
	}
	relative := "formula-decisions/" + p.ID + ".json"
	data, err := readArtifact(dir, relative)
	if os.IsNotExist(err) {
		return p.Applying && p.Status == "pending", nil
	}
	if err != nil {
		return false, err
	}
	f, err := formula.Parse([]byte(p.YAML))
	if err != nil {
		return false, err
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
	if _, err = formula.ActivationStarted(dir, a); err != nil {
		return false, err
	}
	var progress struct {
		Complete bool `json:"complete"`
	}
	if err = json.Unmarshal(data, &progress); err != nil {
		return false, err
	}
	return !progress.Complete, nil
}

func validateMetaReport(f formula.Formula, p Proposal) error {
	if p.CapabilityVersion == "v2" && (p.CapabilityEvidence == nil || p.CapabilityEvidence.Meta == nil) {
		return fmt.Errorf("missing saved meta evidence")
	}
	if p.CapabilityEvidence == nil || p.CapabilityEvidence.Meta == nil {
		return nil
	}
	if p.CapabilityVersion != "v2" {
		return fmt.Errorf("invalid meta capability version")
	}
	r := p.CapabilityEvidence.Meta
	count := 0
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			count++
			if !reflect.DeepEqual(a.Command, r.Command) || a.Binding != r.Binding || a.TimeoutMS != r.TimeoutMS || len(a.Command) == 0 || a.TimeoutMS < 0 || a.TimeoutMS > 3600000 {
				return fmt.Errorf("meta capability binding mismatch")
			}
		}
	}
	if count != 1 {
		return fmt.Errorf("meta evidence requires one learn Atom")
	}
	if r.BeforeYAML != "" {
		if _, err := formula.Parse([]byte(r.BeforeYAML)); err != nil {
			return err
		}
		if p.ActiveHashBefore != "" && hash([]byte(r.BeforeYAML)) != p.ActiveHashBefore {
			return fmt.Errorf("original YAML mismatch")
		}
	}
	return nil
}

func pendingProposal(dir string) (*Proposal, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "assays"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending, recovery *Proposal
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !regexp.MustCompile(`^assay-[a-f0-9]{64}$`).MatchString(id) {
			continue
		}
		p, err := InspectProposal(dir, id)
		if err != nil {
			return nil, err
		}
		incomplete, err := ProposalActivationIncomplete(dir, p)
		if err != nil {
			return nil, err
		}
		if incomplete {
			saved := p
			recovery = &saved
		}
		if p.Status == "pending" && pending == nil {
			saved := p
			pending = &saved
		}
	}
	if recovery != nil {
		return recovery, nil
	}
	return pending, nil
}
