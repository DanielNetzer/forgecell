package cli

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/ui"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// uiSetupFakes replaces the browser, git identity and the setup engine seams,
// and records the Labs each setup launch was offered. No real git, gh or
// coding agent runs.
func uiSetupFakes(t *testing.T) func() [][]string {
	t.Helper()
	previousOpen := openBrowser
	openBrowser = func(string) error { return nil }
	t.Cleanup(func() { openBrowser = previousOpen })
	previousName := gitIdentity
	gitIdentity = func(context.Context, string, ...string) (string, error) { return "Test Operator\n", nil }
	t.Cleanup(func() { gitIdentity = previousName })
	var mu sync.Mutex
	var offered [][]string
	previousSetup := uiSetupOptions
	uiSetupOptions = func(labs []string, _ io.Writer) *ui.SetupOptions {
		mu.Lock()
		defer mu.Unlock()
		offered = append(offered, slices.Clone(labs))
		return &ui.SetupOptions{}
	}
	t.Cleanup(func() { uiSetupOptions = previousSetup })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(offered)
	}
}

// With no Lab for the checkout's repository, forgecell ui serves Set up your
// Lab instead of failing, and creates nothing until an engine call does.
func TestUISetupServesSetUpYourLabWhenNoLabExists(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/acme/widgets.git")
	offered := uiSetupFakes(t)
	labs := filepath.Join(home, ".forgecell", "labs")

	_, stop := startUI(t, "--no-open")
	code, output := stop()
	if code != 0 || !strings.Contains(output, "Set up your Lab") || strings.Contains(output, "no Lab selected") {
		t.Fatalf("ui with no Lab: %d %s", code, output)
	}
	if got := offered(); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("setup offered %q", got)
	}
	if _, err := os.Lstat(labs); !os.IsNotExist(err) {
		t.Fatalf("forgecell ui wrote Lab storage before any engine call: %v", err)
	}
}

// approvedLab is a Lab for repo whose Formula was proposed and approved through
// the engine's own proposal path.
func approvedLab(t *testing.T, dir, repo string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	recipe := "kind: formula\nid: widgets-github-issue\nname: " + repo + " issue workflow\nintake: {source: github-issues, repo: " + repo + "}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms:\n  - {id: intake, type: intake, source: github-issues}\n  - {id: scope, type: gate, purpose: scope, mode: human}\n  - {id: harness, type: harness, binding: codex}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review, mode: human}\n  - {id: ship, type: ship, via: github}\n  - {id: document, type: document, target: github-issue}\n"
	p, err := onboarding.SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onboarding.Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if ui.NeedsSetup(dir) {
		t.Fatal("an approved Lab needs setup")
	}
}

// With exactly one Lab for the repository, forgecell ui opens it as before.
func TestUISetupOpensTheOneMatchingLab(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/acme/widgets.git")
	offered := uiSetupFakes(t)
	lab := filepath.Join(home, ".forgecell", "labs", "acme--widgets")
	approvedLab(t, lab, "acme/widgets")

	_, stop := startUI(t, "--no-open")
	code, output := stop()
	if code != 0 || strings.Contains(output, "Set up your Lab") || strings.Contains(output, "Choose a Lab") || !strings.Contains(output, "Lab "+lab) {
		t.Fatalf("ui with one Lab: %d %s", code, output)
	}
	if got := offered(); len(got) != 0 {
		t.Fatalf("a ready Lab was served as setup: %q", got)
	}
}

// The one Lab for the repository with no approved Formula opens in Set up your
// Lab for that Lab, with no chooser, so its Formula can be approved again.
func TestUISetupContinuesTheOneUnapprovedLab(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/acme/widgets.git")
	offered := uiSetupFakes(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	lab := filepath.Join(labs, "acme--widgets")
	identityLab(t, lab, "acme/widgets")
	before := identityTree(t, labs)

	_, stop := startUI(t, "--no-open")
	code, output := stop()
	if code != 0 || !strings.Contains(output, "Set up your Lab "+lab) || strings.Contains(output, "Choose a Lab") {
		t.Fatalf("ui with one unapproved Lab: %d %s", code, output)
	}
	if got := offered(); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("setup offered %q", got)
	}
	if identityTree(t, labs) != before {
		t.Fatal("serving setup modified the Lab")
	}
}

// With several Labs for the repository, forgecell ui offers a chooser where
// every other command still asks for --lab DIR; an explicit --lab wins.
func TestUISetupOffersAChooserForSeveralLabs(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/acme/widgets.git")
	offered := uiSetupFakes(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	first, second := filepath.Join(labs, "widgets-a"), filepath.Join(labs, "widgets-b")
	identityLab(t, first, "acme/widgets")
	identityLab(t, second, "Acme/Widgets")
	identityLab(t, filepath.Join(labs, "other"), "acme/other")
	before := identityTree(t, labs)

	if got := uiLabChoices(context.Background(), ""); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("choices %q", got)
	}
	if got := uiLabChoices(context.Background(), "named"); got != nil {
		t.Fatalf("a Lab name still offered a chooser: %q", got)
	}
	_, stop := startUI(t, "--no-open")
	code, output := stop()
	if code != 0 || !strings.Contains(output, "Choose a Lab: 2 Labs match") {
		t.Fatalf("ui with several Labs: %d %s", code, output)
	}
	if got := offered(); len(got) != 1 || !slices.Equal(got[0], []string{first, second}) {
		t.Fatalf("chooser offered %q", got)
	}
	if code, _, stderr := identityRun("doctor"); code != 1 || !strings.Contains(stderr, "--lab") {
		t.Fatalf("doctor no longer asks for --lab: %d %s", code, stderr)
	}

	_, stop = startUI(t, "--no-open", "--lab", first)
	if code, output := stop(); code != 0 || strings.Contains(output, "Choose a Lab") || !strings.Contains(output, "Lab "+first) {
		t.Fatalf("explicit --lab: %d %s", code, output)
	}
	if identityTree(t, labs) != before {
		t.Fatal("choosing among Labs modified them")
	}

	// FORGECELL_LAB chooses the Lab itself, so no chooser is offered.
	t.Setenv("FORGECELL_LAB", first)
	if got := uiLabChoices(context.Background(), ""); got != nil {
		t.Fatalf("FORGECELL_LAB still offered a chooser: %q", got)
	}
}

// A truncated Lab scan cannot prove which Labs match, so no chooser is
// offered and the resolution error stands.
func TestUISetupRefusesAChooserFromATruncatedScan(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/acme/widgets.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "widgets-a"), "acme/widgets")
	identityLab(t, filepath.Join(labs, "widgets-b"), "acme/widgets")
	identityLab(t, filepath.Join(labs, "widgets-c"), "acme/widgets")
	previous := maxLabEntries
	maxLabEntries = 2
	t.Cleanup(func() { maxLabEntries = previous })
	if got := uiLabChoices(context.Background(), ""); got != nil {
		t.Fatalf("a truncated scan offered %q", got)
	}
}
