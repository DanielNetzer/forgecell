package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

const MaxCIObservations = 128
const MaxCIHistoryBytes = 4_000_000
const maxCIRecordBytes = checks.MaxSnapshotBytes + 4096

// CIIdentity binds evidence to the immutable delivery intent, not mutable lifecycle data.
type CIIdentity struct {
	MoleculeID    string `json:"moleculeId"`
	PreviewDigest string `json:"previewDigest"`
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	URL           string `json:"url"`
	Commit        string `json:"commit"`
}
type CIObservation struct {
	SchemaVersion string          `json:"schemaVersion"`
	Sequence      int             `json:"sequence"`
	Previous      string          `json:"previous,omitempty"`
	SHA256        string          `json:"sha256"`
	Identity      CIIdentity      `json:"identity"`
	Snapshot      checks.Snapshot `json:"snapshot"`
}

func ciHash(a CIObservation) string {
	a.SHA256 = ""
	raw, _ := json.Marshal(a)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func ciReadFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("invalid or oversized CI evidence file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("CI evidence exceeds bound")
	}
	return raw, nil
}
func ciIdentity(o Options) (CIIdentity, error) {
	var id CIIdentity
	if !safeID.MatchString(o.MoleculeID) {
		return id, fmt.Errorf("invalid Molecule id")
	}
	raw, err := ciReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"), 8_000_000)
	if err != nil {
		return id, err
	}
	var r Receipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return id, err
	}
	raw, err = ciReadFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"), 8_000_000)
	if err != nil {
		return id, err
	}
	var record molecule.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return id, err
	}
	if record.ID != o.MoleculeID || !MatchesPublishedEvidence(r, record) {
		return id, fmt.Errorf("CI requires a matching published delivery receipt and ledger")
	}
	number, err := strconv.Atoi(r.URL[len("https://github.com/"+r.Preview.Repo+"/pull/"):])
	if err != nil {
		return id, err
	}
	return CIIdentity{o.MoleculeID, r.Preview.Digest, r.Preview.Repo, number, r.URL, r.Commit}, nil
}
func ciValidate(id CIIdentity, s checks.Snapshot) error {
	if s.Repo != id.Repo || s.PR != id.PR || s.Commit != id.Commit {
		return fmt.Errorf("CI repository, PR or commit differs from delivery")
	}
	return checks.Validate(s)
}
func ciDir(o Options, create bool) (string, error) {
	dir := o.LabDir
	for _, part := range []string{"deliveries", o.MoleculeID, "ci"} {
		dir = filepath.Join(dir, part)
		if create {
			if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
				return "", err
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("invalid CI directory")
		}

		if create {
			parent, err := os.Open(filepath.Dir(dir))
			if err != nil {
				return "", err
			}
			err = parent.Sync()
			closeErr := parent.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
		}
	}
	return dir, nil
}

// ReadCI validates every link; it never selects an older success as current evidence.
// Missing history is legacy unknown. Oversized or inconsistent history is an error.
func ReadCI(o Options) ([]CIObservation, error) {
	if !safeID.MatchString(o.MoleculeID) {
		return nil, fmt.Errorf("invalid Molecule id")
	}
	dir, err := ciDir(o, false)
	if os.IsNotExist(err) {
		return []CIObservation{}, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := ciIdentity(o)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(MaxCIObservations + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > MaxCIObservations {
		return nil, fmt.Errorf("CI history count exceeds bound")
	}
	sort.Strings(names)
	history := []CIObservation{}
	previous := ""
	total := 0
	for i, name := range names {
		raw, err := ciReadFile(filepath.Join(dir, name), maxCIRecordBytes)
		if err != nil {
			return nil, err
		}
		total += len(raw)
		if total > MaxCIHistoryBytes {
			return nil, fmt.Errorf("CI history bytes exceed bound")
		}
		var a CIObservation
		if err = json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		if a.SchemaVersion != "v1" || a.Sequence != i+1 || a.Previous != previous || a.Identity != id || a.SHA256 != ciHash(a) || name != fmt.Sprintf("%06d-%s.json", a.Sequence, a.SHA256) {
			return nil, fmt.Errorf("invalid CI history identity or hash link")
		}
		if err = ciValidate(id, a.Snapshot); err != nil {
			return nil, err
		}
		history = append(history, a)
		previous = a.SHA256
	}
	return history, nil
}

// AppendCI serializes with publication and publishes one complete file atomically.
// Existing observations, execution ledgers and delivery receipts are never rewritten.
func AppendCI(o Options, s checks.Snapshot) (CIObservation, error) {
	var a CIObservation
	if !safeID.MatchString(o.MoleculeID) {
		return a, fmt.Errorf("invalid Molecule id")
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return a, fmt.Errorf("delivery/CI append conflict: %w", err)
	}
	defer os.Remove(lock)
	return appendCI(o, s)
}

// CollectCI holds the publication lock across validation, remote observation and
// append. A competing collection fails before lookup, so overlapping starts cannot
// invert observation order or let a delayed success hide newer uncertainty.
func CollectCI(ctx context.Context, o Options, target checks.Options) (CIObservation, error) {
	var a CIObservation
	if !safeID.MatchString(o.MoleculeID) {
		return a, fmt.Errorf("invalid Molecule id")
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return a, fmt.Errorf("delivery/CI collection conflict: %w", err)
	}
	defer os.Remove(lock)
	if err := ValidateCITarget(o, target.Repo, target.PR, target.Commit); err != nil {
		return a, err
	}
	s, err := checks.Collect(ctx, target)
	if err != nil {
		return a, err
	}
	return appendCI(o, s)
}

func appendCI(o Options, s checks.Snapshot) (CIObservation, error) {
	var a CIObservation
	id, err := ciIdentity(o)
	if err != nil {
		return a, err
	}
	if err = ciValidate(id, s); err != nil {
		return a, err
	}
	history, err := ReadCI(o)
	if err != nil {
		return a, err
	}
	if len(history) >= MaxCIObservations {
		return a, fmt.Errorf("CI history count exceeds bound; observation not saved")
	}
	a = CIObservation{SchemaVersion: "v1", Sequence: len(history) + 1, Identity: id, Snapshot: s}
	if len(history) > 0 {
		a.Previous = history[len(history)-1].SHA256
	}
	a.SHA256 = ciHash(a)
	raw, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	raw = append(raw, '\n')
	total := len(raw)
	for _, old := range history {
		path := filepath.Join(o.LabDir, "deliveries", o.MoleculeID, "ci", fmt.Sprintf("%06d-%s.json", old.Sequence, old.SHA256))
		b, readErr := ciReadFile(path, maxCIRecordBytes)
		if readErr != nil {
			return a, readErr
		}
		total += len(b)
	}
	if len(raw) > maxCIRecordBytes || total > MaxCIHistoryBytes {
		return a, fmt.Errorf("CI history bytes exceed bound; observation not saved")
	}
	dir, err := ciDir(o, true)
	if err != nil {
		return a, err
	}
	// Temporary files live outside the history directory so readers see only complete links.
	f, err := os.CreateTemp(filepath.Join(o.LabDir, "deliveries"), ".ci-")
	if err != nil {
		return a, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return a, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return a, err
	}
	if err = f.Close(); err != nil {
		return a, err
	}
	target := filepath.Join(dir, fmt.Sprintf("%06d-%s.json", a.Sequence, a.SHA256))
	if err = os.Link(f.Name(), target); err != nil {
		return a, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return a, err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return a, err
	}
	return a, nil
}

// CIClassification describes the latest serialized observation. Regressing times
// make ordering ambiguous (including legacy overlapping lookups), hence unknown.
// CI never proves delivery lifecycle.
func CIClassification(history []CIObservation) string {
	if len(history) == 0 {
		return "unknown"
	}
	latest := history[0].Snapshot
	latestAt, err := time.Parse(time.RFC3339Nano, latest.CheckedAt)
	if err != nil {
		return "unknown"
	}
	for _, a := range history[1:] {
		at, err := time.Parse(time.RFC3339Nano, a.Snapshot.CheckedAt)
		if err != nil {
			return "unknown"
		}
		if at.Before(latestAt) {
			return "unknown"
		}
		latest = a.Snapshot
		latestAt = at
	}
	switch latest.Status {
	case "passed":
		return "passed"
	case "failed":
		return "failure"
	default:
		return "unknown"
	}
}

// ValidateCITarget checks local linkage before any GitHub lookup.
func ValidateCITarget(o Options, repo string, pr int, commit string) error {
	id, err := ciIdentity(o)
	if err != nil {
		return err
	}
	if id.Repo != repo || id.PR != pr || id.Commit != commit {
		return fmt.Errorf("CI target differs from delivery")
	}
	_, err = ReadCI(o)
	return err
}
