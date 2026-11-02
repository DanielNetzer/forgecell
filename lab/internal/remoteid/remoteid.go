// Package remoteid derives the forge identity of a Git remote URL. It is a leaf
// package: it imports only the standard library, so the onboarding, molecule and
// delivery packages can all use it without an import cycle.
package remoteid

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxRemote bounds the remote URL FromRemote reads.
const MaxRemote = 1024

// maxIdentity bounds the identity, as formula.MaxForgeIdentity does.
const maxIdentity = 256

var (
	identityHost    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	identitySegment = regexp.MustCompile(`^[a-z0-9_.~-]+$`)
)

// FromRemote returns the lower-case host/path identity of a plugin-forge
// remote, without a trailing .git, for the forms https://host/path,
// ssh://[git@]host/path and git@host:path. Credentialed URLs, ports, queries,
// fragments, percent-encoding, local paths, file: and other schemes, and hosts
// without a dot are refused. The result is a valid forge.identity. It reads
// nothing and never contacts the remote.
func FromRemote(remote string) (string, error) {
	if remote == "" || len(remote) > MaxRemote {
		return "", fmt.Errorf("origin remote must be a non-empty URL of at most %d bytes", MaxRemote)
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
	if why := validIdentity(identity); why != "" {
		return "", fmt.Errorf("origin remote does not name a forge project: identity %s", why)
	}
	return identity, nil
}

// validIdentity is formula.ValidForgeIdentity, kept here so this package stays
// a leaf. The two must agree; remoteid_test.go pins the shared shapes.
func validIdentity(identity string) string {
	if identity == "" || len(identity) > maxIdentity {
		return fmt.Sprintf("must be a non-empty host/path of at most %d characters", maxIdentity)
	}
	parts := strings.Split(identity, "/")
	if len(parts) < 2 || !identityHost.MatchString(parts[0]) {
		return "must be a lower-case host/path whose host contains a dot, such as gitlab.com/group/project"
	}
	for _, seg := range parts[1:] {
		if seg == "." || seg == ".." || strings.Contains(seg, "..") || !identitySegment.MatchString(seg) {
			return "path segments must be non-empty lower-case letters, digits, '.', '_', '~' or '-', without '..'"
		}
	}
	if strings.HasSuffix(parts[len(parts)-1], ".git") {
		return "must name the project without a trailing .git"
	}
	return ""
}
