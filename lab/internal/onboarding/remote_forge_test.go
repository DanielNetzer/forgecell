package onboarding

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

func TestForgeIdentityFromRemoteAcceptedForms(t *testing.T) {
	for remote, want := range map[string]string{
		"https://gitlab.com/welltech-com/backend.git":        "gitlab.com/welltech-com/backend",
		"https://gitlab.com/welltech-com/backend":            "gitlab.com/welltech-com/backend",
		"https://gitlab.com/welltech-com/backend/":           "gitlab.com/welltech-com/backend",
		"HTTPS://GitLab.com/Group/Sub/Project.git":           "gitlab.com/group/sub/project",
		"ssh://git@gitlab.com/welltech-com/backend.git":      "gitlab.com/welltech-com/backend",
		"ssh://gitlab.example.org/group/project":             "gitlab.example.org/group/project",
		"git@gitlab.com:welltech-com/backend.git":            "gitlab.com/welltech-com/backend",
		"git@git.internal.example:a/b/c":                     "git.internal.example/a/b/c",
		"https://github.com/Owner/Repo.git":                  "github.com/owner/repo",
		"git@gitlab.com:group/project.v2.git":                "gitlab.com/group/project.v2",
		"https://bitbucket.example.com/scm/team/project.git": "bitbucket.example.com/scm/team/project",
	} {
		t.Run(remote, func(t *testing.T) {
			got, err := ForgeIdentityFromRemote(remote)
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
			if why := formula.ValidForgeIdentity(got); why != "" {
				t.Fatalf("result is not a valid forge.identity: %s", why)
			}
		})
	}
}

func TestForgeIdentityFromRemoteRefusedForms(t *testing.T) {
	for name, remote := range map[string]string{
		"empty":               "",
		"https credentials":   "https://user:token@gitlab.com/group/project.git",
		"https user":          "https://oauth2@gitlab.com/group/project.git",
		"ssh other user":      "ssh://deploy@gitlab.com/group/project.git",
		"ssh password":        "ssh://git:secret@gitlab.com/group/project.git",
		"ssh port":            "ssh://git@gitlab.com:2222/group/project.git",
		"https port":          "https://gitlab.com:8443/group/project.git",
		"scp other user":      "deploy@gitlab.com:group/project.git",
		"local absolute path": "/srv/git/project.git",
		"local relative path": "../project",
		"windows path":        `C:\repos\project`,
		"file url":            "file:///srv/git/project.git",
		"file scheme only":    "file:/srv/git/project.git",
		"http":                "http://gitlab.com/group/project.git",
		"git protocol":        "git://gitlab.com/group/project.git",
		"no dot host":         "https://gitlab/group/project.git",
		"scp no dot host":     "git@gitlab:group/project.git",
		"localhost":           "ssh://git@localhost/group/project.git",
		"no path":             "https://gitlab.com/",
		"host only":           "https://gitlab.com",
		"dot dot":             "https://gitlab.com/group/../project.git",
		"dot segment":         "git@gitlab.com:group/./project.git",
		"query":               "https://gitlab.com/group/project.git?ref=main",
		"fragment":            "https://gitlab.com/group/project.git#main",
		"percent encoding":    "https://gitlab.com/group/%2e%2e/project.git",
		"whitespace":          "https://gitlab.com/group/my project.git",
		"newline":             "https://gitlab.com/group/project.git\nx",
		"empty segment":       "https://gitlab.com/group//project.git",
		"scp absolute path":   "git@gitlab.com:/srv/project.git",
		"overlong":            "https://gitlab.com/" + strings.Repeat("a", 1100),
		"identity too long":   "https://gitlab.com/" + strings.Repeat("a", 300),
		"kelvin sign host":    "https://Kitlab.com/group/project.git",
		"non-ASCII scp host":  "git@gitläb.com:group/project.git",
		"kelvin sign path":    "git@gitlab.com:group/proKject.git",
		"non-ASCII ssh path":  "ssh://git@gitlab.com/gröup/project.git",
		"double .git":         "https://gitlab.com/group/project.git.git",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ForgeIdentityFromRemote(remote)
			if err == nil || got != "" {
				t.Fatalf("got %q, %v; want a refusal", got, err)
			}
			if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("the refusal echoes credentials: %v", err)
			}
		})
	}
}

func TestForgeIdentityFromRemoteRefusesNonASCIIBeforeCaseFolding(t *testing.T) {
	// U+212A KELVIN SIGN lower-cases to an ASCII "k"; it must be refused as
	// non-ASCII rather than folded into gitlab.com.
	for _, remote := range []string{"https://gitlaK.com/group/project.git", "git@gitlab.com:group/K.git"} {
		got, err := ForgeIdentityFromRemote(remote)
		if err == nil || got != "" || !strings.Contains(err.Error(), "ASCII") {
			t.Fatalf("%q: got %q, %v; want a non-ASCII refusal", remote, got, err)
		}
	}
}

func TestGitHubRepoFromRemoteIsUnchangedByForgeIdentity(t *testing.T) {
	if repo, ok := GitHubRepoFromRemote("https://github.com/Owner/Repo.git"); !ok || repo != "Owner/Repo" {
		t.Fatalf("got %q, %v", repo, ok)
	}
	if _, ok := GitHubRepoFromRemote("https://gitlab.com/owner/repo.git"); ok {
		t.Fatal("a GitLab remote became a GitHub repository")
	}
}
