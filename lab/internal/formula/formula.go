// Package formula validates durable recipes and retains their exact approved bytes.
package formula

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go.yaml.in/yaml/v4"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Binding struct {
	Binding      string   `yaml:"binding"`
	Command      []string `yaml:"command"`
	TimeoutMS    int      `yaml:"timeoutMs"`
	Instructions string   `yaml:"instructions"`
}
type Atom struct {
	Purpose   string   `yaml:"purpose"`
	ID        string   `yaml:"id"`
	Type      string   `yaml:"type"`
	Command   []string `yaml:"command"`
	TimeoutMS int      `yaml:"timeoutMs"`
	Source    string   `yaml:"source"`
	Binding   string   `yaml:"binding"`
	Workflows []string `yaml:"workflows"`
}
type Intake struct {
	Source string `yaml:"source"`
	Repo   string `yaml:"repo"`
}
type Formula struct {
	SchemaVersion string  `yaml:"schemaVersion"`
	Kind          string  `yaml:"kind"`
	ID            string  `yaml:"id"`
	Name          string  `yaml:"name"`
	Harness       Binding `yaml:"harness"`
	Intake        Intake  `yaml:"intake"`
	Atoms         []Atom  `yaml:"atoms"`
	YAML          string  `yaml:"-"`
	SHA256        string  `yaml:"-"`
}
type Loaded struct {
	Formula  Formula
	Approved bool
	Approval Approval
	File     string
}

func validateCommand(m map[string]any) error {
	if value, ok := m["command"]; ok {
		args, ok := value.([]any)
		if !ok || len(args) == 0 {
			return fmt.Errorf("command must be a non-empty argv array")
		}
		for _, arg := range args {
			s, ok := arg.(string)
			if !ok || strings.TrimSpace(s) == "" || strings.ContainsRune(s, 0) {
				return fmt.Errorf("command argv must contain non-empty strings")
			}
		}
	}
	if value, ok := m["timeoutMs"]; ok {
		n, ok := value.(int)
		if !ok || n < 1 || n > 3600000 {
			return fmt.Errorf("timeoutMs must be an integer from 1 to 3600000")
		}
	}
	return nil
}
func Parse(data []byte) (Formula, error) {
	var f Formula
	if len(data) > 1_000_000 {
		return f, fmt.Errorf("Formula exceeds 1 MB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return f, err
	}
	if raw == nil {
		return f, fmt.Errorf("Formula must be a mapping")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return f, fmt.Errorf("Formula must contain exactly one YAML document")
	}
	if raw["kind"] != "formula" {
		return f, fmt.Errorf("kind must be formula")
	}
	if v, ok := raw["schemaVersion"]; ok && v != "v0" {
		return f, fmt.Errorf("unsupported Formula schema")
	}
	id, ok := raw["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return f, fmt.Errorf("Formula id is required")
	}
	atoms, ok := raw["atoms"].([]any)
	if !ok || len(atoms) == 0 {
		return f, fmt.Errorf("Formula needs at least one Atom")
	}
	for _, value := range atoms {
		m, ok := value.(map[string]any)
		if !ok {
			return f, fmt.Errorf("Atom must be a mapping")
		}
		if err := validateCommand(m); err != nil {
			return f, err
		}
		typ, ok := m["type"].(string)
		if !ok || strings.TrimSpace(typ) == "" {
			return f, fmt.Errorf("Atom type is required")
		}
	}
	if value, exists := raw["harness"]; exists {
		m, ok := value.(map[string]any)
		if !ok {
			return f, fmt.Errorf("harness must be a mapping")
		}
		if err := validateCommand(m); err != nil {
			return f, err
		}
		if value, ok := m["instructions"]; ok {
			s, ok := value.(string)
			if !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 8000 {
				return f, fmt.Errorf("instructions must be non-empty bounded text")
			}
		}
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return f, err
	}
	f.ID = strings.TrimSpace(f.ID)
	if f.Name == "" {
		f.Name = f.ID
	}
	if f.SchemaVersion == "" {
		f.SchemaVersion = "v0"
	}
	if f.Harness.Binding == "" {
		f.Harness.Binding = "unbound"
	}
	if f.Intake.Source == "" {
		f.Intake.Source = "github-issues"
	}
	seen := map[string]bool{}
	for i := range f.Atoms {
		a := &f.Atoms[i]
		a.Type = strings.TrimSpace(a.Type)
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" {
			a.ID = fmt.Sprintf("%s-%d", a.Type, i+1)
		}
		if seen[a.ID] {
			return Formula{}, fmt.Errorf("duplicate Atom id: %s", a.ID)
		}
		seen[a.ID] = true
	}
	f.YAML = string(data)
	f.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	return f, nil
}

var unsafeFilename = regexp.MustCompile(`[^a-z0-9._-]+`)

func Inspect(labDir, id string) (Loaded, error) {
	if id == "" {
		data, err := os.ReadFile(filepath.Join(labDir, "lab.json"))
		if os.IsNotExist(err) {
			return Loaded{}, nil
		}
		if err != nil {
			return Loaded{}, err
		}
		var cfg struct {
			SchemaVersion   string `json:"schemaVersion"`
			ActiveFormulaID string `json:"activeFormulaId"`
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			return Loaded{}, fmt.Errorf("invalid Lab configuration: %w", err)
		}
		if cfg.SchemaVersion != "v0" || cfg.ActiveFormulaID == "" {
			return Loaded{}, fmt.Errorf("invalid Lab configuration")
		}
		id = cfg.ActiveFormulaID
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return Loaded{}, fmt.Errorf("invalid Formula id")
	}
	name := strings.Trim(unsafeFilename.ReplaceAllString(strings.ToLower(id), "-"), "-")
	if name == "" || name == "." || name == ".." {
		return Loaded{}, fmt.Errorf("invalid Formula id")
	}
	file := filepath.Join(labDir, "formulas", name+".yaml")
	info, err := os.Lstat(file)
	if err != nil {
		return Loaded{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1_000_000 {
		return Loaded{}, fmt.Errorf("Formula must be a bounded regular file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Loaded{}, err
	}
	f, err := Parse(data)
	if err != nil {
		return Loaded{}, err
	}
	if f.ID != id {
		return Loaded{}, fmt.Errorf("Formula id does not match selected recipe")
	}
	return Loaded{Formula: f, File: file}, nil
}

// Approval identifies one human activation, even when two decisions approve identical bytes.
type Approval struct {
	FormulaID  string `json:"formulaId"`
	SHA256     string `json:"sha256"`
	DecisionID string `json:"decisionId"`
	SourceKind string `json:"sourceKind"`
	SourceID   string `json:"sourceId"`
}
type activation struct {
	ActiveIDBefore   string   `json:"activeIdBefore,omitempty"`
	ActiveHashBefore string   `json:"activeHashBefore,omitempty"`
	Approval         Approval `json:"approval"`
	YAML             string   `json:"yaml"`
	ConfigBefore     string   `json:"configBefore"`
	TargetBefore     string   `json:"targetBefore"`
	Config           string   `json:"config"`
	Complete         bool     `json:"complete"`
}

func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func FileHash(path string) (string, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return "absent", nil
	}
	if e != nil {
		return "", e
	}
	return digest(b), nil
}
func approvalError(dir string, e error) error {
	return fmt.Errorf("Formula execution approval rejected: %v; review current YAML with forgecell init --lab %q, then init --lab %q --approve PROPOSAL_ID", e, dir, dir)
}
func decisionFile(dir string, a Approval) (string, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(a.DecisionID) || a.SourceID == "" || a.SourceKind == "" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(a.SHA256) {
		return "", fmt.Errorf("invalid approval identity")
	}
	return filepath.Join(dir, "formula-decisions", a.DecisionID+".json"), nil
}
func readActivation(dir string, a Approval) (activation, error) {
	var v activation
	path, e := decisionFile(dir, a)
	if e != nil {
		return v, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return v, e
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	f, e := Parse([]byte(v.YAML))
	if e != nil || v.Approval != a || f.ID != a.FormulaID || f.SHA256 != a.SHA256 {
		return v, fmt.Errorf("activation intent changed")
	}
	var cfg struct {
		Active   string   `json:"activeFormulaId"`
		Approval Approval `json:"currentApproval"`
	}
	if json.Unmarshal([]byte(v.Config), &cfg) != nil || cfg.Active != a.FormulaID || cfg.Approval != a {
		return v, fmt.Errorf("activation configuration changed")
	}
	return v, nil
}

// Load is the execution boundary. Inspect remains available for historical/legacy review.
func Load(dir, id string) (Loaded, error) {
	if _, e := os.Lstat(filepath.Join(dir, ".formula-write.lock")); e == nil {
		return Loaded{}, approvalError(dir, fmt.Errorf("Formula activation is locked; inspect interrupted approval and resume its exact decision"))
	} else if !os.IsNotExist(e) {
		return Loaded{}, approvalError(dir, e)
	}
	return loadCurrent(dir, id, "")
}

// LoadForReview verifies the current decision while the caller owns the shared write lock.
func LoadForReview(dir, id, decision string) (Loaded, error) { return loadCurrent(dir, id, decision) }
func loadCurrent(dir, id, reviewingDecision string) (Loaded, error) {
	loaded, e := Inspect(dir, id)
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	if loaded.Formula.ID == "" {
		return loaded, nil
	}
	b, e := os.ReadFile(filepath.Join(dir, "lab.json"))
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	var cfg struct {
		Schema   string   `json:"schemaVersion"`
		Active   string   `json:"activeFormulaId"`
		Approval Approval `json:"currentApproval"`
	}
	if e = json.Unmarshal(b, &cfg); e != nil {
		return loaded, approvalError(dir, e)
	}
	a := cfg.Approval
	if cfg.Schema != "v0" || cfg.Active != loaded.Formula.ID || a.FormulaID != cfg.Active || a.SHA256 != loaded.Formula.SHA256 {
		return loaded, approvalError(dir, fmt.Errorf("missing, stale or non-current exact-byte approval"))
	}
	v, e := readActivation(dir, a)
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	if !v.Complete || v.Config != string(b) {
		return loaded, approvalError(dir, fmt.Errorf("activation decision is incomplete or stale"))
	}

	{
		entries, e := os.ReadDir(filepath.Join(dir, "formula-decisions"))
		if e != nil {
			return loaded, approvalError(dir, e)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(dir, "formula-decisions", entry.Name()))
			if e != nil {
				return loaded, approvalError(dir, e)
			}
			var pending activation
			if json.Unmarshal(data, &pending) == nil && !pending.Complete && pending.Approval.DecisionID != reviewingDecision && (pending.ConfigBefore == digest(b) || pending.Config == string(b) || pending.Approval == a) {
				return loaded, approvalError(dir, fmt.Errorf("unfinished activation intent; resume its exact approve decision before execution"))
			}
		}
	}
	loaded.Approved = true
	loaded.Approval = a
	return loaded, nil
}

// Revalidate checks both bytes and decision freshness immediately before invocation.
func Revalidate(dir string, expected Loaded) error {
	current, e := Load(dir, expected.Formula.ID)
	if e != nil {
		return e
	}
	if !current.Approved || current.Approval != expected.Approval {
		return approvalError(dir, fmt.Errorf("approval identity changed"))
	}
	return nil
}
func durable(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".approval-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func saveActivation(dir string, v activation) error {
	path, e := decisionFile(dir, v.Approval)
	if e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return durable(path, append(b, '\n'))
}

// BeginActivation is called under .formula-write.lock, before replacing any bytes.
func BeginActivation(dir, yaml string, a Approval, configBefore, targetBefore string) error {
	f, e := Parse([]byte(yaml))
	if e != nil {
		return e
	}
	if a.FormulaID != f.ID || a.SHA256 != f.SHA256 {
		return fmt.Errorf("approval bytes differ")
	}
	path, e := decisionFile(dir, a)
	if e != nil {
		return e
	}
	if _, e = os.Stat(path); e == nil {
		v, e := readActivation(dir, a)
		if e != nil {
			return e
		}
		if v.YAML != yaml || v.ConfigBefore != configBefore || v.TargetBefore != targetBefore {
			return fmt.Errorf("conflicting activation intent")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	actual, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	if actual != configBefore {
		return fmt.Errorf("configuration changed; stale activation refused")
	}
	inspected, e := Inspect(dir, f.ID)
	if e == nil && inspected.Formula.SHA256 != targetBefore {
		return fmt.Errorf("target changed; stale activation refused")
	}
	if e != nil && !(os.IsNotExist(e) && targetBefore == "absent") {
		return e
	}
	cfg := map[string]any{}
	if configBefore != "absent" {
		b, e := os.ReadFile(filepath.Join(dir, "lab.json"))
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &cfg); e != nil {
			return e
		}
	}
	activeID, activeHash := "", ""
	if configBefore != "absent" {
		active, e := Inspect(dir, "")
		if e != nil {
			return e
		}
		activeID, activeHash = active.Formula.ID, active.Formula.SHA256
	}
	cfg["schemaVersion"] = "v0"
	cfg["activeFormulaId"] = f.ID
	cfg["currentApproval"] = a
	b, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		return e
	}
	return saveActivation(dir, activation{ActiveIDBefore: activeID, ActiveHashBefore: activeHash, Approval: a, YAML: yaml, ConfigBefore: configBefore, TargetBefore: targetBefore, Config: string(append(b, '\n'))})
}

// verifyOriginalActive permits replacement of the transaction's target, but a
// changed-ID activation never writes the original recipe. That snapshot must
// remain intact throughout recovery, even after lab.json has been replaced.
func verifyOriginalActive(dir string, v activation) error {
	if v.ActiveIDBefore == "" {
		if v.ConfigBefore != "absent" {
			return fmt.Errorf("activation lacks original Formula snapshot; review a new proposal")
		}
		return nil
	}
	if v.ActiveIDBefore == v.Approval.FormulaID {
		return nil
	}
	active, e := Inspect(dir, v.ActiveIDBefore)
	if e != nil || active.Formula.SHA256 != v.ActiveHashBefore {
		return fmt.Errorf("original active Formula changed; interrupted activation refused; review a new proposal")
	}
	return nil
}

// ApplyActivation retries only the saved transition; it cannot overwrite a newer approval.
func ApplyActivation(dir string, a Approval) error {
	v, e := readActivation(dir, a)
	if e != nil {
		return e
	}
	cfg, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	if cfg != v.ConfigBefore && cfg != digest([]byte(v.Config)) {
		return fmt.Errorf("newer configuration conflicts with activation")
	}
	loaded, e := Inspect(dir, a.FormulaID)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	actual := "absent"
	if e == nil {
		actual = loaded.Formula.SHA256
	}
	if actual != v.TargetBefore && actual != a.SHA256 {
		return fmt.Errorf("recipe conflicts with interrupted activation")
	}
	if v.Complete {
		if cfg != digest([]byte(v.Config)) || actual != a.SHA256 {
			return fmt.Errorf("completed activation is stale")
		}
		return nil
	}
	if e = verifyOriginalActive(dir, v); e != nil {
		return e
	}
	name := strings.Trim(unsafeFilename.ReplaceAllString(strings.ToLower(a.FormulaID), "-"), "-")
	if e = durable(filepath.Join(dir, "formulas", name+".yaml"), []byte(v.YAML)); e != nil {
		return e
	}
	return durable(filepath.Join(dir, "lab.json"), []byte(v.Config))
}
func CompleteActivation(dir string, a Approval) error {
	v, e := readActivation(dir, a)
	if e != nil {
		return e
	}
	cfg, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	f, e := Inspect(dir, a.FormulaID)
	if e != nil {
		return e
	}
	if cfg != digest([]byte(v.Config)) || f.Formula.SHA256 != a.SHA256 {
		return fmt.Errorf("activation changed before completion")
	}
	if !v.Complete {
		if e = verifyOriginalActive(dir, v); e != nil {
			return e
		}
	}
	v.Complete = true
	return saveActivation(dir, v)
}

// ActivationStarted distinguishes an untouched proposal from a persisted approval intent.
func ActivationStarted(dir string, a Approval) (bool, error) {
	path, e := decisionFile(dir, a)
	if e != nil {
		return false, e
	}
	if _, e = os.Lstat(path); os.IsNotExist(e) {
		return false, nil
	} else if e != nil {
		return false, e
	}
	_, e = readActivation(dir, a)
	return true, e
}
