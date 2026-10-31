package onboarding

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
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

// maxForgeRemote bounds the remote URL ForgeIdentityFromRemote reads.
const maxForgeRemote = 1024

// ForgeIdentityFromRemote returns the lower-case host/path identity of a
// plugin-forge remote, without a trailing .git, for the forms
// https://host/path, ssh://[git@]host/path and git@host:path. Credentialed URLs,
// ports, queries, fragments, percent-encoding, local paths, file: and other
// schemes, and hosts without a dot are refused. The result is a valid
// forge.identity. It reads nothing and never contacts the remote.
func ForgeIdentityFromRemote(remote string) (string, error) {
	if remote == "" || len(remote) > maxForgeRemote {
		return "", fmt.Errorf("origin remote must be a non-empty URL of at most %d bytes", maxForgeRemote)
	}
	// Checked before any lower-casing, so no Unicode case folding (such as the
	// Kelvin sign folding to "k") can turn a non-ASCII host or path into ASCII.
	for i := 0; i < len(remote); i++ {
		if remote[i] >= 0x80 {
			return "", fmt.Errorf("origin remote must be ASCII; a non-ASCII host or path is refused")
		}
	}
	if strings.ContainsAny(remote, " \t\r\n\x00\\?#%") {
		return "", fmt.Errorf("origin remote must not contain whitespace, a backslash, a query, a fragment or percent-encoding")
	}
	lower := strings.ToLower(remote)
	var host, rest string
	switch {
	case strings.HasPrefix(lower, "https://"):
		host, rest, _ = strings.Cut(remote[len("https://"):], "/")
		if strings.Contains(host, "@") {
			return "", fmt.Errorf("origin remote carries credentials; remove them from the URL")
		}
	case strings.HasPrefix(lower, "ssh://"):
		host, rest, _ = strings.Cut(remote[len("ssh://"):], "/")
		if user, h, ok := strings.Cut(host, "@"); ok {
			if user != "git" {
				return "", fmt.Errorf("origin remote carries credentials or a user other than git; remove them from the URL")
			}
			host = h
		}
	case strings.HasPrefix(lower, "file:"):
		return "", fmt.Errorf("origin remote is a local file: URL, not a forge")
	case strings.Contains(remote, "://"):
		return "", fmt.Errorf("origin remote must use https, ssh or the git@host:path form")
	default:
		user, after, ok := strings.Cut(remote, "@")
		if !ok || user != "git" {
			return "", fmt.Errorf("origin remote is a local path or an unsupported form; use https, ssh or git@host:path")
		}
		h, p, ok := strings.Cut(after, ":")
		if !ok || strings.Contains(h, "/") {
			return "", fmt.Errorf("origin remote is a local path or an unsupported form; use https, ssh or git@host:path")
		}
		host, rest = h, p
	}
	if strings.ContainsAny(host, "@:") {
		return "", fmt.Errorf("origin remote must not carry credentials or a port")
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	identity := strings.ToLower(host + "/" + rest)
	if why := formula.ValidForgeIdentity(identity); why != "" {
		return "", fmt.Errorf("origin remote does not name a forge project: identity %s", why)
	}
	return identity, nil
}
