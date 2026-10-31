package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const forgeTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// forgeIdentityLab writes a Lab whose active Formula delivers through a forge
// plugin; identity is omitted from the forge block when empty.
func forgeIdentityLab(t *testing.T, dir, identity string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600); err != nil {
		t.Fatal(err)
	}
	forge := "forge: {plugin: gitlab, project: welltech-com/backend}\n"
	if identity != "" {
		forge = "forge: {plugin: gitlab, project: welltech-com/backend, identity: " + identity + "}\n"
	}
	recipe := "kind: formula\nid: f\nintake: {repo: welltech-com/backend}\nharness: {binding: codex}\natoms: [{type: harness}]\nplugins:\n  - {name: gitlab, manifest: plugins/gitlab/plugin.json, sha256: " + forgeTestDigest + "}\n" + forge
	if err := os.WriteFile(filepath.Join(dir, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
}

const forgeTestRemote = "git@gitlab.com:welltech-com/backend.git"
const forgeTestIdentity = "gitlab.com/welltech-com/backend"

// forgeTestLabName spells out the expected directory name independently of
// forgeLabName: host--path, "-", then the first 8 hex of the identity's sha256.
func forgeTestLabName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return strings.ReplaceAll(identity, "/", "--") + "-" + hex.EncodeToString(sum[:])[:8]
}

func TestForgeLabGitLabCheckoutResolvesToItsHostPathLab(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	labs := filepath.Join(home, ".forgecell", "labs")
	want := filepath.Join(labs, forgeTestLabName(forgeTestIdentity))
	forgeIdentityLab(t, want, forgeTestIdentity)
	identityLab(t, filepath.Join(labs, "owner--repo"), "Owner/Repo")
	before := identityTree(t, labs)
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != want || got.Notice != "" {
		t.Fatalf("got %+v, %v; want %s", got, err, want)
	}
	if identityTree(t, labs) != before {
		t.Fatal("resolution changed the Lab tree")
	}
}

func TestForgeLabFoundByIdentityUnderAnotherNameIsAnnounced(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://gitlab.com/welltech-com/backend.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	other := filepath.Join(labs, "backend-lab")
	forgeIdentityLab(t, other, forgeTestIdentity)
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != other || !strings.Contains(got.Notice, "forge.identity matches") {
		t.Fatalf("got %+v, %v; want %s", got, err, other)
	}
	forgeIdentityLab(t, filepath.Join(labs, "backend-copy"), forgeTestIdentity)
	if _, err := resolveDefaultLab(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "more than one Lab") {
		t.Fatalf("two matching Labs must be refused: %v", err)
	}
	canonical := filepath.Join(labs, forgeTestLabName(forgeTestIdentity))
	forgeIdentityLab(t, canonical, forgeTestIdentity)
	got, err = resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != canonical || !strings.Contains(got.Notice, "other Labs also match") {
		t.Fatalf("the host--path Lab should win and name the others: %+v, %v", got, err)
	}
}

func TestForgeLabGitHubCheckoutIsUnchanged(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, "https://github.com/Owner/Repo.git")
	labs := filepath.Join(home, ".forgecell", "labs")
	forgeIdentityLab(t, filepath.Join(labs, forgeTestLabName(forgeTestIdentity)), forgeTestIdentity)
	forgeIdentityLab(t, filepath.Join(labs, forgeTestLabName("github.com/owner/repo")), "github.com/owner/repo")
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != filepath.Join(labs, "owner--repo") || got.Notice != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestForgeLabWithoutAMatchingLabKeepsThePathKeyedFallback(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	forgeIdentityLab(t, filepath.Join(home, ".forgecell", "labs", "elsewhere"), "gitlab.com/other/project")
	want, err := defaultLabDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != want || !strings.Contains(got.Notice, "path-keyed") {
		t.Fatalf("got %+v, %v; want path-keyed %s", got, err, want)
	}
}

func TestForgeLabNameWithAPluginForgeOriginSaysSoAccurately(t *testing.T) {
	labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	_, err := resolveDefaultLab(context.Background(), "staging")
	if err == nil || !strings.Contains(err.Error(), "--lab DIR") || !strings.Contains(err.Error(), "forge identity "+forgeTestIdentity) || strings.Contains(err.Error(), "needs a GitHub origin") {
		t.Fatalf("--lab-name with a plugin-forge origin and no such Lab: %v", err)
	}
}

func TestForgeLabNamesOfAmbiguousIdentitiesAreDistinct(t *testing.T) {
	a, b := "h.com/a--b/c", "h.com/a/b--c"
	if strings.ReplaceAll(a, "/", "--") != strings.ReplaceAll(b, "/", "--") {
		t.Fatal("the pair is meant to share host--path")
	}
	if forgeLabName(a, "") == forgeLabName(b, "") {
		t.Fatalf("%s and %s share Lab directory %s", a, b, forgeLabName(a, ""))
	}

	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	dirA, dirB := filepath.Join(labs, forgeTestLabName(a)), filepath.Join(labs, forgeTestLabName(b))
	forgeIdentityLab(t, dirA, a)
	forgeIdentityLab(t, dirB, b)
	for remote, want := range map[string]string{"git@h.com:a--b/c.git": dirA, "git@h.com:a/b--c.git": dirB} {
		identityStubOrigin(t, remote)
		got, err := resolveDefaultLab(context.Background(), "")
		if err != nil || got.Dir != want || got.Notice != "" {
			t.Fatalf("%s: got %+v, %v; want %s", remote, got, err, want)
		}
	}
}

func TestForgeLabCanonicalDirectoryOfAnotherIdentityFallsThrough(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"another identity": func(t *testing.T, dir string) { forgeIdentityLab(t, dir, "gitlab.com/welltech-com/other") },
		"no identity":      func(t *testing.T, dir string) { forgeIdentityLab(t, dir, "") },
		"GitHub Lab":       func(t *testing.T, dir string) { identityLab(t, dir, "welltech-com/backend") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			home := labIdentityEnv(t)
			identityFakeCheckout(t)
			identityStubOrigin(t, forgeTestRemote)
			labs := filepath.Join(home, ".forgecell", "labs")
			canonical := filepath.Join(labs, forgeTestLabName(forgeTestIdentity))
			setup(t, canonical)
			before := identityTree(t, labs)

			want, err := defaultLabDir()
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolveDefaultLab(context.Background(), "")
			if err != nil || got.Dir != want || !strings.Contains(got.Notice, "path-keyed") || !strings.Contains(got.Notice, "so it was not used") {
				t.Fatalf("without another match: got %+v, %v; want path-keyed %s naming the skipped Lab", got, err, want)
			}

			other := filepath.Join(labs, "backend-lab")
			forgeIdentityLab(t, other, forgeTestIdentity)
			got, err = resolveDefaultLab(context.Background(), "")
			if err != nil || got.Dir != other || !strings.Contains(got.Notice, "forge.identity matches") || !strings.Contains(got.Notice, "so it was not used") {
				t.Fatalf("the identity scan should find %s: got %+v, %v", other, got, err)
			}
			if err := os.RemoveAll(other); err != nil {
				t.Fatal(err)
			}
			if identityTree(t, labs) != before {
				t.Fatal("resolution changed the Lab tree")
			}
		})
	}
}

func TestForgeLabUnverifiableCanonicalDirectoryIsRefused(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"no lab.json": func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt lab.json": func(t *testing.T, dir string) {
			forgeIdentityLab(t, dir, forgeTestIdentity)
			if err := os.WriteFile(filepath.Join(dir, "lab.json"), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, dir string) {
			real := filepath.Join(filepath.Dir(dir), "real-target")
			forgeIdentityLab(t, real, forgeTestIdentity)
			if err := os.Symlink(real, dir); err != nil {
				t.Fatal(err)
			}
		},
		"file": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			home := labIdentityEnv(t)
			identityFakeCheckout(t)
			identityStubOrigin(t, forgeTestRemote)
			labs := filepath.Join(home, ".forgecell", "labs")
			dir := filepath.Join(labs, forgeTestLabName(forgeTestIdentity))
			setup(t, dir)
			before := identityTree(t, labs)
			got, err := resolveDefaultLab(context.Background(), "")
			if err == nil || got.Dir != "" || !strings.Contains(err.Error(), "--lab DIR") {
				t.Fatalf("got %+v, %v; want a refusal", got, err)
			}
			if identityTree(t, labs) != before {
				t.Fatal("a refused resolution changed the Lab tree")
			}
		})
	}
}

func TestForgeLabCorruptCandidateIsNamedAndBlocksTheFallback(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	labs := filepath.Join(home, ".forgecell", "labs")
	broken := filepath.Join(labs, "backend-broken")
	forgeIdentityLab(t, broken, forgeTestIdentity)
	if err := os.WriteFile(filepath.Join(broken, "formulas", "f.yaml"), []byte("kind: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := identityTree(t, labs)
	got, err := resolveDefaultLab(context.Background(), "")
	if err == nil || got.Dir != "" || !strings.Contains(err.Error(), "these Labs could not be read") || !strings.Contains(err.Error(), "backend-broken") || !strings.Contains(err.Error(), "--lab DIR") {
		t.Fatalf("a corrupt Lab that may hold the identity must block the path-keyed fallback: %+v, %v", got, err)
	}
	if identityTree(t, labs) != before {
		t.Fatal("a refused resolution changed the Lab tree")
	}

	canonical := filepath.Join(labs, forgeTestLabName(forgeTestIdentity))
	forgeIdentityLab(t, canonical, forgeTestIdentity)
	got, err = resolveDefaultLab(context.Background(), "")
	if err != nil || got.Dir != canonical || !strings.Contains(got.Notice, "these Labs could not be read") || !strings.Contains(got.Notice, "backend-broken") {
		t.Fatalf("a match should still be used and the unreadable Lab named: %+v, %v", got, err)
	}
}

func TestForgeLabNameSelectsTheNamedForgeLab(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	labs := filepath.Join(home, ".forgecell", "labs")
	forgeIdentityLab(t, filepath.Join(labs, forgeTestLabName(forgeTestIdentity)), forgeTestIdentity)
	named := filepath.Join(labs, forgeTestLabName(forgeTestIdentity)+"@staging")
	forgeIdentityLab(t, named, forgeTestIdentity)
	got, err := resolveDefaultLab(context.Background(), "staging")
	if err != nil || got.Dir != named {
		t.Fatalf("got %+v, %v; want %s", got, err, named)
	}
}

func TestForgeLabNamesCannotCollideWithGitHubNames(t *testing.T) {
	forge := forgeLabName(forgeTestIdentity, "")
	if forge != forgeTestLabName(forgeTestIdentity) || !strings.HasPrefix(forge, "gitlab.com--welltech-com--backend-") || len(forge) != len("gitlab.com--welltech-com--backend-")+8 {
		t.Fatalf("forge Lab name %q", forge)
	}
	host, _, _ := strings.Cut(forge, "--")
	github := identityLabName("welltech-com/backend", "")
	owner, _, _ := strings.Cut(github, "--")
	if !strings.Contains(host, ".") || strings.Contains(owner, ".") || forge == github {
		t.Fatalf("forge %q and GitHub %q names can collide", forge, github)
	}
	if forgeLabName(forgeTestIdentity, "Staging") != forgeTestLabName(forgeTestIdentity)+"@staging" {
		t.Fatal("a Lab name is not appended in lower case")
	}
}

func TestForgeLabTooLongNameIsRefused(t *testing.T) {
	labIdentityEnv(t)
	root := identityFakeCheckout(t)
	identity := "gitlab.com/" + strings.Repeat("a/", 120) + "b"
	if _, _, err := resolveForgeLab(root, identity, ""); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("an overlong Lab directory name should be refused: %v", err)
	}
}

func TestForgeLabListingShowsTheIdentity(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	identityStubOrigin(t, forgeTestRemote)
	labs := filepath.Join(home, ".forgecell", "labs")
	forgeIdentityLab(t, filepath.Join(labs, forgeTestLabName(forgeTestIdentity)), forgeTestIdentity)
	identityLab(t, filepath.Join(labs, "owner--repo"), "Owner/Repo")

	code, out, stderr := identityRun("labs")
	if code != 0 || !strings.Contains(out, "  identity: "+forgeTestIdentity+"\n") || strings.Count(out, "identity:") != 1 {
		t.Fatalf("code %d, out %q, err %q", code, out, stderr)
	}
	code, out, stderr = identityRun("labs", "--json")
	if code != 0 {
		t.Fatalf("code %d, err %q", code, stderr)
	}
	var report struct {
		Labs []map[string]any `json:"labs"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || len(report.Labs) != 2 {
		t.Fatalf("%v: %s", err, out)
	}
	for _, l := range report.Labs {
		identity, present := l["identity"]
		switch filepath.Base(l["directory"].(string)) {
		case forgeTestLabName(forgeTestIdentity):
			if identity != forgeTestIdentity {
				t.Fatalf("forge Lab identity %v", identity)
			}
		default:
			if present {
				t.Fatalf("a GitHub Lab gained an identity field: %v", l)
			}
		}
	}
}
