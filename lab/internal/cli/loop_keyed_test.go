package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// keyedLoopLab is a Lab whose Formula reads Issues from the fake tracker plugin
// "jira" and approves its scope gate by policy, with a git checkout whose
// origin names acme/widgets and a fake coding harness. runLoop is wrapped to
// supply an analysis, so no provider starts; the plugin is real.
func keyedLoopLab(t *testing.T, server pinned.File) string {
	t.Helper()
	gsEnv(t, "")
	root := plugintest.TempDir(t)
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "code.txt"), "base", 0o600)
	gsWrite(t, filepath.Join(repo, "package.json"), `{"scripts":{"test":"test x = x"}}`, 0o600)
	gsGit(t, repo, "init", "-q", "-b", "main")
	gsGit(t, repo, "config", "user.name", "Test")
	gsGit(t, repo, "config", "user.email", "test@example.test")
	gsGit(t, repo, "add", ".")
	gsGit(t, repo, "commit", "-qm", "base")
	gsGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	harnessPath := filepath.Join(root, "harness")
	gsWrite(t, harnessPath, "#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n", 0o700)
	lab := filepath.Join(root, "lab")
	raw := plugintest.Manifest(t, server, "tracker", "good")
	gsWrite(t, filepath.Join(lab, "plugins", "jira", "plugin.json"), string(raw), 0o600)
	sum := sha256.Sum256(raw)
	command, err := json.Marshal([]string{harnessPath})
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(gsMoleculeYAML(string(command), gsPolicyScope, gsReviewAtom), "intake: {source: github-issues, repo: acme/widgets}\n",
		"intake: {source: jira, repo: acme/widgets}\nplugins:\n  - {name: jira, manifest: plugins/jira/plugin.json, sha256: "+hex.EncodeToString(sum[:])+"}\n", 1)
	gsApproveFormula(t, lab, yaml)
	t.Chdir(repo)
	previous := runLoop
	runLoop = func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		o.Template.Analyze = keyedLoopAnalysis
		o.Template.TargetBranch = "main"
		return previous(ctx, o)
	}
	t.Cleanup(func() { runLoop = previous })
	return lab
}

// keyedLoopAnalysis is keyedRunAnalysis with a check that names no scope file,
// which the auto-when-ready policy requires.
func keyedLoopAnalysis(ctx context.Context, b formula.Binding, i molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
	a, err := keyedRunAnalysis(ctx, b, i, s)
	a.Checks[0].Argv = []string{"/bin/sh", "-c", "test -f package.json"}
	return a, err
}

func TestLoopWithAPluginTrackerEndToEnd(t *testing.T) {
	usePluginDoctorSeams(t)
	server := plugintest.Server(t)
	lab := keyedLoopLab(t, server)

	// The fake tracker declares only the selector "all", which reaches it raw.
	code, out, errOut := runCLI("loop", "--lab", lab, "--once", "--select", "all")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.HasPrefix(out, "Lab loop for acme/widgets · select all · max concurrent 1\nPass 1: 1 eligible Issue(s)\n") {
		t.Fatalf("loop output:\n%s", out)
	}
	if !regexp.MustCompile(`\nacme/widgets FAKE-1 · \S+ · Molecule mol-fake-1-[0-9a-f]{24} · started intake · approved by policy\n`).MatchString(out) {
		t.Fatalf("loop output:\n%s", out)
	}
	if strings.Contains(out, "#0") || strings.Contains(out, "run 0") || strings.Contains(errOut, lab) || strings.Contains(out+errOut, plugintest.PlantedSecret) {
		t.Fatalf("loop output:\n%s%s", out, errOut)
	}
	ledgers := keyedLedgers(t, lab)
	if len(ledgers) != 1 {
		t.Fatalf("ledgers = %d", len(ledgers))
	}
	r := ledgers[0]
	if r.Issue.Key != "FAKE-1" || r.Issue.Number != 0 || len(r.PolicyDecisions) != 1 || len(r.HarnessAttempts) != 1 {
		t.Fatalf("record issue %+v decisions %d attempts %d notes %q", r.Issue, len(r.PolicyDecisions), len(r.HarnessAttempts), r.Notes)
	}
	// The Molecule records its own reads, at intake and at approval; the
	// listing's calls belong to no Molecule.
	if got := strings.Join(pluginCallTools(r), ","); got != "tracker_describe,tracker_get,tracker_describe,tracker_get" {
		t.Fatalf("plugin calls = %s", got)
	}
	calls, err := json.Marshal(r.PluginCalls)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"First", "Body", "tracker.example", "FAKE-1", lab} {
		if strings.Contains(string(calls), content) {
			t.Fatalf("plugin calls carry %q: %s", content, calls)
		}
	}
	requireRunRootEmpty(t, lab)

	// A second pass starts nothing for the key.
	code, out, errOut = runCLI("loop", "--lab", lab, "--once", "--select", "all")
	if code != 0 || strings.Contains(out, "started intake") || len(keyedLedgers(t, lab)) != 1 {
		t.Fatalf("second pass: exit %d\n%s%s", code, out, errOut)
	}
}

func TestLoopWithAPluginTrackerRefusesAnUndeclaredSelector(t *testing.T) {
	usePluginDoctorSeams(t)
	server := plugintest.Server(t)
	lab := keyedLoopLab(t, server)
	// oldest is not a selector the fake declares; the plugin path refuses it, not
	// intake.ParseSelector, and nothing starts.
	code, out, errOut := runCLI("loop", "--lab", lab, "--once")
	if code != 1 || !strings.Contains(errOut, "tracker_list") || strings.Contains(errOut, "unknown selection") || strings.Contains(errOut, lab) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if len(keyedLedgers(t, lab)) != 0 {
		t.Fatal("a refused listing started a Molecule")
	}
	// A GitHub selector form is passed raw as well.
	code, _, errOut = runCLI("loop", "--lab", lab, "--once", "--select", "filter:123")
	if code != 1 || strings.Contains(errOut, "unknown selection") || len(keyedLedgers(t, lab)) != 0 {
		t.Fatalf("filter:123: exit %d\n%s", code, errOut)
	}
	requireRunRootEmpty(t, lab)
}

// TestRenderLoopKeyedLine: a keyed item gets its own line and the header names
// the raw tracker selector; the numeric line and header are unchanged.
func TestRenderLoopKeyedLine(t *testing.T) {
	var keyed bytes.Buffer
	renderLoop(&keyed, molecule.LoopReport{Repository: "acme/widgets", TrackerSelect: "filter:123", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
		{Repository: "acme/widgets", Key: "ENG-1", MoleculeID: "mol-eng-1-a", State: molecule.LoopStalled, Reason: "r"},
		{Repository: "acme/widgets", Key: "ENG-2\x1b[2J", State: molecule.LoopLocked, Reason: "held"},
	}}}}, intake.Selector{}, 2)
	text := keyed.String()
	if !strings.HasPrefix(text, "Lab loop for acme/widgets · select filter:123 · max concurrent 2\n") || !strings.Contains(text, "\nacme/widgets ENG-1 · stalled · Molecule mol-eng-1-a\n  r\n") {
		t.Fatalf("keyed:\n%s", text)
	}
	if strings.Contains(text, "#0") || strings.Contains(text, "\x1b") || !strings.Contains(text, "· locked · Molecule none\n") {
		t.Fatalf("keyed:\n%q", text)
	}
	var numeric bytes.Buffer
	renderLoop(&numeric, molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
		{Repository: "acme/widgets", Issue: 12, MoleculeID: "mol-12-a", State: molecule.LoopStalled, Reason: "r"},
	}}}}, intake.Selector{Kind: intake.SelectLabel, Value: "x"}, 1)
	if numeric.String() != "Lab loop for acme/widgets · select label:x · max concurrent 1\nPass 1: 1 eligible Issue(s)\nacme/widgets#12 · stalled · Molecule mol-12-a\n  r\n1 Issue(s) made no progress in the final pass and need attention\n" {
		t.Fatalf("numeric:\n%s", numeric.String())
	}
}
