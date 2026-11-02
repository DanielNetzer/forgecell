package onboarding

import (
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/remoteid"
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

// ForgeIdentityFromRemote returns the lower-case host/path identity of a
// plugin-forge remote, without a trailing .git, for the forms
// https://host/path, ssh://[git@]host/path and git@host:path. Credentialed URLs,
// ports, queries, fragments, percent-encoding, local paths, file: and other
// schemes, and hosts without a dot are refused. The result is a valid
// forge.identity. It reads nothing and never contacts the remote. The parsing
// lives in remoteid, so molecule and delivery share it.
func ForgeIdentityFromRemote(remote string) (string, error) {
	return remoteid.FromRemote(remote)
}
