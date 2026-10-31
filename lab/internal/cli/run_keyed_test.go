package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// keyedRunLab is a Lab whose Formula reads Issues from the tracker plugin
// "jira" (the fake tracker in mode), a git checkout whose origin names
// acme/widgets and a fake coding harness. runKeyed is replaced by RunKeyed
// with a supplied analysis, so no provider starts; the plugin is real.
func keyedRunLab(t *testing.T, server pinned.File, mode string) (lab, repo string, started *int) {
	t.Helper()
	gsEnv(t, "")
	root := plugintest.TempDir(t)
	repo = filepath.Join(root, "repo")
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
	lab = filepath.Join(root, "lab")
	raw := plugintest.Manifest(t, server, "tracker", mode)
	gsWrite(t, filepath.Join(lab, "plugins", "jira", "plugin.json"), string(raw), 0o600)
	sum := sha256.Sum256(raw)
	command, err := json.Marshal([]string{harnessPath})
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(gsMoleculeYAML(string(command), gsScopeAtom, gsReviewAtom), "intake: {source: github-issues, repo: acme/widgets}\n",
		"intake: {source: jira, repo: acme/widgets}\nplugins:\n  - {name: jira, manifest: plugins/jira/plugin.json, sha256: "+hex.EncodeToString(sum[:])+"}\n", 1)
	gsApproveFormula(t, lab, yaml)
	t.Chdir(repo)
	started = new(int)
	previous := runKeyed
	runKeyed = func(ctx context.Context, o molecule.Options, key string) (molecule.Record, error) {
		*started++
		o.Analyze = keyedRunAnalysis
		return previous(ctx, o, key)
	}
	t.Cleanup(func() { runKeyed = previous })
	return lab, repo, started
}

func keyedRunAnalysis(_ context.Context, _ formula.Binding, _ molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
	id := ""
	for _, e := range s.Evidence {
		if e.Path == "package.json" {
			id = e.ID
		}
	}
	return readiness.Analysis{Summary: "Change code", Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}, {Path: "request.json", Reason: "Fixture harness trace", Evidence: []string{id}}}, Acceptance: []readiness.Criterion{{Description: "Code contains changed", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "check", Category: "candidate", Argv: []string{"/bin/sh", "-c", "test \"$(cat code.txt)\" = changed"}, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{id}, Reason: "Fixture acceptance", Evidence: []string{"issue", id}}}}, nil
}

func keyedLedgers(t *testing.T, lab string) []molecule.Record {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(lab, "ledgers"))
	var out []molecule.Record
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && strings.HasPrefix(e.Name(), "mol-") {
			out = append(out, gsLoadRecord(t, filepath.Join(lab, "ledgers", e.Name())))
		}
	}
	return out
}

func pluginCallTools(r molecule.Record) []string {
	var tools []string
	for _, c := range r.PluginCalls {
		tools = append(tools, c.Tool)
	}
	return tools
}

func TestRunKeyedThroughTrackerPlugin(t *testing.T) {
	usePluginDoctorSeams(t)
	server := plugintest.Server(t)
	lab, _, started := keyedRunLab(t, server, "good")

	code, out, errOut := runCLI("run", "FAKE-1", "--lab", lab, "--target", "main")
	if code != 0 || *started != 1 {
		t.Fatalf("exit %d, started %d\n%s%s", code, *started, out, errOut)
	}
	ledgers := keyedLedgers(t, lab)
	if len(ledgers) != 1 {
		t.Fatalf("ledgers = %d", len(ledgers))
	}
	r := ledgers[0]
	if !regexp.MustCompile(`^mol-fake-1-[0-9a-f]{24}$`).MatchString(r.ID) || r.Issue.Key != "FAKE-1" || r.Issue.Number != 0 || r.Issue.State != "OPEN" || r.Issue.Title != "First" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("record %s issue %+v readiness %+v notes %q", r.ID, r.Issue, r.Readiness, r.Notes)
	}
	var source string
	for _, action := range r.Atoms[0].Provenance.Actions {
		if action.AttemptID == "intake" {
			source = action.ResolvedSource
		}
	}
	if source != "jira" {
		t.Fatalf("intake resolved source = %q", source)
	}
	// The CLI's describe and the reader's describe and get are recorded, with
	// digests and no Issue content.
	if got := strings.Join(pluginCallTools(r), ","); got != "tracker_describe,tracker_describe,tracker_get" {
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
	// The keyed renderers name the key: never run 0 or repo#0.
	digest := r.Readiness.Plans[0].Digest
	if !strings.Contains(out, "acme/widgets FAKE-1 — First\n") || !strings.Contains(out, "forgecell run 'FAKE-1' --lab ") || !strings.Contains(out, "--approve "+digest+"\n") {
		t.Fatalf("run output:\n%s", out)
	}
	if strings.Contains(out, "run 0 ") || strings.Contains(out, "#0") || strings.Contains(out, plugintest.PlantedSecret) {
		t.Fatalf("run output:\n%s", out)
	}
	requireRunRootEmpty(t, lab)

	// --approve runs the same keyed Molecule.
	code, out, errOut = runCLI("run", "FAKE-1", "--lab", lab, "--target", "main", "--approve", digest)
	if code != 0 || *started != 2 {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errOut)
	}
	ledgers = keyedLedgers(t, lab)
	if len(ledgers) != 1 || ledgers[0].ID != r.ID {
		t.Fatalf("approval made another Molecule: %d", len(ledgers))
	}
	done := ledgers[0]
	if done.Readiness.Phase == "scope-waiting" || len(done.HarnessAttempts) != 1 || done.Status != "waiting" {
		t.Fatalf("approved: status %q phase %q attempts %d notes %q", done.Status, done.Readiness.Phase, len(done.HarnessAttempts), done.Notes)
	}
	if got := strings.Join(pluginCallTools(done), ","); got != "tracker_describe,tracker_describe,tracker_get,tracker_describe,tracker_describe,tracker_get" {
		t.Fatalf("plugin calls after approval = %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(done.Workspace.Path, "code.txt")); string(data) != "changed" {
		t.Fatalf("coding did not run in the keyed workspace: %q", data)
	}
	requireRunRootEmpty(t, lab)
}

func TestRunRefusesTheOtherArgumentForm(t *testing.T) {
	usePluginDoctorSeams(t)
	server := plugintest.Server(t)
	lab, _, started := keyedRunLab(t, server, "good")
	for _, c := range []struct {
		arg, want string
	}{
		{"12", "reads Issues from the tracker plugin jira, so run takes that tracker's Issue key, not a GitHub Issue number; nothing was started"},
		{"#12", "not a GitHub Issue number"},
		{"acme/widgets#12", "not a GitHub Issue number"},
		{"ENG-1", "issue key does not match the tracker's key pattern"},
	} {
		code, out, errOut := runCLI("run", c.arg, "--lab", lab, "--target", "main")
		if code != 1 || out != "" || !strings.Contains(errOut, c.want) || strings.Contains(errOut, lab) {
			t.Errorf("%s: exit %d\n%s%s", c.arg, code, out, errOut)
		}
	}
	if *started != 0 || len(keyedLedgers(t, lab)) != 0 {
		t.Fatalf("a refused argument started a Molecule: %d", *started)
	}
	requireRunRootEmpty(t, lab)

	// A plugin that cannot describe itself stops the run before any Molecule,
	// with only its fixed reason.
	broken, _, brokenStarted := keyedRunLab(t, server, "crash")
	code, out, errOut := runCLI("run", "FAKE-1", "--lab", broken, "--target", "main")
	if code != 1 || out != "" || errOut != "forgecell: plugin fake-tracker tracker_describe: failed\n" || *brokenStarted != 0 {
		t.Fatalf("crash: exit %d\n%s%s", code, out, errOut)
	}
	requireRunRootEmpty(t, broken)

	// With GitHub Issues, a tracker key is refused and nothing starts.
	github := gsSeedMolecule(t)
	before := len(keyedLedgers(t, github.lab))
	code, out, errOut = runCLI("run", "ENG-123", "--lab", github.lab)
	if code != 1 || out != "" || !strings.Contains(errOut, "the active Formula reads GitHub Issues, so run takes 123, #123, owner/repo#123 or a GitHub issue URL, not a tracker key; nothing was started") {
		t.Fatalf("github key: exit %d\n%s%s", code, out, errOut)
	}
	if after := len(keyedLedgers(t, github.lab)); after != before {
		t.Fatalf("a refused key wrote a ledger: %d -> %d", before, after)
	}
	if _, err := os.Lstat(filepath.Join(github.lab, mcpadapter.RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("the GitHub path made a plugin run root: %v", err)
	}
}

// TestAmendRunRefusesAKeyedMolecule: amend --run on a keyed Molecule is
// refused with a plain reason before it amends, never "unsupported intake
// source"; presentAmend refuses the run of a keyed amendment as well.
func TestAmendRunRefusesAKeyedMolecule(t *testing.T) {
	usePluginDoctorSeams(t)
	server := plugintest.Server(t)
	lab, _, started := keyedRunLab(t, server, "good")
	if code, out, errOut := runCLI("run", "FAKE-1", "--lab", lab, "--target", "main"); code != 0 {
		t.Fatalf("run: exit %d\n%s%s", code, out, errOut)
	}
	r := keyedLedgers(t, lab)[0]
	code, out, errOut := runCLI("amend", r.ID, "--lab", lab, "--note", "retry", "--run")
	want := "forgecell: " + errKeyedAmendRun.Error() + "\n"
	if code != 1 || out != "" || errOut != want || strings.Contains(errOut, "unsupported intake source") {
		t.Fatalf("amend --run: exit %d\n%s%s", code, out, errOut)
	}
	if after := keyedLedgers(t, lab)[0]; len(after.Readiness.Plans) != len(r.Readiness.Plans) || len(after.HarnessAttempts) != 0 || *started != 1 {
		t.Fatalf("a refused amend --run changed the Molecule: plans %d attempts %d", len(after.Readiness.Plans), len(after.HarnessAttempts))
	}

	plan := readiness.Plan{Revision: 2, Inputs: readiness.Inputs{BaseCommit: "base", TargetBranch: "main"}}
	keyed := molecule.Record{ID: "mol-eng-7-a", Status: "waiting", LabDir: "/lab", Issue: molecule.Issue{Key: "ENG-7", Repo: "acme/widgets"},
		Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{Revision: 1}, Digest: "digest-1"}, {Plan: plan, Digest: "digest-2"}}}}
	var amendOut, amendErr bytes.Buffer
	if code := presentAmend(context.Background(), &amendOut, &amendErr, filepath.Join(plugintest.TempDir(t), "lab"), keyed, true); code != 1 {
		t.Fatalf("presentAmend: exit %d\n%s%s", code, amendOut.String(), amendErr.String())
	}
	if amendErr.String() != "forgecell: the amendment is recorded, but its run did not start: "+errKeyedAmendRun.Error()+"\n" || !strings.Contains(amendOut.String(), "--approve digest-2\n") {
		t.Fatalf("presentAmend:\n%s%s", amendOut.String(), amendErr.String())
	}
}

// TestKeyedAndNumericRenderers: the approve hint and the recovery proposal
// name a keyed Molecule by its key, and a GitHub Molecule exactly as before.
func TestKeyedAndNumericRenderers(t *testing.T) {
	record := func(issue molecule.Issue) molecule.Record {
		return molecule.Record{ID: "mol-x", Status: "waiting", LabDir: "/lab", FormulaID: "sample", Issue: issue,
			Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Digest: strings.Repeat("d", 64), Plan: readiness.Plan{Inputs: readiness.Inputs{BaseCommit: "base", TargetBranch: "main"}}}}}}
	}
	var numeric, keyed bytes.Buffer
	presentRun(&numeric, record(molecule.Issue{Number: 12, Repo: "acme/widgets", Title: "T"}))
	presentRun(&keyed, record(molecule.Issue{Key: "ENG-7", Repo: "acme/widgets", Title: "T"}))
	if !strings.HasPrefix(numeric.String(), "Molecule mol-x · waiting\nacme/widgets#12 — T\n") || !strings.Contains(numeric.String(), "\nforgecell run 12 --lab '/lab' --formula 'sample' --base 'base' --target 'main' --approve "+strings.Repeat("d", 64)+"\n") {
		t.Fatalf("numeric:\n%s", numeric.String())
	}
	if !strings.HasPrefix(keyed.String(), "Molecule mol-x · waiting\nacme/widgets ENG-7 — T\n") || !strings.Contains(keyed.String(), "\nforgecell run 'ENG-7' --lab '/lab' --formula 'sample' --base 'base' --target 'main' --approve "+strings.Repeat("d", 64)+"\n") {
		t.Fatalf("keyed:\n%s", keyed.String())
	}
	if strings.Contains(keyed.String(), "run 0") || strings.Contains(keyed.String(), "#0") {
		t.Fatalf("keyed:\n%s", keyed.String())
	}

	// The approve command carries the exact key, shell-quoted, never a display
	// form; a ledger key that is not a valid key gets no command.
	var quoted, invalid bytes.Buffer
	presentRun(&quoted, record(molecule.Issue{Key: "ENG 7's", Repo: "acme/widgets", Title: "T"}))
	if !strings.Contains(quoted.String(), "\nforgecell run 'ENG 7'\\''s' --lab '/lab' ") {
		t.Fatalf("quoted:\n%s", quoted.String())
	}
	presentRun(&invalid, record(molecule.Issue{Key: "ENG-7\x1b[2J", Repo: "acme/widgets", Title: "T"}))
	if strings.Contains(invalid.String(), "forgecell run ") || strings.Contains(invalid.String(), "\x1b") || !strings.Contains(invalid.String(), "This Molecule's Issue key is not a valid tracker key, so no approve command is shown.\n") {
		t.Fatalf("invalid:\n%q", invalid.String())
	}

	var doctor bytes.Buffer
	renderDoctor(&doctor, doctorView{Lab: "/lab", Note: "note", RecoveryProposals: []molecule.RecoveryProposal{
		{MoleculeID: "mol-12-a", Repository: "acme/widgets", Issue: 12, Status: "running", Phase: "coding", Command: "forgecell recover mol-12-a"},
		{MoleculeID: "mol-eng-7-b", Repository: "acme/widgets", IssueKey: "ENG-7", Status: "running", Phase: "coding", Command: "forgecell recover mol-eng-7-b"},
	}})
	text := doctor.String()
	if !strings.Contains(text, "  acme/widgets#12 · Molecule mol-12-a · running coding\n") || !strings.Contains(text, "  acme/widgets ENG-7 · Molecule mol-eng-7-b · running coding\n") || strings.Contains(text, "#0") {
		t.Fatalf("recovery proposals:\n%s", text)
	}
}
