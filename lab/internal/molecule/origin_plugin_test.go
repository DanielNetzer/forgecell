package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const (
	nestedProject  = "welltech-com/platform/backend"
	nestedIdentity = "gitlab.com/welltech-com/platform/backend"
	nestedOrigin   = "https://gitlab.com/welltech-com/platform/backend.git"
)

// nestedIssue is the keyed Issue fixture on the nested GitLab project.
func nestedIssue() Issue {
	issue := keyedIssueFixture()
	issue.Repo = nestedProject
	return issue
}

// installPluginLab approves o's Formula with the tracker plugin jira (never
// started: the keyed reader stands in for it) and the fake forge plugin gitlab
// on project, whose identity is omitted when empty.
func installPluginLab(t *testing.T, o Options, project, identity string) formula.Formula {
	t.Helper()
	server := plugintest.Server(t)
	raw := plugintest.Manifest(t, server, "forge", "good")
	if err := os.MkdirAll(filepath.Join(o.LabDir, "plugins", "fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.LabDir, "plugins", "fake", "plugin.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	recipe, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const github = "intake: {source: github-issues, repo: acme/widgets}\n"
	if !strings.Contains(string(recipe), github) {
		t.Fatalf("fixture Formula changed:\n%s", recipe)
	}
	forge := "forge:\n  plugin: gitlab\n  project: " + project + "\n"
	if identity != "" {
		forge += "  identity: " + identity + "\n"
	}
	yaml := strings.Replace(string(recipe), github, "intake: {source: jira, repo: "+project+"}\n", 1) +
		"plugins:\n  - name: gitlab\n    manifest: plugins/fake/plugin.json\n    sha256: " + hex.EncodeToString(sum[:]) +
		"\n  - {name: jira, manifest: plugins/jira/plugin.json, sha256: " + strings.Repeat("a", 64) + "}\n" + forge
	decision := "plugin-lab-no-identity"
	if identity != "" {
		decision = "plugin-lab"
	}
	approveFixture(t, o.LabDir, yaml, decision)
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// nestedPluginFixture is a keyed run fixture on the nested project whose
// checkout origin is the matching gitlab.com remote.
func nestedPluginFixture(t *testing.T, identity string) Options {
	t.Helper()
	o, _ := keyedFixture(t, nestedIssue())
	installPluginLab(t, o, nestedProject, identity)
	testGit(t, o.SourceRoot, "remote", "set-url", "origin", nestedOrigin)
	return o
}

func noLedgers(t *testing.T, lab string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(lab, "ledgers"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("a refused run wrote ledger %s", e.Name())
		}
	}
}

func TestPluginForgeKeyedRunAndApprovalOnAGitLabOrigin(t *testing.T) {
	t.Parallel()
	o := nestedPluginFixture(t, nestedIdentity)
	r, err := RunIntakeKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("RunIntakeKeyed: %v", err)
	}
	if r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("status = %q readiness = %+v notes = %q", r.Status, r.Readiness, r.Notes)
	}
	if r.Workspace.Repo != nestedProject || r.Issue.Repo != nestedProject || r.Issue.Key != "ENG-1234" {
		t.Fatalf("workspace %+v issue %+v", r.Workspace, r.Issue)
	}
	// The start lock is one flat file under ledgers/.
	name, err := issueProjectKeyLockName(nestedProject, "ENG-1234")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(o.LabDir, "ledgers", name)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("start lock %s: %v", name, err)
	}
	if s := ReadQueueInventory(o.LabDir).SummarizeKey(nestedProject, "ENG-1234"); s.State != "recorded" || len(s.Attempts) != 1 || s.Attempts[0].ID != r.ID {
		t.Fatalf("queue summary = %+v", s)
	}

	o.Approve, o.AllowKeyedApproval = r.Readiness.Plans[0].Digest, true
	done, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if done.ID != r.ID || done.Status != "waiting" || len(done.HarnessAttempts) != 1 {
		t.Fatalf("approved %q status %q attempts %d notes %q", done.ID, done.Status, len(done.HarnessAttempts), done.Notes)
	}
}

func TestPluginForgeRunRefusesAnotherOrigin(t *testing.T) {
	t.Parallel()
	for name, origin := range map[string]string{
		"other project":       "https://gitlab.com/welltech-com/platform/other.git",
		"parent group":        "https://gitlab.com/welltech-com/platform.git",
		"other host":          "https://gitlab.example.com/welltech-com/platform/backend.git",
		"GitHub":              "https://github.com/welltech-com/platform/backend.git",
		"credentialed remote": "https://user:token@gitlab.com/welltech-com/platform/backend.git",
		"local path":          "/srv/git/backend.git",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := nestedPluginFixture(t, nestedIdentity)
			testGit(t, o.SourceRoot, "remote", "set-url", "origin", origin)
			if _, err := RunKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != "Git origin does not match the issue repository" {
				t.Fatalf("err = %v", err)
			}
			noLedgers(t, o.LabDir)
		})
	}
}

func TestPluginForgeWithoutIdentityIsRefused(t *testing.T) {
	t.Parallel()
	// A matching gitlab.com origin and a GitHub origin are both refused: a forge
	// block without an identity never falls back to the GitHub check.
	for _, origin := range []string{nestedOrigin, "https://github.com/welltech-com/platform/backend.git"} {
		o := nestedPluginFixture(t, "")
		testGit(t, o.SourceRoot, "remote", "set-url", "origin", origin)
		if _, err := RunIntakeKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != ErrNoForgeIdentity.Error() {
			t.Fatalf("%s: err = %v", origin, err)
		}
		noLedgers(t, o.LabDir)
	}
}

func TestPluginForgeApprovalRefusesAChangedOrigin(t *testing.T) {
	t.Parallel()
	const other = "https://gitlab.com/welltech-com/platform/other.git"
	// The approval reads every fetch and push URL of origin, as delivery does.
	for name, args := range map[string][]string{
		"fetch URL":        {"remote", "set-url", "origin", other},
		"push URL":         {"remote", "set-url", "--push", "origin", other},
		"second fetch URL": {"remote", "set-url", "--add", "origin", other},
		"second push URL":  {"remote", "set-url", "--add", "--push", "origin", other},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := nestedPluginFixture(t, nestedIdentity)
			r, err := RunKeyed(context.Background(), o, "ENG-1234")
			if err != nil || r.Readiness == nil {
				t.Fatalf("RunKeyed: %+v, %v", r.Readiness, err)
			}
			testGit(t, o.SourceRoot, args...)
			o.Approve, o.AllowKeyedApproval = r.Readiness.Plans[0].Digest, true
			if _, err := RunKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != "repository origin changed" {
				t.Fatalf("approve: %v", err)
			}
			saved, err := ReadRecord(o.LabDir, r.ID)
			if err != nil || len(saved.HarnessAttempts) != 0 {
				t.Fatalf("coding started: %d attempts, %v", len(saved.HarnessAttempts), err)
			}
		})
	}
}

func TestOriginURLsHaveIdentity(t *testing.T) {
	t.Parallel()
	o := nestedPluginFixture(t, nestedIdentity)
	ctx := context.Background()
	if !originURLsHaveIdentity(ctx, o.SourceRoot, nestedIdentity) {
		t.Fatal("the matching origin was refused")
	}
	// A push URL of the same project in another remote form still matches.
	testGit(t, o.SourceRoot, "remote", "set-url", "--push", "origin", "git@gitlab.com:welltech-com/platform/backend.git")
	if !originURLsHaveIdentity(ctx, o.SourceRoot, nestedIdentity) {
		t.Fatal("a matching push URL was refused")
	}
	testGit(t, o.SourceRoot, "remote", "set-url", "--add", "--push", "origin", "https://gitlab.com/welltech-com/platform/other.git")
	if originURLsHaveIdentity(ctx, o.SourceRoot, nestedIdentity) {
		t.Fatal("a second, mismatched push URL was accepted")
	}
	if originURLsHaveIdentity(ctx, t.TempDir(), nestedIdentity) {
		t.Fatal("a directory without an origin was accepted")
	}
}

func TestPluginForgeApprovalWithoutIdentityIsRefused(t *testing.T) {
	t.Parallel()
	o := nestedPluginFixture(t, nestedIdentity)
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil || r.Readiness == nil {
		t.Fatalf("RunKeyed: %+v, %v", r.Readiness, err)
	}
	// The Lab now approves the same Formula without forge.identity, and the
	// pending Molecule's snapshot is swapped to it. The approval is refused and
	// never codes: the plan still binds the old Formula hash, so
	// resolveExecutionRoles stops it before the origin check, which refuses such a
	// Formula with ErrNoForgeIdentity (TestForgeOriginIdentity).
	without := strings.Replace(r.FormulaSnapshot.YAML, "  identity: "+nestedIdentity+"\n", "", 1)
	if without == r.FormulaSnapshot.YAML {
		t.Fatalf("snapshot has no identity line:\n%s", without)
	}
	approveFixture(t, o.LabDir, without, "plugin-lab-no-identity")
	f, err := formula.Parse([]byte(without))
	if err != nil {
		t.Fatal(err)
	}
	r.FormulaSnapshot = Snapshot{YAML: f.YAML, SHA256: f.SHA256}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	o.Approve, o.AllowKeyedApproval = r.Readiness.Plans[0].Digest, true
	if _, err := RunKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != "plan Formula hash does not match exact snapshot" {
		t.Fatalf("an approval under a Formula without forge.identity: err = %v", err)
	}
	if saved, err := ReadRecord(o.LabDir, r.ID); err != nil || len(saved.HarnessAttempts) != 0 {
		t.Fatalf("coding started: %v", err)
	}
}

func TestGitHubFormulaKeepsTheGitHubOriginCheck(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	if r, err := Run(context.Background(), o); err != nil || r.Readiness == nil {
		t.Fatalf("a GitHub origin was refused: %v", err)
	}
	for _, origin := range []string{"https://gitlab.com/acme/widgets.git", nestedOrigin} {
		o, _ := fixture(t)
		testGit(t, o.SourceRoot, "remote", "set-url", "origin", origin)
		if _, err := Run(context.Background(), o); err == nil || err.Error() != "Git origin does not match the issue repository" {
			t.Fatalf("%s: err = %v", origin, err)
		}
	}
}

func TestForgeOriginIdentity(t *testing.T) {
	if id, err := ForgeOriginIdentity(formula.Formula{}); id != "" || err != nil {
		t.Fatalf("GitHub Formula: %q, %v", id, err)
	}
	if id, err := ForgeOriginIdentity(formula.Formula{Forge: &formula.ForgeRef{Plugin: "gitlab", Project: nestedProject}}); id != "" || err != ErrNoForgeIdentity {
		t.Fatalf("no identity: %q, %v", id, err)
	}
	if id, err := ForgeOriginIdentity(formula.Formula{Forge: &formula.ForgeRef{Plugin: "gitlab", Project: nestedProject, Identity: nestedIdentity}}); id != nestedIdentity || err != nil {
		t.Fatalf("identity: %q, %v", id, err)
	}
	for remote, want := range map[string]bool{
		nestedOrigin: true,
		"git@gitlab.com:Welltech-Com/Platform/Backend.git":     true,
		"ssh://git@gitlab.com/welltech-com/platform/backend":   true,
		"https://gitlab.com/welltech-com/platform/backend/x":   false,
		"https://gitlab.com/welltech-com/backend.git":          false,
		"https://gitlab.com:443/welltech-com/platform/backend": false,
		"": false,
	} {
		if got := OriginHasIdentity(remote, nestedIdentity); got != want {
			t.Errorf("OriginHasIdentity(%q) = %t", remote, got)
		}
	}
	if OriginHasIdentity(nestedOrigin, "") {
		t.Error("an empty identity matched")
	}
}

func TestNestedProjectAllowedGatesBindKeyProject(t *testing.T) {
	plugins := []formula.PluginRef{{Name: "gitlab"}, {Name: "jira"}}
	both := formula.Formula{Plugins: plugins, Forge: &formula.ForgeRef{Plugin: "gitlab", Project: nestedProject}, Intake: formula.Intake{Source: "jira", Repo: "Welltech-Com/Platform/Backend"}}
	if !both.NestedProjectAllowed() {
		t.Fatal("a plugin tracker and forge on the same project were refused")
	}
	if got, err := intake.BindKeyProject(both.Intake.Repo, both.NestedProjectAllowed()); err != nil || got != both.Intake.Repo {
		t.Fatalf("BindKeyProject = %q, %v", got, err)
	}
	githubTracker := both
	githubTracker.Intake.Source = formula.DefaultIntakeSource
	githubForge := both
	githubForge.Forge = nil
	undeclaredForge := both
	undeclaredForge.Forge = &formula.ForgeRef{Plugin: "other", Project: nestedProject}
	otherProject := both
	otherProject.Forge = &formula.ForgeRef{Plugin: "gitlab", Project: "welltech-com/platform/frontend"}
	for name, f := range map[string]formula.Formula{"GitHub tracker": githubTracker, "GitHub forge": githubForge, "undeclared forge": undeclaredForge, "other project": otherProject} {
		if f.NestedProjectAllowed() {
			t.Errorf("%s: nested allowed", name)
		}
		if got, err := intake.BindKeyProject(f.Intake.Repo, f.NestedProjectAllowed()); err == nil {
			t.Errorf("%s: nested bound as %q", name, got)
		}
	}
}

func TestIssueProjectKeyLockNames(t *testing.T) {
	// Two-segment projects keep exactly the keyed lock name.
	for _, repo := range []string{"Acme/Widgets", "acme/widgets", "acme/other", "a.b/c_d", "welltech-com/backend"} {
		for _, key := range []string{"ENG-1", "eng_1", "ENG-1234"} {
			want, wantErr := issueKeyLockName(repo, key)
			got, err := issueProjectKeyLockName(repo, key)
			if got != want || err != nil || wantErr != nil {
				t.Fatalf("issueProjectKeyLockName(%q, %q) = %q, %v; issueKeyLockName = %q, %v", repo, key, got, err, want, wantErr)
			}
		}
	}
	// A nested name hashes the same preimage over the full lower-cased path.
	nested, err := issueProjectKeyLockName("Welltech-Com/Platform/Backend", "ENG-1")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("forgecell keyed issue lock\x00" + nestedProject + "\x00ENG-1"))
	if want := "issue-" + hex.EncodeToString(sum[:16]) + "-eng-1" + loopLockSuffix; nested != want {
		t.Fatalf("nested lock = %q, want %q", nested, want)
	}
	if !regexp.MustCompile(`^issue-[0-9a-f]{32}-eng-1\.loop-lock$`).MatchString(nested) || !isLoopLock(nested) || strings.ContainsAny(nested, `/\`) {
		t.Fatalf("nested lock is not one flat lock file: %q", nested)
	}
	if again, _ := issueProjectKeyLockName(nestedProject, "ENG-1"); again != nested {
		t.Fatalf("nested lock is case-sensitive: %q, %q", again, nested)
	}
	seen := map[string]string{}
	for _, repo := range []string{nestedProject, "welltech-com/platform-backend", "welltech-com/platform/backend/api", "welltech-com-platform/backend", "a/b/c", "a/b-c", "a-b/c", "a/bc"} {
		for _, key := range []string{"ENG-1", "eng_1"} {
			name, err := issueProjectKeyLockName(repo, key)
			if err != nil {
				t.Fatalf("%s %s: %v", repo, key, err)
			}
			if other, ok := seen[name]; ok {
				t.Fatalf("%s %s and %s share lock %q", repo, key, other, name)
			}
			seen[name] = repo + " " + key
		}
	}
	for _, c := range []struct{ repo, key string }{
		{"a//b", "ENG-1"}, {"a/../b", "ENG-1"}, {"a/b/c.git", "ENG-1"}, {"/a/b", "ENG-1"}, {"a/b/", "ENG-1"},
		{strings.Repeat("g/", 22) + "g", "ENG-1"}, {nestedProject, "###"}, {nestedProject, ""}, {"acme", "ENG-1"}, {"../x/y", "ENG-1"},
	} {
		if name, err := issueProjectKeyLockName(c.repo, c.key); err == nil {
			t.Errorf("issueProjectKeyLockName(%q, %q) = %q", c.repo, c.key, name)
		}
	}
}
