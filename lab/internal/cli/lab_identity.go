package cli

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	return rest, strings.ToLower(name), explicitLab, nil
}

// resolveDefaultLab keys the default Lab by the checkout's GitHub identity
// rather than its path. It only reads local state and never creates anything.
func resolveDefaultLab(ctx context.Context, name string) (labResolution, error) {
	if os.Getenv("FORGECELL_LAB") != "" {
		dir, err := defaultLabDir()
		return labResolution{Dir: dir}, err
	}
	root, err := resolvedCheckoutRoot()
	if err != nil {
		return labResolution{}, err
	}
	repo, why := checkoutRepository(ctx, root)
	if repo == "" {
		if name != "" {
			return labResolution{}, fmt.Errorf("--lab-name needs a GitHub origin remote (%s); use --lab DIR to choose a Lab explicitly", why)
		}
		dir, err := defaultLabDir()
		if err != nil {
			return labResolution{}, err
		}
		return labResolution{Dir: dir, Notice: fmt.Sprintf("%s; using path-keyed Lab %s", why, dir)}, nil
	}
	home, err := forgecellHome(root)
	if err != nil {
		return labResolution{}, err
	}
	labs := filepath.Join(home, "labs")
	identity := filepath.Join(labs, identityLabName(repo, name))
	if name == "" {
		entries, truncated, err := scanLabs(labs)
		if err != nil {
			return labResolution{}, fmt.Errorf("cannot scan Labs under %s (%v); use --lab DIR to choose one", labs, err)
		}
		if truncated {
			return labResolution{}, fmt.Errorf("more than %d entries under %s, so a unique Lab for %s cannot be established; use --lab DIR to choose one", maxLabEntries, labs, repo)
		}
		var matches []string
		for _, e := range entries {
			if e.Err == "" && strings.EqualFold(e.Repo, repo) {
				matches = append(matches, e.Dir)
			}
		}
		switch {
		case len(matches) > 1:
			return labResolution{}, fmt.Errorf("repository %s matches more than one Lab; choose one with --lab DIR (or --lab-name NAME):\n  %s", repo, strings.Join(matches, "\n  "))
		case len(matches) == 1 && matches[0] == identity:
			return labResolution{Dir: identity}, nil
		case len(matches) == 1:
			return labResolution{Dir: matches[0], Notice: fmt.Sprintf("using existing Lab %s for %s (its Formula's intake.repo matches); pass --lab DIR to choose another", matches[0], repo)}, nil
		}
	}
	if info, err := os.Lstat(identity); err == nil && info.IsDir() {
		if e := inspectLab(identity); e.Err == "" && e.Repo != "" && !strings.EqualFold(e.Repo, repo) {
			return labResolution{}, fmt.Errorf("Lab directory %s belongs to repository %s, not %s; choose a different Lab with --lab-name NAME or --lab DIR", identity, e.Repo, repo)
		}
	}
	return labResolution{Dir: identity}, nil
}

// checkoutRepository returns OWNER/REPO from origin, or why none is available.
func checkoutRepository(ctx context.Context, root string) (repo, why string) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		return "", "not inside a git checkout"
	}
	remote, err := originRemote(ctx, root)
	if err != nil || remote == "" {
		return "", "no origin remote could be read"
	}
	repo, ok := onboarding.GitHubRepoFromRemote(remote)
	if !ok {
		return "", "origin is not a GitHub repository"
	}
	return repo, ""
}

func identityLabName(repo, name string) string {
	label := strings.ToLower(strings.Replace(repo, "/", "-", 1))
	if name != "" {
		label += "-" + strings.ToLower(name)
	}
	return label
}

type labEntry struct {
	Dir       string
	Repo      string
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
	return e
}
