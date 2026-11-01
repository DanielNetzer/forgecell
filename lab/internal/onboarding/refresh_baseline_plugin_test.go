package onboarding

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// approvedPluginLab bootstraps and approves a plugin-forge Formula from a
// checkout whose .gitlab-ci.yml runs gitlabCI.
func approvedPluginLab(t *testing.T, gitlabCI string) pluginFixture {
	t.Helper()
	f := newPluginFixture(t, gitlabCI)
	out, err := Prepare(context.Background(), f.options(gitlabDescriber()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(f.lab, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	return f
}

func refreshOptions(f pluginFixture, d PluginDescriber) InitOptions {
	return InitOptions{Cwd: f.root, LabDir: f.lab, Launcher: "/installed/forgecell", Runner: f.runner, RefreshBaseline: true, Describer: d}
}

func TestRefreshBaselineRederivesAPluginForgeFromGitLabCI(t *testing.T) {
	f := approvedPluginLab(t, "test:\n  script:\n    - go test ./...\n")
	writePluginFile(t, filepath.Join(f.root, ".gitlab-ci.yml"), []byte(pluginGitLabCI))
	pluginGit(t, f.root, "commit", "-qam", "vet on merge requests")

	d := gitlabDescriber()
	out, err := Prepare(context.Background(), refreshOptions(f, d))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.Proposal == nil || *f.ghCalls != 0 {
		t.Fatalf("status %s, gh calls %d: %+v", out.Status, *f.ghCalls, out)
	}
	if len(d.forgeCalls) != 1 || d.forgeCalls[0].Forge == nil || d.forgeCalls[0].Forge.Plugin != "fake-forge" || len(d.tracks) != 0 {
		t.Fatalf("refresh must describe only the approved forge once: %d forge, %d tracker", len(d.forgeCalls), len(d.tracks))
	}
	proposed, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range proposed.RequiredChecks {
		ids = append(ids, c.ID)
		if c.Workflow != ".gitlab-ci.yml" {
			t.Fatalf("%s cites %s", c.ID, c.Workflow)
		}
	}
	if strings.Join(ids, ",") != "baseline-go-test,baseline-go-vet" {
		t.Fatalf("requiredChecks = %v", ids)
	}
	if !strings.Contains(out.Note, "derived read-only from the .gitlab-ci.yml at revision") || !strings.Contains(out.Note, "added baseline-go-vet") || !strings.Contains(out.Note, "(from .gitlab-ci.yml sha256 ") {
		t.Fatalf("note = %s", out.Note)
	}
	if err = verifyBaselineDiff(out.BeforeYAML, out.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshBaselineOfAnUnchangedGitLabCISavesNothing(t *testing.T) {
	f := approvedPluginLab(t, pluginGitLabCI)
	out, err := Prepare(context.Background(), refreshOptions(f, gitlabDescriber()))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "unchanged" || out.Proposal != nil || !strings.Contains(out.Note, "derived from the current .gitlab-ci.yml equals the approved requiredChecks (2 check(s))") {
		t.Fatalf("%s %s", out.Status, out.Note)
	}
}

func TestRefreshBaselineOfAPluginForgeRefusesDroppingEveryCheck(t *testing.T) {
	f := approvedPluginLab(t, pluginGitLabCI)
	writePluginFile(t, filepath.Join(f.root, ".gitlab-ci.yml"), []byte("test:\n  script:\n    - make test\n"))
	pluginGit(t, f.root, "commit", "-qam", "no recognized check")
	_, err := Prepare(context.Background(), refreshOptions(f, gitlabDescriber()))
	if err == nil || !strings.Contains(err.Error(), "the current .gitlab-ci.yml yields no recognized baseline check") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshBaselineOfCIKindNoneSaysThereIsNoCI(t *testing.T) {
	f := approvedPluginLab(t, pluginGitLabCI)
	d := gitlabDescriber()
	d.forge.CIKind = "none"
	_, err := Prepare(context.Background(), refreshOptions(f, d))
	if err == nil || !strings.Contains(err.Error(), "the forge plugin reports no CI (CI kind none), so there is no baseline to derive") || strings.Contains(err.Error(), "pull-request workflows") {
		t.Fatalf("err = %v", err)
	}

	f = approvedPluginLab(t, "")
	out, err := Prepare(context.Background(), refreshOptions(f, d))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "unchanged" || out.Proposal != nil || !strings.Contains(out.Note, "the forge plugin reports no CI (CI kind none), so there is no baseline to derive") || strings.Contains(out.Note, "pull-request workflows") {
		t.Fatalf("%s %s", out.Status, out.Note)
	}
}

func TestRefreshBaselineOfAPluginForgeRefusesAChangedIdentity(t *testing.T) {
	f := approvedPluginLab(t, pluginGitLabCI)
	d := gitlabDescriber()
	d.forge.Identity = "gitlab.example.com/acme/other"
	_, err := Prepare(context.Background(), refreshOptions(f, d))
	if err == nil || !strings.Contains(err.Error(), "the approved forge.identity is gitlab.example.com/acme/backend") {
		t.Fatalf("err = %v", err)
	}
	d = gitlabDescriber()
	d.forge.CIKind = "jenkins"
	if _, err = Prepare(context.Background(), refreshOptions(f, d)); err == nil || !strings.Contains(err.Error(), "CI kind must be") {
		t.Fatalf("an unknown CI kind was accepted: %v", err)
	}
}
