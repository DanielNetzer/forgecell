package molecule

import (
	"context"
	"errors"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/remoteid"
)

// ErrNoForgeIdentity refuses a plugin-forge Formula whose forge block has no
// identity: its checkout origin cannot be checked, and it never falls back to
// the GitHub check.
var ErrNoForgeIdentity = errors.New("the Formula's forge block has no identity; re-run init --forge-plugin")

// ForgeOriginIdentity returns the forge identity a Formula's checkout origin
// must resolve to: forge.identity for a Formula with a forge: block, or "" for
// one without, whose origin keeps the GitHub check against intake.repo. A forge
// block without an identity is ErrNoForgeIdentity.
func ForgeOriginIdentity(f formula.Formula) (string, error) {
	if f.Forge == nil {
		return "", nil
	}
	if f.Forge.Identity == "" {
		return "", ErrNoForgeIdentity
	}
	return f.Forge.Identity, nil
}

// originURLsHaveIdentity reports whether every fetch and push URL of the
// checkout's origin remote (git remote get-url --all and --push --all, as
// delivery reads them) resolves to identity. A git failure, an empty list or
// any one other URL fails.
func originURLsHaveIdentity(ctx context.Context, dir, identity string) bool {
	for _, args := range [][]string{{"remote", "get-url", "--all", "origin"}, {"remote", "get-url", "--push", "--all", "origin"}} {
		urls, err := git(ctx, dir, args...)
		if err != nil || urls == "" {
			return false
		}
		for _, url := range strings.Split(urls, "\n") {
			if !OriginHasIdentity(strings.TrimSpace(url), identity) {
				return false
			}
		}
	}
	return true
}

// OriginHasIdentity reports whether the remote URL resolves, by
// remoteid.FromRemote, to identity, ignoring case. An empty identity or an
// unparsable remote never matches.
func OriginHasIdentity(remote, identity string) bool {
	if identity == "" {
		return false
	}
	got, err := remoteid.FromRemote(remote)
	return err == nil && strings.EqualFold(got, identity)
}
