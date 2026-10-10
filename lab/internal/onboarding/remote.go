package onboarding

import (
	"regexp"
	"strings"
)

var githubRemote = regexp.MustCompile(`(?i)^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([a-z0-9_.-]+)/([a-z0-9_.-]+?)/?$`)

// GitHubRepoFromRemote returns OWNER/REPO for the supported GitHub remote forms,
// preserving case. Other hosts, credentialed URLs and local paths are rejected.
func GitHubRepoFromRemote(remote string) (string, bool) {
	m := githubRemote.FindStringSubmatch(remote)
	if m == nil {
		return "", false
	}
	owner, repo := m[1], strings.TrimSuffix(m[2], ".git")
	for _, part := range []string{owner, repo} {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return owner + "/" + repo, true
}
