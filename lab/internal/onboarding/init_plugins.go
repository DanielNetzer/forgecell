package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"go.yaml.in/yaml/v4"
)

// PluginDescriber describes the forge and tracker plugins a Formula pins, once
// and read-only. The default starts them through mcpadapter; tests inject fakes.
type PluginDescriber interface {
	DescribeForge(ctx context.Context, labDir string, f formula.Formula) (contract.ForgeDescription, error)
	DescribeTracker(ctx context.Context, labDir string, f formula.Formula) (contract.TrackerDescription, error)
}

// mcpDescriber starts the pinned plugins under the Lab's own run root, as
// ForgeFor and TrackerFor do for every other operation.
type mcpDescriber struct{}

func (mcpDescriber) DescribeForge(ctx context.Context, labDir string, f formula.Formula) (contract.ForgeDescription, error) {
	fg, ok, err := mcpadapter.ForgeFor(labDir, f, mcpadapter.Options{})
	if err != nil {
		return contract.ForgeDescription{}, err
	}
	d, describes := fg.(interface {
		Describe(context.Context) (contract.ForgeDescription, error)
	})
	if !ok || !describes {
		return contract.ForgeDescription{}, errors.New("the Formula declares no plugin forge")
	}
	return d.Describe(ctx)
}

func (mcpDescriber) DescribeTracker(ctx context.Context, labDir string, f formula.Formula) (contract.TrackerDescription, error) {
	t, ok, err := mcpadapter.TrackerFor(labDir, f, mcpadapter.Options{})
	if err != nil {
		return contract.TrackerDescription{}, err
	}
	d, describes := t.(interface {
		Describe(context.Context) (contract.TrackerDescription, error)
	})
	if !ok || !describes {
		return contract.TrackerDescription{}, errors.New("the Formula declares no plugin tracker")
	}
	return d.Describe(ctx)
}

func (o InitOptions) describer() PluginDescriber {
	if o.Describer != nil {
		return o.Describer
	}
	return mcpDescriber{}
}

// Bounds of the plugin init input.
const (
	// maxTrackerConfigBytes bounds the --tracker-config file, as intake.config is.
	maxTrackerConfigBytes = mcpadapter.MaxConfigBytes
	maxPluginManifestPath = 512
	maxDefaultBranchBytes = 255
)

var (
	// pluginProject is intake.repo's owner/repo shape.
	pluginProject = regexp.MustCompile(`^([a-zA-Z0-9_.-]+)/([a-zA-Z0-9_.-]+)$`)
	// pluginBranch is a plain branch name that starts with a letter or digit;
	// plainBranch adds the segment rules a regular expression cannot say.
	pluginBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	// pluginManifestReservedRoots are the Lab directories a plugins: manifest must
	// never be read from, as formula's extension manifest rule says.
	pluginManifestReservedRoots = []string{"workspaces", "verification", "ledgers"}
)

const pluginFlags = "--forge-plugin, --project, --tracker-plugin and --tracker-config"

func (o InitOptions) pluginInitRequested() bool {
	return o.ForgePlugin != "" || o.Project != "" || o.TrackerPlugin != "" || o.TrackerConfig != ""
}

// checkPluginManifestPath applies the plugins: manifest rule: a clean
// Lab-relative path to a file, outside the Lab's reserved directories.
func checkPluginManifestPath(flag, manifest string) error {
	if manifest == "" || manifest == "." || len(manifest) > maxPluginManifestPath || !pinned.CleanRelative(manifest) {
		return fmt.Errorf("%s must be a clean Lab-relative path to the plugin manifest, at most %d bytes", flag, maxPluginManifestPath)
	}
	root, _, _ := strings.Cut(manifest, "/")
	for _, reserved := range pluginManifestReservedRoots {
		if strings.EqualFold(root, reserved) {
			return fmt.Errorf("%s must not be under %s/", flag, reserved)
		}
	}
	return nil
}

// checkPluginProject requires exactly owner/repo, intake.repo's shape. A nested
// group is a documented limit, refused with a plain reason.
func checkPluginProject(project string) error {
	if strings.Count(project, "/") > 1 {
		return fmt.Errorf("--project must be exactly owner/repo; nested groups such as group/subgroup/repo are not supported yet")
	}
	m := pluginProject.FindStringSubmatch(project)
	if m == nil || m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." {
		return fmt.Errorf("--project must be exactly owner/repo: letters, digits, '.', '_' and '-' in each of the two segments")
	}
	return nil
}

// readTrackerConfig reads the --tracker-config file: a regular file, never a
// symlink, of at most maxTrackerConfigBytes holding one JSON object without
// duplicate keys. Refusals never name the path.
func readTrackerConfig(cwd, name string) (map[string]any, error) {
	full := name
	if !filepath.IsAbs(full) {
		full = filepath.Join(cwd, full)
	}
	full = filepath.Clean(full)
	before, err := os.Lstat(full)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("--tracker-config must name a readable regular file, not a symlink")
	}
	if before.Size() > maxTrackerConfigBytes {
		return nil, fmt.Errorf("--tracker-config is larger than %d bytes", maxTrackerConfigBytes)
	}
	file, err := pinned.OpenNoFollow(full)
	if err != nil {
		return nil, errors.New("--tracker-config must name a readable regular file, not a symlink")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("--tracker-config was replaced while it was opened")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTrackerConfigBytes+1))
	if err != nil {
		return nil, errors.New("--tracker-config is unreadable")
	}
	if len(raw) > maxTrackerConfigBytes {
		return nil, fmt.Errorf("--tracker-config is larger than %d bytes", maxTrackerConfigBytes)
	}
	if err = pinned.RejectDuplicateKeys(raw); err != nil {
		var dup *pinned.DuplicateKeyError
		if errors.As(err, &dup) {
			return nil, errors.New("--tracker-config repeats a key")
		}
		return nil, errors.New("--tracker-config must hold one JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err = dec.Decode(&value); err != nil {
		return nil, errors.New("--tracker-config must hold one JSON object")
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("--tracker-config must hold one JSON object with no trailing data")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("--tracker-config must hold a JSON object")
	}
	converted, err := exactJSONNumbers(object)
	if err != nil {
		return nil, err
	}
	return converted.(map[string]any), nil
}

// exactJSONNumbers replaces each json.Number with an int64 or a finite float64,
// refusing an integer that int64 cannot hold, so no value is changed silently.
func exactJSONNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		if !strings.ContainsAny(string(v), ".eE") {
			return nil, errors.New("--tracker-config holds an integer outside the 64-bit range")
		}
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, errors.New("--tracker-config holds a number outside the 64-bit range")
		}
		return f, nil
	case []any:
		for i := range v {
			item, err := exactJSONNumbers(v[i])
			if err != nil {
				return nil, err
			}
			v[i] = item
		}
		return v, nil
	case map[string]any:
		for key, child := range v {
			item, err := exactJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			v[key] = item
		}
		return v, nil
	}
	return value, nil
}

// loadPluginPin hashes a Lab-relative manifest and loads it with plugin.Load,
// which re-reads it against that digest, refuses a symlink on the way and
// verifies every file it pins. The kind must be want. This is trust on first
// use: the pin is shown in the proposal for review.
func loadPluginPin(labDir, flag, rel, want string) (formula.PluginRef, error) {
	_, digest, err := plugin.LoadFile(filepath.Join(labDir, filepath.FromSlash(rel)))
	if err != nil {
		return formula.PluginRef{}, fmt.Errorf("%s %s: %w", flag, rel, err)
	}
	m, _, err := plugin.Load(labDir, rel, digest)
	if err != nil {
		return formula.PluginRef{}, fmt.Errorf("%s %s: %w", flag, rel, err)
	}
	if m.Kind != want {
		return formula.PluginRef{}, fmt.Errorf("%s %s is a %s plugin; it must be a %s plugin", flag, rel, m.Kind, want)
	}
	return formula.PluginRef{Name: m.Name, Manifest: rel, SHA256: digest}, nil
}

// plainBranch reports whether name is a plain branch: at most
// maxDefaultBranchBytes, starting with a letter or digit, with no empty
// segment (so no // and no trailing /), no . or .. segment and no .lock suffix.
func plainBranch(name string) bool {
	if len(name) > maxDefaultBranchBytes || !pluginBranch.MatchString(name) || strings.HasSuffix(name, ".lock") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// checkForgeDescription requires what init records from forge_describe: a valid
// identity, a plain default branch and a CI kind baseline derivation knows.
func checkForgeDescription(d contract.ForgeDescription) error {
	if why := formula.ValidForgeIdentity(d.Identity); why != "" {
		return fmt.Errorf("the forge plugin's identity %s", why)
	}
	if !plainBranch(d.DefaultBranch) {
		return errors.New("the forge plugin described no plain default branch")
	}
	switch d.CIKind {
	case gitlabCIKind, githubActionKind, ciKindNone:
		return nil
	}
	return fmt.Errorf("the forge plugin's CI kind must be %s, %s or %s; no proposal saved", gitlabCIKind, githubActionKind, ciKindNone)
}

// preparePluginInit proposes a bootstrap Formula whose forge and tracker are the
// pinned plugins. It describes each plugin once, reads the checkout without
// GitHub, derives the baseline from the forge's CI kind and saves an ordinary
// proposal, so init --review shows the exact YAML and init --approve approves it.
func preparePluginInit(ctx context.Context, o InitOptions) (out Outcome, err error) {
	if o.ForgePlugin == "" || o.Project == "" || o.TrackerPlugin == "" || o.TrackerConfig == "" {
		return out, fmt.Errorf("%s always come together; give all four", pluginFlags)
	}
	if o.Rebind != "" || o.ModelSet || o.Role != "" || o.EnableReview || o.RefreshBaseline {
		return out, fmt.Errorf("%s propose a new bootstrap Formula; do not combine them with --rebind, --model, --role, --enable-review or --refresh-baseline", pluginFlags)
	}
	if o.MetaHarness != "" || o.MetaCommand != nil {
		return out, fmt.Errorf("%s cannot be combined with --meta-harness or --meta-command; propose the meta binding as a separate reviewed change", pluginFlags)
	}
	timeoutMS, maxTimeoutMS, err := harnessBudget(o)
	if err != nil {
		return out, err
	}
	if o.LabDir == "" || !filepath.IsAbs(o.LabDir) || filepath.Clean(o.LabDir) != o.LabDir {
		return out, errors.New("the Lab directory must be a clean absolute path")
	}
	if err = checkPluginManifestPath("--forge-plugin", o.ForgePlugin); err != nil {
		return out, err
	}
	if err = checkPluginManifestPath("--tracker-plugin", o.TrackerPlugin); err != nil {
		return out, err
	}
	if err = checkPluginProject(o.Project); err != nil {
		return out, err
	}
	config, err := readTrackerConfig(o.Cwd, o.TrackerConfig)
	if err != nil {
		return out, err
	}
	active, err := formula.Inspect(o.LabDir, "")
	if err != nil {
		return out, err
	}
	if active.Formula.ID != "" {
		return out, fmt.Errorf("the Lab already has a Formula (%s); adding plugins to an existing Formula is a hand-reviewed change; use a new Lab for %s", active.Formula.ID, pluginFlags)
	}
	if pending, e := pendingProposal(o.LabDir); e != nil {
		return out, e
	} else if pending != nil {
		return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before %s", formula.ShellWord(o.LabDir), pending.ID, pluginFlags)
	}
	forgePin, err := loadPluginPin(o.LabDir, "--forge-plugin", o.ForgePlugin, "forge")
	if err != nil {
		return out, err
	}
	trackerPin, err := loadPluginPin(o.LabDir, "--tracker-plugin", o.TrackerPlugin, "tracker")
	if err != nil {
		return out, err
	}
	if forgePin.Name == trackerPin.Name {
		return out, fmt.Errorf("the forge and tracker plugins are both named %s; each pin needs its own name", forgePin.Name)
	}
	draft := formula.Formula{
		Plugins: []formula.PluginRef{forgePin, trackerPin},
		Forge:   &formula.ForgeRef{Plugin: forgePin.Name, Project: o.Project},
		Intake:  formula.Intake{Source: trackerPin.Name, Repo: o.Project, Config: config},
	}
	// The order is forge describe, then the checkout read, then tracker
	// describe: the tracker plugin starts and receives intake.config only once
	// the origin is verified to be the described project.
	//
	// Describing starts each pinned plugin under the Lab's plugin-runs/
	// directory (mcpadapter.PrepareRunRoot). A refused init may leave that
	// directory behind; it is Forgecell-owned and reused by later runs.
	describer := o.describer()
	forgeInfo, err := describer.DescribeForge(ctx, o.LabDir, draft)
	if err != nil {
		return out, fmt.Errorf("describing the forge plugin %s: %w", forgePin.Name, err)
	}
	if err = checkForgeDescription(forgeInfo); err != nil {
		return out, err
	}

	out.Repository, err = ReadRepositoryWith(ctx, o.Cwd, o.Runner, ForgeIdentity{Identity: forgeInfo.Identity, DefaultBranch: forgeInfo.DefaultBranch, CIKind: forgeInfo.CIKind})
	if err != nil {
		return out, err
	}
	out.Repository.Repo = o.Project

	trackerInfo, err := describer.DescribeTracker(ctx, o.LabDir, draft)
	if err != nil {
		return out, fmt.Errorf("describing the tracker plugin %s: %w", trackerPin.Name, err)
	}
	if _, e := regexp.Compile(trackerInfo.KeyPattern); e != nil || trackerInfo.KeyPattern == "" {
		return out, errors.New("the tracker plugin described no valid keyPattern")
	}
	meta := InspectMeta(ctx, active.Formula, out.Repository.Root, o.Runner)
	out.Meta = &meta

	out.Candidates = o.Candidates
	if out.Candidates == nil {
		out.Candidates = Discover(ctx, out.Repository.Root, o.Runner)
	}
	for i := range out.Candidates {
		out.Candidates[i] = out.Candidates[i].CompleteUnknowns()
	}
	out.Selection = Select(out.Candidates, o.Preferred, "", o.Env)
	out.Status = out.Selection.Status
	if out.Status != "ready" {
		out.Note = "No Formula proposed; resolve harness selection first."
		return out, nil
	}
	var selected Candidate
	for _, c := range out.Candidates {
		if c.ID == out.Selection.ID {
			selected = c
		}
	}
	if selected.Executable == "" || o.Launcher == "" {
		return out, fmt.Errorf("verified harness executable and Forgecell launcher are required")
	}
	command := []string{o.Launcher, "__adapter", selected.ID, selected.Executable}
	if err = requireAbsoluteNative("harness.command", command); err != nil {
		return out, err
	}

	baseline, err := bootstrapBaselineFor(ctx, out.Repository, forgeInfo.CIKind)
	if err != nil {
		return out, err
	}
	workflows := []string{}
	switch {
	case forgeInfo.CIKind == gitlabCIKind && out.Repository.GitLabCI:
		workflows = []string{gitlabCIFile}
	case forgeInfo.CIKind == githubActionKind:
		workflows = append(workflows, out.Repository.Workflows...)
	}
	repoName := strings.ToLower(strings.Split(o.Project, "/")[1])
	recipe := map[string]any{
		"schemaVersion": "v0", "kind": "formula",
		"id":     repoName + "-" + trackerPin.Name,
		"name":   o.Project + " issue workflow",
		"source": "bootstrap-assay", "createdAt": time.Now().UTC().Format(time.RFC3339Nano),
		"plugins": []map[string]any{
			{"name": forgePin.Name, "manifest": forgePin.Manifest, "sha256": forgePin.SHA256},
			{"name": trackerPin.Name, "manifest": trackerPin.Manifest, "sha256": trackerPin.SHA256},
		},
		"forge":             map[string]any{"plugin": forgePin.Name, "project": o.Project, "identity": forgeInfo.Identity},
		"intake":            map[string]any{"source": trackerPin.Name, "repo": o.Project, "config": config},
		"harness":           map[string]any{"binding": selected.ID, "command": command, "invoke": "byo", "timeoutMs": timeoutMS, "maxTimeoutMs": maxTimeoutMS},
		"lab":               map[string]any{"dir": o.LabDir, "observe": false},
		"repositoryContext": out.Repository.Compact(),
		"atoms": []map[string]any{
			{"id": "intake", "type": "intake", "source": trackerPin.Name},
			{"id": "scope", "type": "gate", "purpose": "scope", "mode": "human"},
			{"id": "harness", "type": "harness", "binding": selected.ID},
			{"id": "check", "type": "check", "workflows": workflows},
			{"id": "gate", "type": "gate", "purpose": "review", "mode": "human"},
			{"id": "ship", "type": "ship", "via": forgePin.Name},
			{"id": "document", "type": "document", "target": trackerPin.Name},
		},
	}
	out.Note = fmt.Sprintf("Bootstrap Assay is read-only. The forge plugin %s (%s, CI kind %s) and the tracker plugin %s (keys match %q) were each described once; their manifests are pinned by SHA-256 on first use, so review the pins. Proposed checks are evidence, not executed commands. Approving binds the coding harness; runs still stop at human review.", forgePin.Name, forgeInfo.Identity, forgeInfo.CIKind, trackerPin.Name, trackerInfo.KeyPattern)
	if len(baseline) > 0 {
		recipe["requiredChecks"] = baseline
		ids := make([]string, len(baseline))
		for i, c := range baseline {
			ids[i] = c.ID
		}
		source := "pull-request workflows"
		if forgeInfo.CIKind == gitlabCIKind {
			source = gitlabCIFile
		}
		out.Note += fmt.Sprintf(" Derived %d CI baseline required check(s) from %s and recorded them as reviewed requiredChecks (%s). Every plan must include them; review the exact YAML before approval.", len(baseline), source, strings.Join(ids, ", "))
	}
	encoded, err := yaml.Marshal(recipe)
	if err != nil {
		return out, err
	}
	out.Binding = &BindingReport{Command: command, Candidate: selected, Workflow: selected.WorkflowCapability()}
	p, err := saveWithEvidence(o.LabDir, string(encoded), &out.Repository, out.Binding)
	if err != nil {
		return out, err
	}
	if err = resources.WriteInitialConfig(o.LabDir); err != nil {
		return out, err
	}
	out.Status = "pending"
	out.Proposal = &p
	return out, nil
}

// readPluginForgeRepository reads the checkout for init --refresh-baseline when
// the approved Formula's forge is a plugin: it describes that forge once through
// ForgeFor, takes its CI kind, and reads the checkout with ReadRepositoryWith.
// It reports false, reading nothing, for a Formula without a forge block, so
// GitHub keeps ReadRepository.
func readPluginForgeRepository(ctx context.Context, o InitOptions) (Repository, string, bool, error) {
	active, err := formula.Inspect(o.LabDir, "")
	if err != nil || active.Formula.ID == "" || active.Formula.Forge == nil {
		return Repository{}, "", false, nil
	}
	approved, err := formula.Load(o.LabDir, "")
	if err != nil || !approved.Approved || approved.Formula.Forge == nil {
		return Repository{}, "", true, fmt.Errorf("--refresh-baseline needs a current exact approval of the active Formula; run init --lab %s to review it, then init --lab %s --approve PROPOSAL_ID", formula.ShellWord(o.LabDir), formula.ShellWord(o.LabDir))
	}
	if pending, e := pendingProposal(o.LabDir); e != nil {
		return Repository{}, "", true, e
	} else if pending != nil {
		return Repository{}, "", true, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before --refresh-baseline", formula.ShellWord(o.LabDir), pending.ID)
	}
	f := approved.Formula
	if _, _, ok := f.ForgePlugin(); !ok {
		return Repository{}, "", true, errors.New("forge.plugin names no declared plugin; no proposal saved")
	}
	info, err := o.describer().DescribeForge(ctx, o.LabDir, f)
	if err != nil {
		return Repository{}, "", true, fmt.Errorf("describing the forge plugin %s: %w", f.Forge.Plugin, err)
	}
	if err = checkForgeDescription(info); err != nil {
		return Repository{}, "", true, err
	}
	if f.Forge.Identity != "" && f.Forge.Identity != info.Identity {
		return Repository{}, "", true, fmt.Errorf("the forge plugin now describes %s, but the approved forge.identity is %s; no proposal saved", info.Identity, f.Forge.Identity)
	}
	repo, err := ReadRepositoryWith(ctx, o.Cwd, o.Runner, ForgeIdentity{Identity: info.Identity, DefaultBranch: info.DefaultBranch, CIKind: info.CIKind})
	if err != nil {
		return Repository{}, "", true, err
	}
	repo.Repo = f.Forge.Project
	return repo, info.CIKind, true, nil
}
