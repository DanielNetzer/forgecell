package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// maxLabEntries bounds the Lab directory scan. A truncated scan cannot prove a
// match is unique, so resolution refuses instead of guessing.
var maxLabEntries = 1024

var labNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// labCommands take a Lab and so resolve a default Lab and accept --lab-name.
var labCommands = map[string]bool{"init": true, "doctor": true, "run": true, "ledger": true, "amend": true, "recover": true, "issues": true, "learn": true, "suggestion": true, "deliver": true, "checks": true}

// originRemote reads the checkout's origin URL from local git configuration only.
var originRemote = func(ctx context.Context, root string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: []string{"git", "remote", "get-url", "origin"}, Dir: root, Stdin: []byte{}, Timeout: 5 * time.Second, MaxOutputBytes: 4096})
	if !r.OK {
		return "", fmt.Errorf("%s", strings.TrimSpace(r.Error+" "+r.Stderr))
	}
	return strings.TrimSpace(r.Stdout), nil
}

type labResolution struct {
	Dir    string
	Notice string
}

// shown makes a directory name or an error text read from disk safe for a
// terminal: escape sequences, control and direction-override characters are gone.
// Only messages use it; a resolved directory is always returned as it is.
func shown(s string) string {
	return harness.ReviewDisplayText(s, 1024)
}

func shownAll(items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = shown(item)
	}
	return out
}

// labArgs separates --lab-name from the command's own flags and reports whether
// --lab was given explicitly. Scanning stops at a bare "--".
func labArgs(args []string) (rest []string, name string, explicitLab bool, err error) {
	rest = append(rest, args[0])
	named := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case arg == "--lab" || arg == "-lab" || strings.HasPrefix(arg, "--lab=") || strings.HasPrefix(arg, "-lab="):
			explicitLab = true
		case arg == "--lab-name" || arg == "-lab-name" || strings.HasPrefix(arg, "--lab-name=") || strings.HasPrefix(arg, "-lab-name="):
			if named {
				return nil, "", false, fmt.Errorf("--lab-name may be given only once")
			}
			named = true
			if _, value, ok := strings.Cut(arg, "="); ok {
				name = value
			} else if i+1 < len(args) {
				i++
				name = args[i]
			} else {
				return nil, "", false, fmt.Errorf("--lab-name requires a name")
			}
			if !labNamePattern.MatchString(name) {
				return nil, "", false, fmt.Errorf("invalid --lab-name %q: use 1-32 letters, digits, - or _", name)
			}
			continue
		}
		rest = append(rest, arg)
	}
	if named && explicitLab {
		return nil, "", false, fmt.Errorf("--lab and --lab-name cannot be combined: --lab DIR already chooses the Lab, so --lab-name would be ignored; drop one of them")
	}
	return rest, strings.ToLower(name), explicitLab, nil
}

// resolveLab is resolveDefaultLab; a variable only so tests can observe it.
var resolveLab = resolveDefaultLab

// resolveDefaultLab keys the default Lab by the checkout's GitHub identity
// rather than its path. It only reads local state and never creates anything.
func resolveDefaultLab(ctx context.Context, name string) (labResolution, error) {
	if os.Getenv("FORGECELL_LAB") != "" {
		if name != "" {
			return labResolution{}, fmt.Errorf("--lab-name cannot be combined with FORGECELL_LAB, which already chooses the Lab; unset FORGECELL_LAB or drop --lab-name")
		}
		dir, err := defaultLabDir()
		return labResolution{Dir: dir}, err
	}
	root, err := resolvedCheckoutRoot()
	if err != nil {
		return labResolution{}, err
	}
	repo, forgeIdentity, why := checkoutOrigin(ctx, root)
	if repo == "" {
		var forgeNotice string
		if forgeIdentity != "" {
			found, ok, err := resolveForgeLab(root, forgeIdentity, name)
			if err != nil || ok {
				return found, err
			}
			forgeNotice = found.Notice
		}
		if name != "" {
			return labResolution{}, fmt.Errorf("--lab-name needs a GitHub origin remote (%s); use --lab DIR to choose a Lab explicitly", why)
		}
		dir, err := defaultLabDir()
		if err != nil {
			return labResolution{}, err
		}
		return labResolution{Dir: dir, Notice: joinNotices(fmt.Sprintf("%s; using path-keyed Lab %s", why, shown(dir)), forgeNotice)}, nil
	}
	home, err := forgecellHome(root)
	if err != nil {
		return labResolution{}, err
	}
	labs := filepath.Join(home, "labs")
	identity := filepath.Join(labs, identityLabName(repo, name))
	info, statErr := os.Lstat(identity)
	switch {
	case statErr == nil && !info.IsDir():
		return labResolution{}, fmt.Errorf("Lab path %s exists but is not a real directory (a symbolic link or file is never followed); remove it or choose a Lab with --lab DIR", shown(identity))
	case statErr != nil && !os.IsNotExist(statErr):
		return labResolution{}, fmt.Errorf("Lab path %s cannot be inspected (%s); choose a Lab with --lab DIR", shown(identity), shown(statErr.Error()))
	}
	// A Lab created before owner--repo naming sits under the ambiguous legacy name.
	// While the new-format directory is absent, a legacy directory that might be
	// this repository's must be settled before anything is chosen or created, so a
	// second Lab never appears beside it.
	legacy := filepath.Join(labs, legacyLabName(repo, name))
	legacyMatch := false
	if statErr != nil {
		if legacyMatch, err = legacyLabFor(legacy, repo, identity); err != nil {
			return labResolution{}, err
		}
	}
	var notice string
	if name == "" {
		entries, truncated, err := scanLabs(labs)
		if err != nil {
			return labResolution{}, fmt.Errorf("cannot scan Labs under %s (%s); use --lab DIR to choose one", shown(labs), shown(err.Error()))
		}
		if truncated {
			return labResolution{}, fmt.Errorf("more than %d entries under %s, so a unique Lab for %s cannot be established; use --lab DIR to choose one", maxLabEntries, shown(labs), repo)
		}
		var matches, unreadable []string
		for _, e := range entries {
			switch {
			case e.Err != "":
				unreadable = append(unreadable, fmt.Sprintf("%s (%s)", shown(e.Dir), shown(e.Err)))
			case strings.EqualFold(e.Repo, repo):
				matches = append(matches, e.Dir)
			}
		}
		notice = unreadableLabsNotice(repo, unreadable)
		identityMatches := slices.Contains(matches, identity)
		switch {
		case identityMatches && len(matches) == 1:
			return labResolution{Dir: identity, Notice: notice}, nil
		case identityMatches:
			return labResolution{Dir: identity, Notice: joinNotices(notice, fmt.Sprintf("using Lab %s for %s; other Labs also match it: %s", shown(identity), repo, strings.Join(shownAll(withoutDir(matches, identity)), ", ")))}, nil
		case legacyMatch && len(matches) > 1 && slices.Contains(matches, legacy):
			return labResolution{Dir: legacy, Notice: joinNotices(notice, fmt.Sprintf("using Lab %s for %s, which uses the earlier Lab naming, so consider renaming it to %s; other Labs also match it: %s", shown(legacy), repo, shown(identity), strings.Join(shownAll(withoutDir(matches, legacy)), ", ")))}, nil
		case len(matches) > 1:
			return labResolution{}, fmt.Errorf("repository %s matches more than one Lab; choose one with --lab DIR:\n  %s", repo, strings.Join(shownAll(matches), "\n  "))
		case len(matches) == 1:
			found := fmt.Sprintf("using existing Lab %s for %s (its Formula's intake.repo matches); pass --lab DIR to choose another", shown(matches[0]), repo)
			if matches[0] == legacy {
				found += fmt.Sprintf("; it uses the earlier Lab naming, so consider renaming it to %s", shown(identity))
			}
			return labResolution{Dir: matches[0], Notice: joinNotices(notice, found)}, nil
		}
	} else if legacyMatch {
		return labResolution{Dir: legacy, Notice: fmt.Sprintf("using existing Lab %s for %s (its Formula's intake.repo matches); it uses the earlier Lab naming, so consider renaming it to %s", shown(legacy), repo, shown(identity))}, nil
	}
	if statErr == nil {
		if e := inspectLab(identity); e.Err == "" && e.Repo != "" && !strings.EqualFold(e.Repo, repo) {
			return labResolution{}, fmt.Errorf("Lab directory %s belongs to repository %s, not %s; choose a different Lab with --lab-name NAME or --lab DIR", shown(identity), shown(e.Repo), repo)
		}
	}
	return labResolution{Dir: identity, Notice: notice}, nil
}

// legacyLabFor probes the exact legacy directory for a repository directly, not
// through the Lab scan. It reports whether that directory is this repository's
// Lab. A directory that might be, but cannot be confirmed (unreadable, corrupt,
// linked, or without a lab.json yet because onboarding is pending), is refused:
// returning the new-format sibling there would split one repository's state
// across two Labs.
func legacyLabFor(legacy, repo, identity string) (bool, error) {
	refuse := func(why string) (bool, error) {
		return false, fmt.Errorf("%s uses the earlier Lab naming and %s, so it may be the Lab for %s, and no new Lab %s was chosen beside it; choose a Lab with --lab DIR, repair it, or rename it to %s", shown(legacy), shown(why), repo, shown(identity), shown(identity))
	}
	info, err := os.Lstat(legacy)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return refuse("cannot be inspected (" + err.Error() + ")")
	case info.Mode()&os.ModeSymlink != 0:
		return refuse("is a symbolic link, which is never followed")
	case !info.IsDir():
		return false, nil
	}
	if _, err = os.Lstat(filepath.Join(legacy, "lab.json")); os.IsNotExist(err) {
		return refuse("has no lab.json yet (an onboarding proposal may be pending)")
	}
	e := inspectLab(legacy)
	if e.Err != "" {
		return refuse("cannot be read (" + e.Err + ")")
	}
	return strings.EqualFold(e.Repo, repo), nil
}

// unreadableLabsNotice names up to five Labs a scan could not read, so a
// resolution never skips them silently. It is empty when every Lab was read.
func unreadableLabsNotice(subject string, unreadable []string) string {
	if len(unreadable) == 0 {
		return ""
	}
	notice := fmt.Sprintf("these Labs could not be read, so none was considered for %s: %s; fix or remove them, or choose a Lab with --lab DIR", subject, strings.Join(unreadable[:min(len(unreadable), 5)], "; "))
	if more := len(unreadable) - 5; more > 0 {
		notice += fmt.Sprintf(" (and %d more)", more)
	}
	return notice
}

func withoutDir(items []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(items), func(item string) bool { return item == drop })
}

func joinNotices(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// checkoutRepository returns OWNER/REPO from origin, or why none is available.
func checkoutRepository(ctx context.Context, root string) (repo, why string) {
	repo, _, why = checkoutOrigin(ctx, root)
	return repo, why
}

// checkoutOrigin reads origin once. It tries GitHub first, exactly as
// checkoutRepository always has; only when origin is not GitHub does it fall back
// to the plugin-forge host/path identity. why is unchanged either way, so a
// checkout with no Lab for its forge identity keeps today's path-keyed fallback.
func checkoutOrigin(ctx context.Context, root string) (repo, forgeIdentity, why string) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		return "", "", "not inside a git checkout"
	}
	remote, err := originRemote(ctx, root)
	if err != nil || remote == "" {
		return "", "", "no origin remote could be read"
	}
	repo, ok := onboarding.GitHubRepoFromRemote(remote)
	if !ok {
		identity, _ := onboarding.ForgeIdentityFromRemote(remote)
		return "", identity, "origin is not a GitHub repository"
	}
	return repo, "", ""
}

// maxLabDirName is the longest Lab directory name resolution will look up.
const maxLabDirName = 255

// forgeLabName replaces each "/" of a forge identity with "--", adds "-" and the
// first 8 hex characters of the identity's sha256, and adds a Lab name with "@".
// A segment may itself contain "--", so host--path alone is ambiguous
// (h.com/a--b/c and h.com/a/b--c); the digest keeps such identities apart. The
// host always contains a dot and a GitHub owner never does, so it cannot equal a
// GitHub owner--repo name. A Lab under it is still used only after its lab.json
// confirms the identity.
func forgeLabName(identity, name string) string {
	sum := sha256.Sum256([]byte(identity))
	label := strings.ReplaceAll(identity, "/", "--") + "-" + hex.EncodeToString(sum[:])[:8]
	if name != "" {
		label += "@" + strings.ToLower(name)
	}
	return label
}

// resolveForgeLab finds the Lab of a plugin-forge checkout by its host/path
// identity. ok is false when no Lab declares that identity, so the caller keeps
// its path-keyed fallback and shows the returned notice. The canonical directory
// is used only after its lab.json verifies a Formula whose forge.identity
// matches; one whose verified Formula declares another identity is skipped for
// the identity scan, and one that cannot be verified, or is linked, is refused,
// never adopted. A scan that meets an unreadable Lab names it, and without a
// match it refuses rather than fall back beside a Lab that may hold the
// identity. It only reads local state and never starts a plugin.
func resolveForgeLab(root, identity, name string) (labResolution, bool, error) {
	home, err := forgecellHome(root)
	if err != nil {
		return labResolution{}, false, err
	}
	labs := filepath.Join(home, "labs")
	label := forgeLabName(identity, name)
	if len(label) > maxLabDirName {
		return labResolution{}, false, fmt.Errorf("the Lab directory name for %s is longer than %d bytes; choose a Lab with --lab DIR", identity, maxLabDirName)
	}
	dir := filepath.Join(labs, label)
	info, statErr := os.Lstat(dir)
	canonical := false
	var notice string
	switch {
	case statErr == nil && !info.IsDir():
		return labResolution{}, false, fmt.Errorf("Lab path %s exists but is not a real directory (a symbolic link or file is never followed); remove it or choose a Lab with --lab DIR", shown(dir))
	case statErr != nil && !os.IsNotExist(statErr):
		return labResolution{}, false, fmt.Errorf("Lab path %s cannot be inspected (%s); choose a Lab with --lab DIR", shown(dir), shown(statErr.Error()))
	case statErr == nil:
		if _, err := os.Lstat(filepath.Join(dir, "lab.json")); err != nil {
			return labResolution{}, false, fmt.Errorf("Lab directory %s has no readable lab.json, so it cannot be verified as the Lab for %s; choose a Lab with --lab DIR or repair it", shown(dir), identity)
		}
		e := inspectLab(dir)
		if e.Err != "" {
			return labResolution{}, false, fmt.Errorf("Lab directory %s cannot be verified as the Lab for %s (%s); choose a Lab with --lab DIR or repair it", shown(dir), identity, shown(e.Err))
		}
		if e.Identity == identity {
			canonical = true
		} else {
			owner := "no forge identity"
			if e.Identity != "" {
				owner = "forge identity " + shown(e.Identity)
			}
			notice = fmt.Sprintf("Lab directory %s belongs to %s, not %s, so it was not used", shown(dir), owner, identity)
		}
	}
	if name != "" {
		if canonical {
			return labResolution{Dir: dir}, true, nil
		}
		return labResolution{}, false, fmt.Errorf("no Lab named %s exists for forge identity %s (looked for %s); use --lab DIR to choose a Lab explicitly", name, identity, shown(dir))
	}
	entries, truncated, err := scanLabs(labs)
	if err != nil {
		return labResolution{}, false, fmt.Errorf("cannot scan Labs under %s (%s); use --lab DIR to choose one", shown(labs), shown(err.Error()))
	}
	if truncated {
		return labResolution{}, false, fmt.Errorf("more than %d entries under %s, so a unique Lab for %s cannot be established; use --lab DIR to choose one", maxLabEntries, shown(labs), identity)
	}
	var matches, unreadable []string
	for _, e := range entries {
		switch {
		case e.Err != "":
			unreadable = append(unreadable, fmt.Sprintf("%s (%s)", shown(e.Dir), shown(e.Err)))
		case e.Identity == identity:
			matches = append(matches, e.Dir)
		}
	}
	unread := unreadableLabsNotice(identity, unreadable)
	notice = joinNotices(notice, unread)
	switch {
	case canonical && len(matches) > 1:
		return labResolution{Dir: dir, Notice: joinNotices(notice, fmt.Sprintf("using Lab %s for %s; other Labs also match it: %s", shown(dir), identity, strings.Join(shownAll(withoutDir(matches, dir)), ", ")))}, true, nil
	case canonical:
		return labResolution{Dir: dir, Notice: notice}, true, nil
	case len(matches) > 1:
		return labResolution{}, false, fmt.Errorf("forge identity %s matches more than one Lab; choose one with --lab DIR:\n  %s", identity, strings.Join(shownAll(matches), "\n  "))
	case len(matches) == 1:
		return labResolution{Dir: matches[0], Notice: joinNotices(notice, fmt.Sprintf("using existing Lab %s for %s (its Formula's forge.identity matches); pass --lab DIR to choose another", shown(matches[0]), identity))}, true, nil
	case unread != "":
		// An unreadable Lab may be this identity's, so no path-keyed Lab is
		// chosen beside it.
		return labResolution{}, false, fmt.Errorf("no readable Lab declares forge identity %s and %s", identity, notice)
	}
	return labResolution{Notice: notice}, false, nil
}

// identityLabName joins owner and repository with "--" and a Lab name with "@".
// GitHub owner names cannot contain "--" or "@", so no two distinct repository
// and name pairs share a directory the way owner "a-b" with repository "c" and
// owner "a" with repository "b-c" once did.
func identityLabName(repo, name string) string {
	label := strings.ToLower(strings.Replace(repo, "/", "--", 1))
	if name != "" {
		label += "@" + strings.ToLower(name)
	}
	return label
}

// legacyLabName is the directory name Labs were given before owner--repo naming.
// It is ambiguous, so a Lab found under it is used only after its lab.json
// confirms the repository.
func legacyLabName(repo, name string) string {
	label := strings.ToLower(strings.Replace(repo, "/", "-", 1))
	if name != "" {
		label += "-" + strings.ToLower(name)
	}
	return label
}

type labEntry struct {
	Dir       string
	Repo      string
	Identity  string
	FormulaID string
	Binding   string
	Err       string
	Linked    bool
}

// scanLabs lists the directories directly under the labs directory without
// following symbolic links, sorted by name. Missing storage is an empty list.
func scanLabs(labs string) ([]labEntry, bool, error) {
	dir, err := os.Open(labs)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(maxLabEntries + 1)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	sort.Strings(names)
	truncated := len(names) > maxLabEntries
	if truncated {
		names = names[:maxLabEntries]
	}
	var entries []labEntry
	for _, name := range names {
		path := filepath.Join(labs, name)
		info, err := os.Lstat(path)
		switch {
		case err != nil:
			entries = append(entries, labEntry{Dir: path, Err: err.Error()})
		case info.Mode()&os.ModeSymlink != 0:
			entries = append(entries, labEntry{Dir: path, Err: "symbolic link is not followed", Linked: true})
		case info.IsDir():
			entries = append(entries, inspectLab(path))
		}
	}
	return entries, truncated, nil
}

// inspectLab reads only the active Formula's identity. It never approves or executes anything.
func inspectLab(dir string) labEntry {
	e := labEntry{Dir: dir}
	info, err := os.Lstat(filepath.Join(dir, "lab.json"))
	if os.IsNotExist(err) {
		return e
	}
	if err != nil {
		e.Err = err.Error()
		return e
	}
	if !info.Mode().IsRegular() || info.Size() > 1_000_000 {
		e.Err = "lab.json must be a bounded regular file"
		return e
	}
	active, err := formula.Inspect(dir, "")
	if err != nil {
		e.Err = err.Error()
		return e
	}
	e.Repo, e.FormulaID, e.Binding = active.Formula.Intake.Repo, active.Formula.ID, active.Formula.Harness.Binding
	if active.Formula.Forge != nil {
		e.Identity = active.Formula.Forge.Identity
	}
	return e
}
