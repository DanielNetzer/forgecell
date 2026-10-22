package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/install"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// goCacheWarnBytes is the GOCACHE size above which doctor warns. Forgecell never runs
// go clean -cache itself.
const goCacheWarnBytes = 20 * resources.GiB

// labTreeRetention is how long a finished Molecule's verification and workspace trees
// are kept before doctor --clean offers to remove them.
const labTreeRetention = 14 * 24 * time.Hour

// sizeBudget bounds each tree walk doctor makes, so a huge cache never stalls it.
const sizeBudget = 3 * time.Second

type resourceVolume struct {
	Path      string `json:"path"`
	Free      string `json:"free"`
	Total     string `json:"total"`
	SoftFloor string `json:"softFloor"`
	HardFloor string `json:"hardFloor"`
	Status    string `json:"status"`
}

type resourceSize struct {
	Path     string `json:"path"`
	Size     string `json:"size"`
	Bytes    uint64 `json:"bytes"`
	Complete bool   `json:"complete"`
}

// doctorResources is doctor's Resources section. It is read-only.
type doctorResources struct {
	Status         string           `json:"status"`
	Reason         string           `json:"reason,omitempty"`
	FloorsError    string           `json:"floorsError,omitempty"`
	Volumes        []resourceVolume `json:"volumes"`
	Memory         string           `json:"availableMemory"`
	MemorySoft     string           `json:"memorySoftFloor"`
	MemoryHard     string           `json:"memoryHardFloor"`
	Lab            resourceSize     `json:"lab"`
	Releases       int              `json:"releases"`
	ReleasesSize   string           `json:"releasesSize"`
	ReleasesPrune  int              `json:"releasesPrunable"`
	ReleasesError  string           `json:"releasesError,omitempty"`
	GoCache        *resourceSize    `json:"goCache,omitempty"`
	Warnings       []string         `json:"warnings,omitempty"`
	StaleTempDirs  int              `json:"staleTempDirs"`
	StaleTempError string           `json:"staleTempError,omitempty"`
}

// goCacheSizes memoizes the GOCACHE walk per process: it is the slow one, and the
// cache's size does not matter to the second.
var goCacheSizes sync.Map

func goCacheSize(dir string) resourceSize {
	if v, ok := goCacheSizes.Load(dir); ok {
		return v.(resourceSize)
	}
	bytes, complete := resources.TreeSize(dir, sizeBudget)
	s := resourceSize{Path: dir, Size: sizeText(bytes, complete), Bytes: bytes, Complete: complete}
	goCacheSizes.Store(dir, s)
	return s
}

func sizeText(bytes uint64, complete bool) string {
	if complete {
		return resources.Format(bytes)
	}
	return "at least " + resources.Format(bytes)
}

func statusText(l resources.Level) string {
	switch l {
	case resources.BelowHard:
		return "below hard floor"
	case resources.BelowSoft:
		return "below soft floor"
	}
	return "ok"
}

// doctorSections memoizes the Resources section per Lab and home for the process: doctor
// is one call per process, and repeated in-process calls (as with --json and the
// default output) then report the same readings rather than drifting samples.
var doctorSections sync.Map

// inspectResources reads free disk and memory against the Lab's floors, the Lab,
// release and GOCACHE sizes and the stale temp-dir count. It never changes anything.
func inspectResources(labDir, home string) doctorResources {
	key := labDir + "\x00" + home
	if v, ok := doctorSections.Load(key); ok {
		return v.(doctorResources)
	}
	d := sampleResources(labDir, home)
	doctorSections.Store(key, d)
	return d
}

func sampleResources(labDir, home string) doctorResources {
	floors, err := resources.LoadFloors(labDir)
	d := doctorResources{Volumes: []resourceVolume{}}
	if err != nil {
		d.FloorsError = flatten(err.Error())
	}
	reading := resources.SystemReader(labDir).Read()
	level, reason := floors.Evaluate(reading)
	d.Status, d.Reason = statusText(level), reason
	for _, v := range reading.Volumes {
		soft, hard := floors.DiskFloors(v.Total)
		status := resources.OK
		if v.Free < hard {
			status = resources.BelowHard
		} else if v.Free < soft {
			status = resources.BelowSoft
		}
		d.Volumes = append(d.Volumes, resourceVolume{Path: v.Path, Free: resources.Format(v.Free), Total: resources.Format(v.Total), SoftFloor: resources.Format(soft), HardFloor: resources.Format(hard), Status: statusText(status)})
	}
	d.Memory = "unknown"
	if reading.MemoryKnown {
		d.Memory = resources.Format(reading.Memory)
	}
	d.MemorySoft, d.MemoryHard = resources.Format(floors.MemorySoft), resources.Format(floors.MemoryHard)
	labBytes, labComplete := resources.TreeSize(labDir, sizeBudget)
	d.Lab = resourceSize{Path: labDir, Size: sizeText(labBytes, labComplete), Bytes: labBytes, Complete: labComplete}
	releases, err := install.InspectReleases(home, releaseLabs(labDir)...)
	if err != nil {
		d.ReleasesError = flatten(err.Error())
	}
	total := uint64(0)
	for _, r := range releases.Releases {
		total += r.Size
	}
	d.Releases, d.ReleasesSize, d.ReleasesPrune = len(releases.Releases), resources.Format(total), len(releases.Prune)
	if cache := resources.GoCacheDir(); cache != "" {
		s := goCacheSize(cache)
		d.GoCache = &s
		if s.Bytes > goCacheWarnBytes {
			d.Warnings = append(d.Warnings, fmt.Sprintf("GOCACHE is %s (above %s); run `go clean -cache` to reclaim it. Forgecell never runs it for you.", s.Size, resources.Format(goCacheWarnBytes)))
		}
	}
	if len(releases.Prune) > 0 {
		d.Warnings = append(d.Warnings, fmt.Sprintf("%d staged release(s) beyond the active one and the %d newest; `forgecell doctor --clean` lists and removes them.", len(releases.Prune), install.KeepReleases))
	}
	stale, err := resources.StaleTempDirs(os.TempDir(), time.Now())
	if err != nil {
		d.StaleTempError = flatten(err.Error())
	}
	d.StaleTempDirs = len(stale)
	return d
}

func renderResources(out io.Writer, d doctorResources) {
	fmt.Fprintf(out, "\nResources: %s\n", d.Status)
	if d.Reason != "" {
		fmt.Fprintf(out, "  %s\n", d.Reason)
	}
	if d.FloorsError != "" {
		fmt.Fprintf(out, "  Floors: defaults used; %s\n", d.FloorsError)
	}
	for _, v := range d.Volumes {
		fmt.Fprintf(out, "  Disk %s: %s free of %s (soft floor %s, hard floor %s) — %s\n", v.Path, v.Free, v.Total, v.SoftFloor, v.HardFloor, v.Status)
	}
	fmt.Fprintf(out, "  Memory: %s available (soft floor %s, hard floor %s)\n", d.Memory, d.MemorySoft, d.MemoryHard)
	fmt.Fprintf(out, "  Lab: %s (%s)\n", d.Lab.Size, d.Lab.Path)
	fmt.Fprintf(out, "  Releases: %d staged, %s; %d prunable\n", d.Releases, d.ReleasesSize, d.ReleasesPrune)
	if d.ReleasesError != "" {
		fmt.Fprintf(out, "  Releases error: %s\n", d.ReleasesError)
	}
	if d.GoCache != nil {
		fmt.Fprintf(out, "  GOCACHE: %s (%s)\n", d.GoCache.Size, d.GoCache.Path)
	}
	fmt.Fprintf(out, "  Stale temp directories: %d\n", d.StaleTempDirs)
	if d.StaleTempError != "" {
		fmt.Fprintf(out, "  Stale temp error: %s\n", d.StaleTempError)
	}
	for _, w := range d.Warnings {
		fmt.Fprintf(out, "  Warning: %s\n", w)
	}
}

// labTree is a finished Molecule's verification or workspace tree doctor --clean may
// remove.
type labTree struct {
	Path     string
	Molecule string
	Status   string
	Kind     string
	Size     uint64
}

// finishedStatuses are the Molecule statuses no later step continues from without a
// new amendment. A waiting, running or interrupted Molecule's trees are never offered:
// an interrupted one is still recoverable.
var finishedStatuses = map[string]bool{"blocked": true, "failed": true, "completed": true, "recorded": true}

// amendableStatuses are the finished statuses amend or recover can still continue
// from, which removing the trees makes impossible.
var amendableStatuses = map[string]bool{"blocked": true, "failed": true}

type ledgerSummary struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	FinishedAt string `json:"finishedAt"`
	Workspace  struct {
		Path string `json:"path"`
	} `json:"workspace"`
	Readiness struct {
		Phase string `json:"phase"`
	} `json:"readiness"`
}

// prunable reports whether the Molecule's trees may be offered: it finished more than
// the retention period before now, is not interrupted and has no pending recovery
// proposal.
func (s ledgerSummary) prunable(labDir string, now time.Time) bool {
	finished, err := time.Parse(time.RFC3339Nano, s.FinishedAt)
	if !finishedStatuses[s.Status] || err != nil || now.Sub(finished) <= labTreeRetention {
		return false
	}
	if s.Readiness.Phase == "interrupted" {
		return false
	}
	proposal, err := molecule.HasRecoveryProposal(labDir, s.ID)
	return err == nil && !proposal
}

// within reports whether path is a direct child of dir.
func within(dir, path string) bool {
	return filepath.Dir(filepath.Clean(path)) == filepath.Clean(dir) && filepath.Base(path) != "." && filepath.Base(path) != ".."
}

// releaseLabs is the Lab doctor was pointed at and, when present, the legacy checkout
// Lab at <checkout>/.forgecell: the Labs release retention scans for references beside
// those under the Forgecell home's labs directory.
func releaseLabs(labDir string) []string {
	labs := []string{labDir}
	if cwd, err := os.Getwd(); err == nil {
		legacy := filepath.Join(checkoutRoot(cwd), ".forgecell")
		if info, err := os.Stat(legacy); err == nil && info.IsDir() {
			labs = append(labs, legacy)
		}
	}
	return labs
}

// readLedgerSummaries reads every well-formed ledger in the Lab. A missing ledgers
// directory has none.
func readLedgerSummaries(labDir string) ([]ledgerSummary, error) {
	entries, err := os.ReadDir(filepath.Join(labDir, "ledgers"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ledgers []ledgerSummary
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > 5_000_000 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(labDir, "ledgers", e.Name()))
		if err != nil {
			continue
		}
		var s ledgerSummary
		if json.Unmarshal(raw, &s) != nil || s.ID == "" || s.ID+".json" != e.Name() || !safeID.MatchString(s.ID) {
			continue
		}
		ledgers = append(ledgers, s)
	}
	return ledgers, nil
}

// verificationTreeOf matches a Molecule's verification tree names: <id>-r<N> and the
// protected-regression view <id>-r<N>-protected.
func verificationTreeOf(id string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `-r[0-9]+(-protected)?$`)
}

// labTreesOf lists the verification trees and the workspace of every ledger that is
// prunable at now. A workspace any other ledger still references is kept. sized measures
// each tree.
func labTreesOf(labDir string, ledgers []ledgerSummary, now time.Time, sized bool) []labTree {
	var old []ledgerSummary
	keep := map[string]bool{}
	for _, s := range ledgers {
		if s.prunable(labDir, now) {
			old = append(old, s)
			continue
		}
		if s.Workspace.Path != "" {
			keep[filepath.Clean(s.Workspace.Path)] = true
		}
	}
	trees := []labTree{}
	seen := map[string]bool{}
	add := func(path string, s ledgerSummary, kind string) {
		path = filepath.Clean(path)
		if seen[path] || keep[path] {
			return
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return
		}
		seen[path] = true
		size := uint64(0)
		if sized {
			size, _ = resources.TreeSize(path, sizeBudget)
		}
		trees = append(trees, labTree{Path: path, Molecule: s.ID, Status: s.Status, Kind: kind, Size: size})
	}
	verifications, _ := os.ReadDir(filepath.Join(labDir, "verification"))
	for _, s := range old {
		pattern := verificationTreeOf(s.ID)
		for _, v := range verifications {
			if pattern.MatchString(v.Name()) {
				add(filepath.Join(labDir, "verification", v.Name()), s, "verification")
			}
		}
		if s.Workspace.Path != "" && within(filepath.Join(labDir, "workspaces"), s.Workspace.Path) {
			add(s.Workspace.Path, s, "workspace")
		}
	}
	sort.Slice(trees, func(i, j int) bool { return trees[i].Path < trees[j].Path })
	return trees
}

// staleLabTrees lists the verification trees (<lab>/verification/<id>-r<N>, and their
// -protected views) and the workspace (<lab>/workspaces/<name>) of every Molecule that
// finished more than 14 days before now. A tree any other Molecule still references is
// kept. Ledgers, history and evidence are never listed.
func staleLabTrees(labDir string, now time.Time) ([]labTree, error) {
	ledgers, err := readLedgerSummaries(labDir)
	if err != nil {
		return nil, err
	}
	return labTreesOf(labDir, ledgers, now, true), nil
}

// revalidate re-reads the Lab's ledgers, under the Molecule's lock, and reports why the
// listed trees of one Molecule must now be kept, or "" when they may still go: its
// ledger must be unchanged in status and still prunable, and the same trees, at the same
// paths and referenced by no other ledger, must be chosen again.
func revalidate(labDir string, listed []labTree, now time.Time) (string, error) {
	id := listed[0].Molecule
	ledgers, err := readLedgerSummaries(labDir)
	if err != nil {
		return "", err
	}
	var current *ledgerSummary
	for i := range ledgers {
		if ledgers[i].ID == id {
			current = &ledgers[i]
		}
	}
	if current == nil {
		return "its ledger can no longer be read", nil
	}
	proposal, err := molecule.HasRecoveryProposal(labDir, id)
	if err != nil {
		return "", fmt.Errorf("recovery proposal of %s: %w", id, err)
	}
	if proposal {
		return "it has a pending recovery proposal", nil
	}
	if current.Status != listed[0].Status {
		return fmt.Sprintf("its status changed from %s to %s since the listing", listed[0].Status, current.Status), nil
	}
	if !current.prunable(labDir, now) {
		return "it is no longer a finished Molecule past retention", nil
	}
	fresh := map[string]labTree{}
	for _, t := range labTreesOf(labDir, ledgers, now, false) {
		fresh[t.Path] = t
	}
	for _, t := range listed {
		if f, ok := fresh[t.Path]; !ok || f.Molecule != id || f.Kind != t.Kind {
			return fmt.Sprintf("its %s tree %s changed or is referenced elsewhere since the listing", t.Kind, t.Path), nil
		}
	}
	return "", nil
}

// groupByMolecule splits trees, sorted by path, into one group per Molecule in order of
// first appearance.
func groupByMolecule(trees []labTree) [][]labTree {
	var groups [][]labTree
	index := map[string]int{}
	for _, t := range trees {
		i, ok := index[t.Molecule]
		if !ok {
			i = len(groups)
			index[t.Molecule] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], t)
	}
	return groups
}

// runDoctorClean lists the staged releases beyond the active one, the previous one and
// the 5 newest, and the finished Molecules' trees older than 14 days, then removes them
// only after the user confirms (or with --yes). Ledgers, history and evidence are never
// removed, and GOCACHE is never cleaned.
func runDoctorClean(labDir, home string, in io.Reader, out, stderr io.Writer, yes bool, now time.Time) int {
	labs := releaseLabs(labDir)
	releases, err := install.InspectReleases(home, labs...)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: releases:", err)
		return 1
	}
	trees, err := staleLabTrees(labDir, now)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: Lab trees:", err)
		return 1
	}
	total := uint64(0)
	fmt.Fprintf(out, "Staged releases: %d; keeping the active one, the previous one, the %d newest and any a Lab Formula or pending suggestion references.\n", len(releases.Releases), install.KeepReleases)
	for _, r := range releases.Releases {
		if len(r.Kept) > 0 {
			fmt.Fprintf(out, "  keep release %s (%s)\n", r.Version, strings.Join(r.Kept, ", "))
		}
	}
	for _, r := range releases.Prune {
		total += r.Size
		fmt.Fprintf(out, "  remove release %s (%s, staged %s)\n", r.Path, resources.Format(r.Size), r.StagedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(out, "Finished Molecule trees older than %d days (interrupted Molecules and those with a recovery proposal are kept):\n", int(labTreeRetention.Hours()/24))
	warned := map[string]bool{}
	for _, t := range trees {
		total += t.Size
		fmt.Fprintf(out, "  remove %s tree of %s: %s (%s)\n", t.Kind, t.Molecule, t.Path, resources.Format(t.Size))
		if amendableStatuses[t.Status] && !warned[t.Molecule] {
			warned[t.Molecule] = true
			fmt.Fprintf(out, "    warning: %s is %s; after removal it can no longer be amended or recovered.\n", t.Molecule, t.Status)
		}
	}
	count := len(releases.Prune) + len(trees)
	if count == 0 {
		fmt.Fprintln(out, "Nothing to remove.")
		return 0
	}
	fmt.Fprintf(out, "Ledgers, history and evidence are kept. GOCACHE is never cleaned; use `go clean -cache` yourself.\n")
	if !yes {
		fmt.Fprintf(out, "Remove %d item(s), %s? [y/N] ", count, resources.Format(total))
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "\nNothing removed.")
			return 0
		}
	}
	removed, err := install.PruneReleases(home, releases.Prune, labs...)
	var problems []error
	if err != nil {
		problems = append(problems, err)
	}
	removedTrees := 0
	for _, group := range groupByMolecule(trees) {
		id := group[0].Molecule
		// The Molecule lock keeps a loop or command from resuming it while its trees
		// go; a Molecule another process holds is skipped.
		unlock, locked, err := molecule.TryLockForCleanup(labDir, id)
		if err != nil {
			problems = append(problems, fmt.Errorf("lock %s: %w", id, err))
			continue
		}
		if locked {
			fmt.Fprintf(out, "skipped %s: it is locked by a running loop or another command\n", id)
			continue
		}
		// Anything that changed since the listing, such as a resumed Molecule, a new
		// recovery proposal or another ledger now using the workspace, keeps the trees.
		reason, err := revalidate(labDir, group, now)
		if err != nil || reason != "" {
			unlock()
			if err != nil {
				problems = append(problems, fmt.Errorf("re-check %s: %w", id, err))
				continue
			}
			fmt.Fprintf(out, "skipped %s: %s\n", id, reason)
			continue
		}
		for _, t := range group {
			if e := resources.RemoveTree(t.Path); e != nil {
				problems = append(problems, e)
				continue
			}
			removed = append(removed, t.Path)
			removedTrees++
		}
		unlock()
	}
	for _, path := range removed {
		fmt.Fprintf(out, "removed %s\n", path)
	}
	if removedTrees > 0 {
		fmt.Fprintln(out, "Removed workspaces stay registered as Git worktrees of their source until `git worktree prune` runs there.")
	}
	if err := errors.Join(problems...); err != nil {
		fmt.Fprintln(stderr, "forgecell: clean:", flatten(err.Error()))
		return 1
	}
	return 0
}
