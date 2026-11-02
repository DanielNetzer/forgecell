package remoteid

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

func TestFromRemoteAcceptedForms(t *testing.T) {
	for remote, want := range map[string]string{
		"https://gitlab.com/welltech-com/platform/backend.git":   "gitlab.com/welltech-com/platform/backend",
		"https://gitlab.com/welltech-com/platform/backend":       "gitlab.com/welltech-com/platform/backend",
		"https://gitlab.com/welltech-com/platform/backend/":      "gitlab.com/welltech-com/platform/backend",
		"HTTPS://GitLab.com/Welltech-Com/Platform/Backend.git":   "gitlab.com/welltech-com/platform/backend",
		"ssh://git@gitlab.com/welltech-com/platform/backend":     "gitlab.com/welltech-com/platform/backend",
		"ssh://gitlab.com/welltech-com/platform/backend.git":     "gitlab.com/welltech-com/platform/backend",
		"git@gitlab.com:welltech-com/platform/backend.git":       "gitlab.com/welltech-com/platform/backend",
		"git@gitlab.com:welltech-com/backend.git":                "gitlab.com/welltech-com/backend",
		"https://github.com/Owner/Repo.git":                      "github.com/owner/repo",
		"git@gitlab.com:group/project.v2.git":                    "gitlab.com/group/project.v2",
		"https://gitlab.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p.git": "gitlab.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p",
	} {
		t.Run(remote, func(t *testing.T) {
			got, err := FromRemote(remote)
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
			if why := validIdentity(got); why != "" {
				t.Fatalf("result is not a valid identity: %s", why)
			}
		})
	}
}

func TestFromRemoteRefusedForms(t *testing.T) {
	for name, remote := range map[string]string{
		"empty":             "",
		"https credentials": "https://user:token@gitlab.com/group/project.git",
		"ssh other user":    "ssh://deploy@gitlab.com/group/project.git",
		"ssh port":          "ssh://git@gitlab.com:2222/group/project.git",
		"https port":        "https://gitlab.com:8443/group/project.git",
		"scp other user":    "deploy@gitlab.com:group/project.git",
		"local path":        "/srv/git/project.git",
		"relative path":     "../project",
		"file url":          "file:///srv/git/project.git",
		"http":              "http://gitlab.com/group/project.git",
		"no dot host":       "https://gitlab/group/project.git",
		"host only":         "https://gitlab.com",
		"dot dot":           "https://gitlab.com/group/../project.git",
		"query":             "https://gitlab.com/group/project.git?ref=main",
		"fragment":          "https://gitlab.com/group/project.git#main",
		"percent":           "https://gitlab.com/group/%2e%2e/project.git",
		"whitespace":        "https://gitlab.com/group/my project.git",
		"empty segment":     "https://gitlab.com/group//project.git",
		"overlong":          "https://gitlab.com/" + strings.Repeat("a", MaxRemote),
		"identity too long": "https://gitlab.com/" + strings.Repeat("a", 300),
		"kelvin sign":       "https://gitlaK.com/group/project.git",
		"non-ASCII path":    "ssh://git@gitlab.com/gröup/project.git",
		"double .git":       "https://gitlab.com/group/project.git.git",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromRemote(remote)
			if err == nil || got != "" {
				t.Fatalf("got %q, %v; want a refusal", got, err)
			}
			if strings.Contains(err.Error(), "token") {
				t.Fatalf("the refusal echoes credentials: %v", err)
			}
		})
	}
}

// TestValidIdentityAgreesWithFormula pins the copy: validIdentity and
// formula.ValidForgeIdentity accept and refuse the same identities. Only this
// test imports formula; the package itself stays a leaf.
func TestValidIdentityAgreesWithFormula(t *testing.T) {
	if maxIdentity != formula.MaxForgeIdentity {
		t.Fatalf("maxIdentity %d, formula.MaxForgeIdentity %d", maxIdentity, formula.MaxForgeIdentity)
	}
	host := "gitlab.com/"
	for identity, accepted := range map[string]bool{
		"gitlab.com/welltech-com/platform/backend":                 true,
		"gitlab.com/group/project":                                 true,
		"github.com/acme/widgets":                                  true,
		"gitlab.example.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s": true,
		"git.sub-domain.example.org/group/project.v2":              true,
		"gitlab.com/group/~user_name-x.y":                          true,
		host + strings.Repeat("a", maxIdentity-len(host)):          true,
		host + strings.Repeat("a", maxIdentity-len(host)+1):        false,
		"":                                       false,
		"gitlab.com":                             false,
		"gitlab.com/":                            false,
		"gitlab/group/project":                   false,
		"GitLab.com/group/project":               false,
		"gitlab.com/Group/project":               false,
		"gitlab.com/group//project":              false,
		"gitlab.com/group/../project":            false,
		"gitlab.com/group/./project":             false,
		"gitlab.com/group/pro..ject":             false,
		"gitlab.com/group/project.git":           false,
		"gitlab.com/group/project/":              false,
		"/gitlab.com/group/project":              false,
		"-gitlab.com/group/project":              false,
		"gitlab-.com/group/project":              false,
		"gitlab.com:22/group/project":            false,
		"user@gitlab.com/group/project":          false,
		"gitlab.com/group/pro ject":              false,
		"gitlab.com/group/%2e%2e":                false,
		"gitlab.com/gröup/project":               false,
		"gitlab.com/group/project?x":             false,
		"gitlab.com/group\\project":              false,
		"gitlab.com/group/project\x00":           false,
		"https://gitlab.com/group/project":       false,
		"gitlab.com/group/project.atom.git":      false,
		"xn--bcher-kva.example/group/project.v1": true,
	} {
		ours, theirs := validIdentity(identity), formula.ValidForgeIdentity(identity)
		if (ours == "") != (theirs == "") {
			t.Errorf("%q: remoteid %q, formula %q", identity, ours, theirs)
		}
		if (ours == "") != accepted {
			t.Errorf("%q: accepted = %v, want %v (%s)", identity, ours == "", accepted, ours)
		}
	}
}

func TestValidIdentityBounds(t *testing.T) {
	host := "gitlab.com/"
	if why := validIdentity(host + strings.Repeat("a", maxIdentity-len(host))); why != "" {
		t.Fatalf("an identity of exactly %d bytes was refused: %s", maxIdentity, why)
	}
	if why := validIdentity(host + strings.Repeat("a", maxIdentity-len(host)+1)); why == "" {
		t.Fatalf("an identity over %d bytes was accepted", maxIdentity)
	}
}
