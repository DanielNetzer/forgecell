package onboarding

import "testing"

func TestGitHubRepoFromRemote(t *testing.T) {
	accepted := map[string]string{
		"https://github.com/Owner/Repo":         "Owner/Repo",
		"https://github.com/Owner/Repo.git":     "Owner/Repo",
		"https://github.com/Owner/Repo/":        "Owner/Repo",
		"https://github.com/Owner/Repo.git/":    "Owner/Repo",
		"git@github.com:Owner/Repo.git":         "Owner/Repo",
		"git@github.com:Owner/Repo":             "Owner/Repo",
		"ssh://git@github.com/Owner/Repo.git":   "Owner/Repo",
		"HTTPS://GitHub.com/owner/repo.git":     "owner/repo",
		"https://github.com/my-org/dot.repo.js": "my-org/dot.repo.js",
		"https://github.com/o_w/r_p-1":          "o_w/r_p-1",
	}
	for remote, want := range accepted {
		got, ok := GitHubRepoFromRemote(remote)
		if !ok || got != want {
			t.Errorf("GitHubRepoFromRemote(%q) = %q, %t; want %q", remote, got, ok, want)
		}
	}
	rejected := []string{
		"",
		"   ",
		"https://github.com/Owner",
		"https://github.com/Owner/",
		"https://github.com/Owner/Repo/extra",
		"https://gitlab.com/Owner/Repo.git",
		"git@gitlab.com:Owner/Repo.git",
		"http://github.com/Owner/Repo",
		"git://github.com/Owner/Repo",
		"https://user:token@github.com/Owner/Repo.git",
		"https://github.com.evil.example/Owner/Repo",
		"/tmp/Owner/Repo",
		"../Repo",
		"Owner/Repo",
		"https://github.com/./Repo",
		"https://github.com/../Repo",
		"https://github.com/Owner/.",
		"https://github.com/Owner/..",
		"https://github.com/Owner/.git",
		"https://github.com/Owner/Repo\n",
	}
	for _, remote := range rejected {
		if got, ok := GitHubRepoFromRemote(remote); ok {
			t.Errorf("GitHubRepoFromRemote(%q) = %q; want rejection", remote, got)
		}
	}
}
