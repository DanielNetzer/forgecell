package molecule

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// DisplayKey is how the Issue is shown to the operator: the tracker's own Key
// when it set one, otherwise GitHub's "#N".
func (i Issue) DisplayKey() string {
	if i.Key != "" {
		return i.Key
	}
	return "#" + strconv.FormatInt(i.Number, 10)
}

// IsKeyed reports an Issue a tracker identifies by its own Key; its Number is 0.
func (i Issue) IsKeyed() bool { return i.Key != "" }

// maxKeySlug bounds the slug part of keyed Molecule ids, workspaces and locks.
const maxKeySlug = 40

// KeySlug is the Key as a name part: lower-case, every rune outside [a-z0-9-]
// mapped to '-', runs of '-' collapsed, trimmed, and at most maxKeySlug bytes. It
// is empty when Key is empty or has no usable character; callers refuse that.
func (i Issue) KeySlug() string {
	var b strings.Builder
	dash := false
	for _, r := range i.Key {
		if b.Len() > maxKeySlug {
			break
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := b.String()
	if len(s) > maxKeySlug {
		s = s[:maxKeySlug]
	}
	return strings.Trim(s, "-")
}

// issueIdentified reports a record Issue with a usable identity: a GitHub number
// (Number >= 1, no Key) or a keyed Issue (Number 0, a Key with a usable slug).
func issueIdentified(i Issue) bool {
	if i.IsKeyed() {
		return i.Number == 0 && i.KeySlug() != ""
	}
	return i.Number >= 1
}

// errUnusableKey is the plain reason a keyed Issue whose Key slugs to nothing is
// refused before any ledger, workspace or lock is named after it.
var errUnusableKey = fmt.Errorf("issue key has no letters or digits to name a Molecule by")

// keySlug returns the Issue's usable slug or errUnusableKey.
func keySlug(i Issue) (string, error) {
	slug := i.KeySlug()
	if slug == "" {
		return "", errUnusableKey
	}
	return slug, nil
}

// RunKeyed is Run for an Issue a tracker identifies by key (for example ENG-1234)
// instead of a GitHub number. The ledger records Issue{Key, Number: 0}, the id is
// mol-<slug>-<nonce> and the workspace issue-<slug>-<nonce>. It reads the Issue
// only through o.ReadIssueKey and ignores o.Issue. Approving a keyed Molecule is
// not supported yet, so o.Approve must be empty.
func RunKeyed(ctx context.Context, o Options, key string) (Record, error) {
	if err := intake.ValidateIssueKey(key); err != nil {
		return Record{}, err
	}
	if _, err := keySlug(Issue{Key: key}); err != nil {
		return Record{}, err
	}
	if o.ReadIssueKey == nil {
		return Record{}, fmt.Errorf("a keyed run needs an Issue key reader")
	}
	if o.Approve != "" {
		return Record{}, fmt.Errorf("approving a keyed Molecule is not supported yet")
	}
	return runIntake(ctx, o, key)
}

// readKeyedIssueMatches checks a keyed reader's response against the request: the
// exact key with Number 0, the repository, an open state and an https URL. The
// GitHub Issue URL check does not apply to a keyed Issue.
func readKeyedIssueMatches(i Issue, repo, key string) bool {
	return i.Key == key && i.Number == 0 && i.Repo == repo && i.State == "OPEN" && httpsURL(i.URL)
}

// planWorkspaceKey is planWorkspace for a keyed Issue: issue-<slug>-<nonce>.
func planWorkspaceKey(o Options, w Workspace, i Issue) (Workspace, error) {
	slug, err := keySlug(i)
	if err != nil {
		return w, err
	}
	nonce, err := token()
	if err != nil {
		return w, err
	}
	name := "issue-" + slug + "-" + nonce
	w.Branch = "forgecell/" + name
	w.Path = filepath.Join(o.LabDir, "workspaces", name)
	return w, nil
}

// httpsURL reports an absolute https URL with a host and no user information, of
// bounded length. A keyed Issue's URL is checked this way instead of against the
// GitHub Issue URL.
func httpsURL(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// planIssueWorkspace plans the workspace by the Issue's form: keyed or numeric.
func planIssueWorkspace(o Options, w Workspace, i Issue) (Workspace, error) {
	if i.IsKeyed() {
		return planWorkspaceKey(o, w, i)
	}
	return planWorkspace(o, w, i.Number)
}
