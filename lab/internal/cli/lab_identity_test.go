package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const identityRawLedger = `{"schemaVersion":"v0","kind":"molecule","id":"mol-test","formulaId":"f","status":"waiting"}`

func labIdentityEnv(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("FORGECELL_HOME", "")
	t.Setenv("FORGECELL_LAB", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

func identityChdir(t *testing.T, dir string) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(original) })
}

func identityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for this test")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// identityCheckout creates a real local git checkout; origin is added only when non-empty.
func identityCheckout(t *testing.T, dir, origin string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	identityGit(t, dir, "init", "-q")
	if origin != "" {
		identityGit(t, dir, "remote", "add", "origin", origin)
	}
	return dir
}

func identityFakeCheckout(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	identityChdir(t, dir)
	return dir
}

func identityStubOrigin(t *testing.T, remote string) *int {
	t.Helper()
	calls := new(int)
	previous := originRemote
	originRemote = func(ctx context.Context, root string) (string, error) {
		*calls++
		if remote == "" {
			return "", fmt.Errorf("no origin")
		}
		return remote, nil
	}
	t.Cleanup(func() { originRemote = previous })
	return calls
}

func identityLab(t *testing.T, dir, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600); err != nil {
		t.Fatal(err)
	}
	recipe := "kind: formula\nid: f\nintake: {repo: " + repo + "}\nharness: {binding: codex}\natoms: [{type: harness}]\n"
	if err := os.WriteFile(filepath.Join(dir, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
}

func identityLedger(t *testing.T, lab string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-test.json"), []byte(identityRawLedger), 0600); err != nil {
		t.Fatal(err)
	}
}

func identityRun(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev")
	return code, out.String(), stderr.String()
}

func identityTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		line := fmt.Sprintf("%s %v", path, info.Mode())
		if info.Mode().IsRegular() {
			raw, _ := os.ReadFile(path)
			line += fmt.Sprintf(" %x", sha256.Sum256(raw))
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestLabIdentityClonesAtDifferentPathsShareOneLab(t *testing.T) {
	home := labIdentityEnv(t)
	base := t.TempDir()
	one := identityCheckout(t, filepath.Join(base, "one", "forgecell"), "https://github.com/Owner/Repo.git")
	two := identityCheckout(t, filepath.Join(base, "two", "unrelated-name"), "git@github.com:owner/repo.git")
	want := filepath.Join(home, ".forgecell", "labs", "owner-repo")
	for _, dir := range []string{one, filepath.Join(one, "sub"), two} {
		identityChdir(t, dir)
		got, err := resolveDefaultLab(context.Background(), "")
		if err != nil || got.Dir != want || got.Notice != "" {
			t.Fatalf("%s resolved to %+v, %v; want %s", dir, got, err, want)
		}
	}
}

func TestLabIdentityRenamedCheckoutKeepsItsLab(t *testing.T) {
	home := labIdentityEnv(t)
	base := t.TempDir()
	before := identityCheckout(t, filepath.Join(base, "forgecell"), "https://github.com/Owner/Repo.git")
	identityChdir(t, before)
	first, err := resolveDefaultLab(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	pathKeyed, err := defaultLabDir()
	if err != nil {
		t.Fatal(err)
	}
	after := filepath.Join(base, "renamed-checkout")
	if err := os.Rename(before, after); err != nil {
		t.Fatal(err)
	}
	identityChdir(t, after)
	second, err := resolveDefaultLab(context.Background(), "")
	if err != nil || second.Dir != first.Dir || first.Dir != filepath.Join(home, ".forgecell", "labs", "owner-repo") {
		t.Fatalf("rename changed the Lab: %+v then %+v, %v", first, second, err)
	}
	if renamedPathKeyed, _ := defaultLabDir(); renamedPathKeyed == pathKeyed {
		t.Fatal("test premise failed: renaming should change the path-keyed Lab")
	}
}

func TestLabIdentityWorktreeSharesTheMainCheckoutLab(t *testing.T) {
	labIdentityEnv(t)
	base := t.TempDir()
	main := identityCheckout(t, filepath.Join(base, "main"), "https://github.com/Owner/Repo.git")
	identityGit(t, main, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	worktree := filepath.Join(base, "feature-worktree")
	identityGit(t, main, "worktree", "add", "-q", worktree, "-b", "feature")
	if info, err := os.Lstat(filepath.Join(worktree, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("worktree .git should be a file: %v", err)
	}
	identityChdir(t, main)
	want, err := resolveDefaultLab(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	pathKeyedMain, _ := defaultLabDir()
	identityChdir(t, worktree)
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != want.Dir {
		t.Fatalf("worktree resolved to %+v, %v; want %s", got, err, want.Dir)
	}
	if pathKeyedWorktree, _ := defaultLabDir(); pathKeyedWorktree == pathKeyedMain {
		t.Fatal("test premise failed: a worktree has a different path-keyed Lab")
	}
}

func TestLabIdentityFallsBackToPathKeyedLabAndSaysSo(t *testing.T) {
	labIdentityEnv(t)
	base := t.TempDir()
	cases := map[string]func() string{
		"no origin remote": func() string { return identityCheckout(t, filepath.Join(base, "no-origin"), "") },
		"non-GitHub remote": func() string {
			return identityCheckout(t, filepath.Join(base, "gitlab"), "https://gitlab.com/owner/repo.git")
		},
		"not a git checkout": func() string {
			dir := filepath.Join(base, "plain")
			os.MkdirAll(dir, 0700)
			return dir
		},
	}
	for name, create := range cases {
		identityChdir(t, create())
		want, err := defaultLabDir()
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveDefaultLab(context.Background(), "")
		if err != nil || got.Dir != want || !strings.Contains(got.Notice, "path-keyed") || !strings.Contains(got.Notice, want) {
			t.Fatalf("%s: got %+v, %v; want path-keyed %s", name, got, err, want)
		}
		if _, err := resolveDefaultLab(context.Background(), "staging"); err == nil || !strings.Contains(err.Error(), "--lab") {
			t.Fatalf("%s: --lab-name without a GitHub identity should point to --lab: %v", name, err)
		}
	}
}

func TestLabIdentityFallsBackWhenGitIsNotInstalled(t *testing.T) {
	labIdentityEnv(t)
	dir := identityCheckout(t, filepath.Join(t.TempDir(), "checkout"), "https://github.com/Owner/Repo.git")
	identityChdir(t, dir)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty-bin"))
	want, err := defaultLabDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != want || !strings.Contains(got.Notice, "path-keyed") {
		t.Fatalf("got %+v, %v; want path-keyed %s", got, err, want)
	}
}

func TestLabIdentityNewRepositoryUsesIdentityDirectoryWithoutCreatingIt(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != filepath.Join(home, ".forgecell", "labs", "owner-repo") {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("resolution created Lab storage")
	}
}

func TestLabIdentitySingleMatchingLabIsReusedAndAnnounced(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	legacy := filepath.Join(labs, "forgecell-public-backlog")
	identityLab(t, legacy, "OWNER/repo")
	identityLab(t, filepath.Join(labs, "someone-else"), "other/project")
	identityLedger(t, legacy)
	before := identityTree(t, labs)

	code, out, stderr := identityRun("ledger", "mol-test")
	if code != 0 || out != identityRawLedger+"\n" {
		t.Fatalf("ledger did not come from the matching Lab: code %d, out %q, err %q", code, out, stderr)
	}
	if !strings.Contains(stderr, legacy) {
		t.Fatalf("chosen Lab was not announced on stderr: %q", stderr)
	}
	if strings.Contains(out, legacy) {
		t.Fatal("announcement leaked into stdout")
	}
	if identityTree(t, labs) != before {
		t.Fatal("Lab selection changed the Lab tree")
	}
}

func TestLabIdentityMatchingIdentityDirectoryIsQuiet(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	identity := filepath.Join(home, ".forgecell", "labs", "owner-repo")
	identityLab(t, identity, "Owner/Repo")
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != identity || got.Notice != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLabIdentityAmbiguousMatchesAreReportedAndNothingIsCreated(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	legacy := filepath.Join(labs, "forgecell-core-0123456789abcdef")
	identity := filepath.Join(labs, "owner-repo")
	identityLab(t, legacy, "Owner/Repo")
	identityLab(t, identity, "owner/repo")
	before := identityTree(t, labs)

	if _, err := resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), identity) || !strings.Contains(err.Error(), "--lab") {
		t.Fatalf("ambiguity was not reported with every candidate: %v", err)
	}
	for _, args := range [][]string{{"init", "--json"}, {"ledger", "mol-test"}, {"doctor"}} {
		code, out, stderr := identityRun(args...)
		if code != 1 || out != "" || !strings.Contains(stderr, legacy) || !strings.Contains(stderr, identity) || !strings.Contains(stderr, "--lab") {
			t.Fatalf("%v: code %d, out %q, err %q", args, code, out, stderr)
		}
	}
	if identityTree(t, labs) != before {
		t.Fatal("ambiguity handling modified Labs")
	}

	identityLedger(t, legacy)
	code, out, stderr := identityRun("ledger", "mol-test", "--lab", legacy)
	if code != 0 || out != identityRawLedger+"\n" || stderr != "" {
		t.Fatalf("an explicit --lab must bypass ambiguity: code %d, out %q, err %q", code, out, stderr)
	}
}

func TestLabIdentityIgnoresUnrelatedUnreadableAndSymlinkedLabs(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "unrelated"), "other/project")
	if err := os.MkdirAll(filepath.Join(labs, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(labs, "broken", "lab.json"), []byte("{"), 0600)
	os.WriteFile(filepath.Join(labs, "stray-file"), []byte("x"), 0600)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	identityLab(t, outside, "Owner/Repo")
	if err := os.Symlink(outside, filepath.Join(labs, "linked")); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != filepath.Join(labs, "owner-repo") {
		t.Fatalf("unrelated, unreadable or symlinked Labs influenced selection: %+v, %v", got, err)
	}

	valid := filepath.Join(labs, "valid-legacy")
	identityLab(t, valid, "owner/repo")
	got, err = resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != valid {
		t.Fatalf("got %+v, %v; want the single readable match %s", got, err, valid)
	}
}

func TestLabIdentityPendingIdentityDirectoryDoesNotHideAMatchingLegacyLab(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	if err := os.MkdirAll(filepath.Join(labs, "owner-repo", "proposals"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(labs, "forgecell-core-abc")
	identityLab(t, legacy, "Owner/Repo")
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != legacy {
		t.Fatalf("got %+v, %v; want %s", got, err, legacy)
	}
}

func TestLabIdentityRefusesAnIdentityDirectoryOwnedByAnotherRepository(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	// foo-bar/baz and foo/bar-baz both map to foo-bar-baz.
	identityStubOrigin(t, "https://github.com/foo/bar-baz.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "foo-bar-baz"), "foo-bar/baz")
	if got, err := resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--lab-name") || !strings.Contains(err.Error(), "--lab") {
		t.Fatalf("another repository's Lab was reused: %+v, %v", got, err)
	}
	identityLab(t, filepath.Join(labs, "foo-bar-baz-x"), "foo/bar")
	if got, err := resolveDefaultLab(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "--lab") {
		t.Fatalf("a named Lab owned by another repository was reused: %+v, %v", got, err)
	}
	if got, err := resolveDefaultLab(context.Background(), "y"); err != nil || got.Dir != filepath.Join(labs, "foo-bar-baz-y") {
		t.Fatalf("a free name should work: %+v, %v", got, err)
	}
}

func TestLabIdentityLabNameSelectsNamedLabWithoutScanning(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "forgecell-core-abc"), "Owner/Repo")
	identityLab(t, filepath.Join(labs, "owner-repo"), "Owner/Repo")
	got, err := resolveDefaultLab(context.Background(), "Staging")
	if err != nil || got.Dir != filepath.Join(labs, "owner-repo-staging") {
		t.Fatalf("got %+v, %v", got, err)
	}
	identityLab(t, filepath.Join(labs, "owner-repo-staging"), "owner/repo")
	got, err = resolveDefaultLab(context.Background(), "staging")
	if err != nil || got.Dir != filepath.Join(labs, "owner-repo-staging") || got.Notice != "" {
		t.Fatalf("an existing named Lab for this repository should be used quietly: %+v, %v", got, err)
	}
	code, out, stderr := identityRun("ledger", "mol-test", "--lab-name", "staging")
	if code == 0 || out != "" || strings.Contains(stderr, "not defined") || strings.Contains(stderr, "more than one") {
		t.Fatalf("named Lab should select owner-repo-staging (no ledger there): %d %q %q", code, out, stderr)
	}
	identityLedger(t, filepath.Join(labs, "owner-repo-staging"))
	code, out, stderr = identityRun("ledger", "mol-test", "--lab-name=staging")
	if code != 0 || out != identityRawLedger+"\n" {
		t.Fatalf("--lab-name=NAME: %d %q %q", code, out, stderr)
	}
	code, out, stderr = identityRun("--lab-name", "x", "ledger")
	if code == 0 {
		t.Fatalf("a leading --lab-name is not a command: %q %q", out, stderr)
	}
}

func TestLabIdentityLabNameValidationHappensBeforeAnyLookup(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	calls := identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	for _, name := range []string{"", "a/b", "a\\b", "..", ".", "a.b", "a b", strings.Repeat("a", 33), "naïve", "a\x00b"} {
		code, out, stderr := identityRun("ledger", "mol-test", "--lab-name", name)
		if code != 1 || out != "" || !strings.Contains(stderr, "lab-name") {
			t.Fatalf("name %q accepted: code %d, out %q, err %q", name, code, out, stderr)
		}
	}
	if code, _, stderr := identityRun("ledger", "mol-test", "--lab", t.TempDir(), "--lab-name", "a/b"); code != 1 || !strings.Contains(stderr, "lab-name") {
		t.Fatalf("an invalid name is refused even with an explicit Lab: %d %q", code, stderr)
	}
	for _, args := range [][]string{{"ledger", "mol-test", "--lab-name"}, {"ledger", "mol-test", "--lab-name", "a", "--lab-name", "b"}} {
		if code, _, stderr := identityRun(args...); code != 1 || !strings.Contains(stderr, "lab-name") {
			t.Fatalf("%v: %d %q", args, code, stderr)
		}
	}
	if *calls != 0 {
		t.Fatalf("validation looked up the remote %d times", *calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("validation touched Lab storage")
	}
	for _, name := range []string{"a", "A_b-1", strings.Repeat("a", 32)} {
		if got, err := resolveDefaultLab(context.Background(), name); err != nil || filepath.Base(got.Dir) != "owner-repo-"+strings.ToLower(name) {
			t.Fatalf("name %q: %+v, %v", name, got, err)
		}
	}
}

func TestLabIdentityEveryLabCommandAcceptsLabNameAndOthersDoNot(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty-bin"))
	for _, args := range [][]string{{"init", "--review", "missing"}, {"doctor"}, {"run"}, {"ledger"}, {"amend"}, {"recover"}, {"issues"}, {"learn"}, {"suggestion"}, {"deliver"}, {"checks"}} {
		_, _, stderr := identityRun(append(append([]string{}, args...), "--lab-name", "x")...)
		if strings.Contains(stderr, "not defined") || strings.Contains(stderr, "lab-name") {
			t.Errorf("%s rejected --lab-name: %q", args[0], stderr)
		}
	}
	for _, args := range [][]string{{"labs"}, {"evaluate"}, {"rollback"}} {
		code, _, _ := identityRun(append(append([]string{}, args...), "--lab-name", "x")...)
		if code == 0 {
			t.Errorf("%s accepted --lab-name", args[0])
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("a rejected or read-only command created Lab storage")
	}
}

func TestLabIdentityExplicitLabWinsWithoutLookupOrNotice(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	calls := identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "forgecell-core-abc"), "Owner/Repo")
	identityLab(t, filepath.Join(labs, "owner-repo"), "Owner/Repo")
	chosen := filepath.Join(t.TempDir(), "chosen")
	identityLedger(t, chosen)
	for _, args := range [][]string{
		{"ledger", "mol-test", "--lab", chosen},
		{"ledger", "mol-test", "--lab=" + chosen},
		{"ledger", "mol-test", "-lab", chosen},
		{"ledger", "mol-test", "--lab", chosen, "--lab-name", "staging"},
	} {
		code, out, stderr := identityRun(args...)
		if code != 0 || out != identityRawLedger+"\n" || stderr != "" {
			t.Fatalf("%v: code %d, out %q, err %q", args, code, out, stderr)
		}
	}
	if *calls != 0 {
		t.Fatalf("explicit --lab still looked up the remote %d times", *calls)
	}

	override := filepath.Join(t.TempDir(), "override")
	t.Setenv("FORGECELL_LAB", override)
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != override || got.Notice != "" || *calls != 0 {
		t.Fatalf("FORGECELL_LAB should stay a silent override: %+v, %v, %d lookups", got, err, *calls)
	}
}

func TestLabIdentityRefusesToGuessWhenTheLabsDirectoryIsTooLarge(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	previous := maxLabEntries
	maxLabEntries = 3
	t.Cleanup(func() { maxLabEntries = previous })
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(home, ".forgecell", "labs", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--lab") {
		t.Fatalf("a truncated scan must not be treated as complete: %+v, %v", got, err)
	}
}
