package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHardeningLabDirectoriesNeverCollide(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "a--b-c"), "a/b-c")
	seen := map[string]string{}
	for _, tc := range []struct{ remote, name, want string }{
		{"https://github.com/a-b/c.git", "", "a-b--c"},
		{"https://github.com/a/b-c.git", "", "a--b-c"},
		{"https://github.com/o/r.git", "x", "o--r@x"},
		{"https://github.com/o/r-x.git", "", "o--r-x"},
		{"https://github.com/O/R.git", "", "o--r"},
	} {
		identityStubOrigin(t, tc.remote)
		got, err := resolveDefaultLab(context.Background(), tc.name)
		if err != nil || got.Dir != filepath.Join(labs, tc.want) {
			t.Fatalf("%s %q resolved to %+v, %v; want %s", tc.remote, tc.name, got, err, tc.want)
		}
		if tc.want == "a--b-c" && got.Notice != "" {
			t.Fatalf("the Lab's own directory should be quiet: %q", got.Notice)
		}
		if prior, dup := seen[got.Dir]; dup {
			t.Fatalf("%s %q and %s share %s", tc.remote, tc.name, prior, got.Dir)
		}
		seen[got.Dir] = tc.remote + " " + tc.name
	}
}

func TestHardeningSymlinkedOrNonDirectoryIdentityPathIsRefused(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	if err := os.MkdirAll(labs, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	identityLab(t, outside, "Owner/Repo")
	setups := map[string]func(path string) error{
		"symlink to a real Lab": func(path string) error { return os.Symlink(outside, path) },
		"dangling symlink":      func(path string) error { return os.Symlink(filepath.Join(outside, "missing"), path) },
		"regular file":          func(path string) error { return os.WriteFile(path, []byte("x"), 0600) },
	}
	for name, setup := range setups {
		for _, labName := range []string{"", "staging"} {
			t.Run(name+"/"+labName, func(t *testing.T) {
				path := filepath.Join(labs, identityLabName("Owner/Repo", labName))
				if err := setup(path); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(path)
				before, outsideBefore := identityTree(t, labs), identityTree(t, outside)
				got, err := resolveDefaultLab(context.Background(), labName)
				if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "--lab DIR") {
					t.Fatalf("an identity path that is not a real directory was used: %+v, %v", got, err)
				}
				if identityTree(t, labs) != before || identityTree(t, outside) != outsideBefore {
					t.Fatal("refusing the path changed something")
				}
			})
		}
	}
}

// The Labs here sit under names that cannot be this repository's, so they are
// named in the notice and resolution carries on; a legacy-named directory that
// could be this repository's is refused instead (see the legacy refusal tests).
func TestHardeningUnrelatedUnreadableLabsAreNamedInTheNotice(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	broken := filepath.Join(labs, "broken")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "lab.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	identityLab(t, outside, "Owner/Repo")
	linked := filepath.Join(labs, "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	before := identityTree(t, labs)

	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != filepath.Join(labs, "owner--repo") {
		t.Fatalf("%+v, %v", got, err)
	}
	if !strings.Contains(got.Notice, broken) || !strings.Contains(got.Notice, linked) {
		t.Fatalf("the notice does not name the Labs that could not be read: %q", got.Notice)
	}
	if identityTree(t, labs) != before {
		t.Fatal("resolution created or changed a Lab")
	}

	legacy := filepath.Join(labs, "forgecell-public-backlog")
	identityLab(t, legacy, "owner/repo")
	got, err = resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != legacy || !strings.Contains(got.Notice, broken) || !strings.Contains(got.Notice, "using existing Lab "+legacy) {
		t.Fatalf("%+v, %v", got, err)
	}

	code, _, stderr := identityRun("ledger", "mol-test")
	if code == 0 || !strings.Contains(stderr, broken) {
		t.Fatalf("the command did not announce the unreadable Lab: code %d, stderr %q", code, stderr)
	}
}

func TestHardeningIdentityDirectoryWinsAmongSeveralMatchingLabs(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	legacy := filepath.Join(labs, "forgecell-core-0123456789abcdef")
	identity := filepath.Join(labs, "owner--repo")
	identityLab(t, legacy, "Owner/Repo")
	identityLab(t, identity, "owner/repo")
	before := identityTree(t, labs)
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != identity || !strings.Contains(got.Notice, legacy) {
		t.Fatalf("got %+v, %v; want %s and a notice naming %s", got, err, identity, legacy)
	}
	if identityTree(t, labs) != before {
		t.Fatal("resolution changed a Lab")
	}
	identityLedger(t, identity)
	if code, out, stderr := identityRun("ledger", "mol-test"); code != 0 || out != identityRawLedger+"\n" || !strings.Contains(stderr, legacy) {
		t.Fatalf("code %d, out %q, err %q", code, out, stderr)
	}
}

func TestHardeningAmbiguityNeverSuggestsLabName(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	one := filepath.Join(labs, "forgecell-core-0123456789abcdef")
	two := filepath.Join(labs, "forgecell-public-backlog")
	identityLab(t, one, "Owner/Repo")
	identityLab(t, two, "owner/repo")
	if _, err := resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), one) || !strings.Contains(err.Error(), two) || !strings.Contains(err.Error(), "--lab DIR") || strings.Contains(err.Error(), "--lab-name") {
		t.Fatalf("%v", err)
	}
	code, out, stderr := identityRun("doctor")
	if code != 1 || out != "" || strings.Contains(stderr, "--lab-name") || !strings.Contains(stderr, "--lab DIR") {
		t.Fatalf("code %d, out %q, err %q", code, out, stderr)
	}
}

func TestHardeningLegacyNamedLabsKeepResolving(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	for _, tc := range []struct{ name, legacyDir, identityDir string }{
		{"", "owner-repo", "owner--repo"},
		{"Staging", "owner-repo-staging", "owner--repo@staging"},
	} {
		t.Run("legacy "+tc.legacyDir, func(t *testing.T) {
			legacy, identity := filepath.Join(labs, tc.legacyDir), filepath.Join(labs, tc.identityDir)
			identityLab(t, legacy, "Owner/Repo")
			defer os.RemoveAll(legacy)
			before := identityTree(t, labs)

			got, err := resolveDefaultLab(context.Background(), tc.name)
			if err != nil || got.Dir != legacy || !strings.Contains(got.Notice, "using existing Lab "+legacy) || !strings.Contains(got.Notice, identity) || !strings.Contains(got.Notice, "renaming") {
				t.Fatalf("a legacy Lab was not reused with a rename notice: %+v, %v", got, err)
			}
			if identityTree(t, labs) != before {
				t.Fatal("resolution created a second Lab beside the legacy one")
			}

			identityLab(t, identity, "owner/repo")
			defer os.RemoveAll(identity)
			got, err = resolveDefaultLab(context.Background(), tc.name)
			if err != nil || got.Dir != identity {
				t.Fatalf("the new-format Lab must win over the legacy one: %+v, %v", got, err)
			}
		})
	}
}

func TestHardeningLegacyLookingLabOfAnotherRepositoryIsNotChosen(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/foo/bar-baz.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "foo-bar-baz"), "foo-bar/baz")
	identityLab(t, filepath.Join(labs, "foo-bar-baz-x"), "foo-bar/baz")
	before := identityTree(t, labs)
	for _, tc := range []struct{ name, want string }{
		{"", "foo--bar-baz"},
		{"x", "foo--bar-baz@x"},
	} {
		got, err := resolveDefaultLab(context.Background(), tc.name)
		if err != nil || got.Dir != filepath.Join(labs, tc.want) {
			t.Fatalf("name %q resolved to %+v, %v; want %s", tc.name, got, err, tc.want)
		}
	}
	if identityTree(t, labs) != before {
		t.Fatal("resolution changed a Lab")
	}
}

func TestHardeningLabNameWithForgecellLabIsAnError(t *testing.T) {
	labIdentityEnv(t)
	identityFakeCheckout(t)
	calls := identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	override := filepath.Join(t.TempDir(), "override")
	identityLedger(t, override)
	t.Setenv("FORGECELL_LAB", override)

	if got, err := resolveDefaultLab(context.Background(), "staging"); err == nil || !strings.Contains(err.Error(), "FORGECELL_LAB") || !strings.Contains(err.Error(), "--lab-name") {
		t.Fatalf("--lab-name was ignored: %+v, %v", got, err)
	}
	code, out, stderr := identityRun("ledger", "mol-test", "--lab-name", "staging")
	if code != 1 || out != "" || !strings.Contains(stderr, "FORGECELL_LAB") {
		t.Fatalf("code %d, out %q, err %q", code, out, stderr)
	}
	code, out, stderr = identityRun("ledger", "mol-test")
	if code != 0 || out != identityRawLedger+"\n" || stderr != "" {
		t.Fatalf("FORGECELL_LAB alone must stay a silent override: code %d, out %q, err %q", code, out, stderr)
	}
	code, out, stderr = identityRun("ledger", "mol-test", "--lab", override)
	if code != 0 || out != identityRawLedger+"\n" || stderr != "" {
		t.Fatalf("an explicit --lab alone is unchanged: code %d, out %q, err %q", code, out, stderr)
	}
	if *calls != 0 {
		t.Fatalf("the remote was looked up %d times", *calls)
	}
}

func TestHardeningLabWithLabNameIsAnErrorNamingBoth(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	calls := identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	chosen := filepath.Join(t.TempDir(), "chosen")
	identityLedger(t, chosen)
	for _, command := range []string{"ledger", "loop"} {
		for _, args := range [][]string{
			{command, "--lab", chosen, "--lab-name", "staging"},
			{command, "--lab-name", "staging", "--lab", chosen},
			{command, "--lab=" + chosen, "--lab-name=staging"},
			{command, "-lab", chosen, "-lab-name", "staging"},
		} {
			code, out, stderr := identityRun(args...)
			if code != 1 || out != "" || !strings.Contains(stderr, "--lab DIR") || !strings.Contains(stderr, "--lab-name") || !strings.Contains(stderr, "cannot be combined") {
				t.Fatalf("%v: code %d, out %q, err %q", args, code, out, stderr)
			}
		}
	}
	if *calls != 0 {
		t.Fatalf("the remote was looked up %d times", *calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("a refused command touched Lab storage")
	}
}

func TestHardeningUnreadableOrPendingLegacyNamedLabIsRefusedNotShadowed(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	identityLab(t, outside, "Owner/Repo")
	setups := map[string]func(dir string) error{
		"corrupt lab.json": func(dir string) error {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "lab.json"), []byte("{"), 0600)
		},
		"unreadable Formula": func(dir string) error {
			identityLab(t, dir, "Owner/Repo")
			return os.Remove(filepath.Join(dir, "formulas", "f.yaml"))
		},
		"lab.json is a directory": func(dir string) error { return os.MkdirAll(filepath.Join(dir, "lab.json"), 0700) },
		"no lab.json yet":         func(dir string) error { return os.MkdirAll(filepath.Join(dir, "proposals"), 0700) },
		"symbolic link": func(dir string) error {
			if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
				return err
			}
			return os.Symlink(outside, dir)
		},
	}
	for setupName, setup := range setups {
		for _, tc := range []struct{ labName, legacyDir, identityDir string }{
			{"", "owner-repo", "owner--repo"},
			{"Staging", "owner-repo-staging", "owner--repo@staging"},
		} {
			t.Run(setupName+"/"+tc.legacyDir, func(t *testing.T) {
				legacy, identity := filepath.Join(labs, tc.legacyDir), filepath.Join(labs, tc.identityDir)
				if err := setup(legacy); err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(legacy)
				before, outsideBefore := identityTree(t, labs), identityTree(t, outside)

				got, err := resolveDefaultLab(context.Background(), tc.labName)
				if err == nil || got.Dir != "" || !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), "--lab DIR") || !strings.Contains(err.Error(), identity) {
					t.Fatalf("a legacy directory that may be this repository's was not refused: %+v, %v", got, err)
				}
				if identityTree(t, labs) != before || identityTree(t, outside) != outsideBefore {
					t.Fatal("refusing changed something")
				}
				if _, statErr := os.Lstat(identity); !os.IsNotExist(statErr) {
					t.Fatalf("the new-format sibling exists: %v", statErr)
				}
				args := []string{"doctor"}
				if tc.labName != "" {
					args = append(args, "--lab-name", tc.labName)
				}
				if code, out, stderr := identityRun(args...); code != 1 || out != "" || !strings.Contains(stderr, legacy) {
					t.Fatalf("the command did not refuse: code %d, out %q, err %q", code, out, stderr)
				}
				if _, statErr := os.Lstat(identity); !os.IsNotExist(statErr) {
					t.Fatalf("the command created %s: %v", identity, statErr)
				}
			})
		}
	}
}

func TestHardeningLegacyDirectoryOfAnotherRepositoryOnlyBlocksWhenItMayBeOurs(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/foo/bar-baz.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "foo-bar-baz"), "foo-bar/baz")
	if err := os.WriteFile(filepath.Join(labs, "foo-bar-baz-note"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "note"} {
		got, err := resolveDefaultLab(context.Background(), name)
		want := "foo--bar-baz"
		if name != "" {
			want += "@" + name
		}
		if err != nil || got.Dir != filepath.Join(labs, want) {
			t.Fatalf("name %q: a Lab of another repository, or a plain file, blocked resolution: %+v, %v", name, got, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(labs, "foo-bar-baz-y", "proposals"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveDefaultLab(context.Background(), "y"); err == nil || !strings.Contains(err.Error(), filepath.Join(labs, "foo-bar-baz-y")) || !strings.Contains(err.Error(), "--lab DIR") {
		t.Fatalf("a pending directory at the exact legacy name was not refused: %+v, %v", got, err)
	}
}

func TestHardeningExactLegacyNamedLabIsPreferredAmongSeveralMatches(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	legacy := filepath.Join(labs, "owner-repo")
	pathKeyed := filepath.Join(labs, "forgecell-core-0123456789abcdef")
	identityLab(t, legacy, "Owner/Repo")
	identityLab(t, pathKeyed, "owner/repo")
	before := identityTree(t, labs)

	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != legacy || !strings.Contains(got.Notice, pathKeyed) || !strings.Contains(got.Notice, "owner--repo") || !strings.Contains(got.Notice, "renaming") {
		t.Fatalf("the exact legacy name was not preferred with a notice: %+v, %v", got, err)
	}
	if identityTree(t, labs) != before {
		t.Fatal("resolution changed a Lab")
	}

	other := filepath.Join(labs, "forgecell-public-backlog")
	identityLab(t, other, "owner/repo")
	if got, err = resolveDefaultLab(context.Background(), ""); err != nil || got.Dir != legacy || !strings.Contains(got.Notice, other) || !strings.Contains(got.Notice, pathKeyed) {
		t.Fatalf("%+v, %v", got, err)
	}

	if err = os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), pathKeyed) || strings.Contains(err.Error(), "--lab-name") {
		t.Fatalf("with no exact legacy or identity directory the ambiguity must stay an error: %v", err)
	}
}

func TestHardeningLabIdentityMessagesStripTerminalControlText(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	override, isolate := string(rune(0x202e)), string(rune(0x2066))
	evil := "evil\x1b[31m\x1b]0;title\x07\nname" + override + isolate
	broken := filepath.Join(labs, evil)
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Skipf("this file system cannot hold the directory name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "lab.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"\x1b", "\n", "\x07", override, isolate}
	clean := func(what, text string) {
		t.Helper()
		if !strings.Contains(text, "evil") || !strings.Contains(text, "name") {
			t.Fatalf("%s lost the directory name: %q", what, text)
		}
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Fatalf("%s carries %q: %q", what, bad, text)
			}
		}
	}

	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != filepath.Join(labs, "owner--repo") {
		t.Fatalf("%+v, %v", got, err)
	}
	clean("the unreadable-Lab notice", got.Notice)

	identityLab(t, filepath.Join(labs, evil+"-2"), "Owner/Repo")
	identityLab(t, filepath.Join(labs, "forgecell-core-abc"), "owner/repo")
	_, err = resolveDefaultLab(context.Background(), "")
	if err == nil {
		t.Fatal("two matching Labs were not reported")
	}
	if text := strings.ReplaceAll(err.Error(), "\n  ", " "); strings.Contains(text, "\n") {
		t.Fatalf("the ambiguity error carries a line break from a directory name: %q", err)
	}
	for _, bad := range []string{"\x1b", "\x07", override, isolate} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("the ambiguity error carries %q: %q", bad, err)
		}
	}
	if !strings.Contains(err.Error(), "evil") {
		t.Fatalf("the ambiguity error lost the directory name: %q", err)
	}

	code, _, stderr := identityRun("doctor")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, bad := range []string{"\x1b", "\x07", override, isolate} {
		if strings.Contains(stderr, bad) {
			t.Fatalf("stderr carries %q: %q", bad, stderr)
		}
	}
}
