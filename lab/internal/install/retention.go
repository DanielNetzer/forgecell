package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// KeepReleases is how many of the newest staged releases retention keeps beside the
// active one. The previous release, which rollback restores, is kept as well, and so is
// every release a Lab Formula references by path.
const KeepReleases = 5

// maxFormulaBytes bounds each Formula file the reference scan reads.
const maxFormulaBytes = 5_000_000

// Release is one staged release directory under Home/releases. Kept lists why
// retention keeps it ("current", "previous", "newest", "referenced by Lab X"); a
// release with no reason is pruned.
type Release struct {
	Version  string    `json:"version"`
	Path     string    `json:"path"`
	Size     uint64    `json:"sizeBytes"`
	StagedAt time.Time `json:"stagedAt"`
	Active   bool      `json:"active,omitempty"`
	Previous bool      `json:"previous,omitempty"`
	Kept     []string  `json:"keptBecause,omitempty"`
}

// Releases is the staged release inventory, newest first, and the releases retention
// would prune: everything except the active release, the previous release, the
// KeepReleases newest and the releases a Lab Formula references.
type Releases struct {
	Home     string    `json:"home"`
	Releases []Release `json:"releases"`
	Prune    []Release `json:"prune"`
}

// homePaths is home, or ~/.forgecell, made absolute, and its symlink-resolved form the
// way Install records its links. It never creates the directory.
func homePaths(home string) (abs, resolved string, err error) {
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		home = filepath.Join(user, ".forgecell")
	}
	abs, err = filepath.Abs(home)
	if err != nil {
		return "", "", err
	}
	resolved = abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = r
	}
	return abs, resolved, nil
}

// absolutePath matches an absolute path token in Formula YAML or JSON text.
var absolutePath = regexp.MustCompile(`/[^\s"'` + "`" + `,\[\]{}]+`)

// releaseScan collects the staged releases Lab Formulas and pending suggestions
// reference, with a label for each referrer ("Lab X", "pending suggestion S in Lab X").
type releaseScan struct {
	abs, resolved string
	refs          map[string][]string
}

// versionOf returns the release version path names under home/releases, in either
// spelling of home, or "".
func (s releaseScan) versionOf(path string) string {
	for _, h := range []string{s.abs, s.resolved} {
		rel, err := filepath.Rel(filepath.Join(h, "releases"), path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		version, _, _ := strings.Cut(rel, string(filepath.Separator))
		if versionPattern.MatchString(version) {
			return version
		}
	}
	return ""
}

// note records every release text references. Each absolute path is matched as written
// and, symlinks resolved, as the release it leads to: both the whole path and each
// .../releases/<v> prefix in it are resolved, so a Formula naming the release through a
// symlinked home or executable still protects it.
func (s releaseScan) note(text, label string) {
	for _, token := range absolutePath.FindAllString(text, -1) {
		token = filepath.Clean(token)
		candidates := []string{token}
		if r, err := filepath.EvalSymlinks(token); err == nil {
			candidates = append(candidates, r)
		}
		parts := strings.Split(token, string(filepath.Separator))
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] != "releases" {
				continue
			}
			prefix := strings.Join(parts[:i+2], string(filepath.Separator))
			if r, err := filepath.EvalSymlinks(prefix); err == nil {
				candidates = append(candidates, r)
			}
		}
		for _, c := range candidates {
			version := s.versionOf(c)
			if version == "" || slices.Contains(s.refs[version], label) {
				continue
			}
			s.refs[version] = append(s.refs[version], label)
		}
	}
}

// readBounded reads a regular file (following a symlink) of at most maxFormulaBytes.
func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	if info.Size() > maxFormulaBytes {
		return nil, fmt.Errorf("%s is too large to scan for release references", path)
	}
	return os.ReadFile(path)
}

// scanLab scans one Lab's formulas directory and its pending suggestions' proposed
// Formula YAML. The Lab and formulas directories are followed through symlinks. A
// missing Lab, formulas or suggestions directory has nothing to scan; anything else it
// cannot read is an error.
func (s releaseScan) scanLab(dir, name string) error {
	lab, err := filepath.EvalSymlinks(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info, err := os.Stat(lab); err != nil || !info.IsDir() {
		return err
	}
	formulas, err := filepath.EvalSymlinks(filepath.Join(lab, "formulas"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		err = filepath.WalkDir(formulas, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			raw, err := readBounded(path)
			if err != nil {
				return err
			}
			s.note(string(raw), "Lab "+name)
			return nil
		})
		if err != nil {
			return fmt.Errorf("formulas: %w", err)
		}
	}
	suggestions := filepath.Join(lab, "suggestions")
	entries, err := os.ReadDir(suggestions)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("suggestions: %w", err)
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		raw, err := readBounded(filepath.Join(suggestions, e.Name()))
		if err != nil {
			return fmt.Errorf("suggestions: %w", err)
		}
		var suggestion struct {
			Status       string `json:"status"`
			ProposedYAML string `json:"proposedYaml"`
		}
		label := "pending suggestion " + id + " in Lab " + name
		if json.Unmarshal(raw, &suggestion) != nil {
			// A suggestion it cannot parse might still be pending; its text is scanned.
			s.note(string(raw), label)
			continue
		}
		if suggestion.Status == "pending" {
			s.note(suggestion.ProposedYAML, label)
		}
	}
	return nil
}

// referencedReleases scans every Lab under home/labs, and the further Labs given (the
// Lab doctor was pointed at and the legacy checkout Lab), for absolute paths into
// <home>/releases/<version>/: in their formulas directories and in the proposed Formula
// of each pending suggestion. It returns each referenced version with its referrers. A
// Lab it cannot read is an error, so retention never prunes what it could not check.
func referencedReleases(abs, resolved string, extra []string) (map[string][]string, error) {
	s := releaseScan{abs: abs, resolved: resolved, refs: map[string][]string{}}
	type lab struct{ path, name string }
	var labs []lab
	entries, err := os.ReadDir(filepath.Join(resolved, "labs"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		labs = append(labs, lab{filepath.Join(resolved, "labs", e.Name()), e.Name()})
	}
	for _, path := range extra {
		if path == "" {
			continue
		}
		if a, err := filepath.Abs(path); err == nil {
			path = a
		}
		labs = append(labs, lab{path, path})
	}
	seen := map[string]bool{}
	var problems []error
	for _, l := range labs {
		key := l.path
		if r, err := filepath.EvalSymlinks(l.path); err == nil {
			key = r
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := s.scanLab(l.path, l.name); err != nil {
			problems = append(problems, fmt.Errorf("scan Lab %s: %w", l.name, err))
		}
	}
	return s.refs, errors.Join(problems...)
}

// InspectReleases lists the staged releases under home (FORGECELL_HOME, or
// ~/.forgecell when empty), why each kept one is kept, and the ones retention would
// prune. labs are further Labs to scan for references beside those under home/labs. It
// only reads; a missing releases directory is an empty inventory.
func InspectReleases(home string, labs ...string) (Releases, error) {
	abs, home, err := homePaths(home)
	if err != nil {
		return Releases{}, err
	}
	out := Releases{Home: home, Releases: []Release{}, Prune: []Release{}}
	current, err := target(home, "current")
	if err != nil {
		return out, err
	}
	previous, err := target(home, "previous")
	if err != nil {
		return out, err
	}
	entries, err := os.ReadDir(filepath.Join(home, "releases"))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	refs, err := referencedReleases(abs, home, labs)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		path := filepath.Join(home, "releases", e.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || !versionPattern.MatchString(e.Name()) {
			continue
		}
		size, _ := resources.TreeSize(path, 0)
		out.Releases = append(out.Releases, Release{Version: e.Name(), Path: path, Size: size, StagedAt: info.ModTime().UTC(), Active: path == current, Previous: path == previous})
	}
	sort.SliceStable(out.Releases, func(i, j int) bool {
		a, b := out.Releases[i], out.Releases[j]
		if !a.StagedAt.Equal(b.StagedAt) {
			return a.StagedAt.After(b.StagedAt)
		}
		return a.Version > b.Version
	})
	for i := range out.Releases {
		r := &out.Releases[i]
		if r.Active {
			r.Kept = append(r.Kept, "current")
		}
		if r.Previous {
			r.Kept = append(r.Kept, "previous")
		}
		if i < KeepReleases {
			r.Kept = append(r.Kept, "newest")
		}
		for _, by := range refs[r.Version] {
			r.Kept = append(r.Kept, "referenced by "+by)
		}
		if len(r.Kept) == 0 {
			out.Prune = append(out.Prune, *r)
		}
	}
	return out, nil
}

// PruneReleases removes the releases InspectReleases chose to prune, under the install
// lock. It re-reads the current and previous links and re-scans the Labs (those under
// home/labs and the labs given) first and never removes a release they name, so an
// install or a new Formula between inspection and pruning cannot lose a release still
// in use. It returns the removed paths and every failure.
func PruneReleases(home string, prune []Release, labs ...string) ([]string, error) {
	abs, home, err := homePaths(home)
	if err != nil {
		return nil, err
	}
	unlock, err := lock(Options{Home: home})
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := target(home, "current")
	if err != nil {
		return nil, err
	}
	previous, err := target(home, "previous")
	if err != nil {
		return nil, err
	}
	refs, err := referencedReleases(abs, home, labs)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	var problems []error
	for _, r := range prune {
		path := filepath.Clean(r.Path)
		if filepath.Dir(path) != filepath.Join(home, "releases") || !versionPattern.MatchString(filepath.Base(path)) {
			problems = append(problems, fmt.Errorf("refusing to remove %s: not a staged release", path))
			continue
		}
		if path == current || path == previous || len(refs[filepath.Base(path)]) > 0 {
			continue
		}
		if err := resources.RemoveTree(path); err != nil {
			problems = append(problems, err)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(problems...)
}
