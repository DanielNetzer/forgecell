// Package verification captures repository evidence separately from coding.
package verification

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

type CaptureResult struct {
	Tree       string              `json:"tree"`
	Changes    []string            `json:"changes"`
	Violations []string            `json:"violations"`
	Artifacts  []ArtifactInventory `json:"artifacts,omitempty"`
}

func gitEnv(index string) []string {
	env := []string{}
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if !strings.HasPrefix(name, "GIT_") {
			env = append(env, s)
		}
	}
	if index != "" {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	return env
}
func git(ctx context.Context, dir, index string, input []byte, args ...string) (string, error) {
	argv := []string{"git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
	argv = append(argv, args...)
	r := process.Run(ctx, process.Options{Argv: argv, Dir: dir, Env: gitEnv(index), Stdin: input, Timeout: 30 * time.Second, MaxOutputBytes: 4_000_000})
	if !r.OK {
		return "", fmt.Errorf("verification Git command failed: %s %s", r.Error, r.Stderr)
	}
	return r.Stdout, nil
}
func paths(raw string) []string {
	out := []string{}
	for _, s := range strings.Split(raw, "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
func regularAncestors(root, file string) error {
	for parent := path.Dir(file); parent != "."; parent = path.Dir(parent) {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(parent)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe ancestor %s", parent)
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(parent), ".git")); err == nil {
			return fmt.Errorf("nested repository %s", parent)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Capture uses a private index and hash-object --no-filters, never git add or the
// user's staging area. It records observed out-of-scope bytes without deleting
// them. This is a repository-tree audit, not a host or transient-write sandbox.
func Capture(ctx context.Context, dir, base string, allowed []string, artifacts []readiness.ArtifactRoot) (CaptureResult, error) {
	out := CaptureResult{Changes: []string{}, Violations: []string{}}
	head, err := git(ctx, dir, "", nil, "rev-parse", "HEAD")
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(head) != base {
		return out, fmt.Errorf("workspace HEAD changed from approved base")
	}
	trackedRaw, err := git(ctx, dir, "", nil, "ls-tree", "-r", "-z", base)
	if err != nil {
		return out, err
	}
	tracked := []string{}
	changed := []string{}
	for _, entry := range paths(trackedRaw) {
		header, file, ok := strings.Cut(entry, "\t")
		parts := strings.Fields(header)
		if !ok || len(parts) != 3 {
			return out, fmt.Errorf("invalid base tree entry")
		}
		tracked = append(tracked, file)
		if err := readiness.ValidatePath(file); err != nil {
			return out, err
		}
		if err := regularAncestors(dir, file); err != nil {
			return out, err
		}
		if parts[0] == "160000" {
			entries, e := os.ReadDir(filepath.Join(dir, filepath.FromSlash(file)))
			if e != nil && !os.IsNotExist(e) {
				return out, e
			}
			if len(entries) > 0 {
				return out, fmt.Errorf("initialized submodule requires separate review: %s", file)
			}
			continue
		}
		mode, data, e := workingBlob(dir, file)
		if e != nil {
			if os.IsNotExist(e) {
				changed = append(changed, file)
				continue
			}
			return out, e
		}
		var h hash.Hash = sha1.New()
		if len(parts[2]) == 64 {
			h = sha256.New()
		}
		fmt.Fprintf(h, "blob %d%c", len(data), 0)
		h.Write(data)
		object := fmt.Sprintf("%x", h.Sum(nil))
		if mode != parts[0] || object != parts[2] {
			changed = append(changed, file)
		}
	}
	// New candidate files may already be staged (including in our independent
	// verification index). They are absent from both the base and --others.
	cached, err := git(ctx, dir, "", nil, "ls-files", "--cached", "-z")
	if err != nil {
		return out, err
	}
	baseline := map[string]bool{}
	for _, file := range tracked {
		baseline[file] = true
	}
	for _, file := range paths(cached) {
		if !baseline[file] {
			changed = append(changed, file)
		}
	}
	allowedSet := map[string]bool{}
	for _, s := range allowed {
		if err := readiness.ValidatePath(s); err != nil {
			return out, err
		}
		allowedSet[s] = true
	}
	untrackedArgs := []string{"ls-files", "--others", "--full-name", "-z"}
	for _, a := range artifacts {
		if err := readiness.ValidatePath(a.Path); err != nil {
			return out, err
		}
		if err := regularAncestors(dir, a.Path); err != nil {
			return out, err
		}
		for _, s := range append(append([]string{}, tracked...), allowed...) {
			if s == a.Path || strings.HasPrefix(s, a.Path+"/") {
				return out, fmt.Errorf("artifact allowance overlaps source: %s", a.Path)
			}
		}
		if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(a.Path))); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return out, fmt.Errorf("artifact root must be a regular directory: %s", a.Path)
			}
		} else if !os.IsNotExist(err) {
			return out, err
		}
		inventory, err := inventoryArtifact(dir, a)
		if err != nil {
			return out, err
		}
		out.Artifacts = append(out.Artifacts, inventory)
		untrackedArgs = append(untrackedArgs, "--exclude="+a.Path+"/")
	}
	untracked, err := git(ctx, dir, "", nil, untrackedArgs...)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, file := range append(changed, paths(untracked)...) {
		if !seen[file] {
			seen[file] = true
			out.Changes = append(out.Changes, file)
		}
	}
	sort.Strings(out.Changes)
	if len(out.Changes) > 1000 {
		return out, fmt.Errorf("more than 1000 changed paths; explicit artifact allowances or review required")
	}
	temp, err := os.MkdirTemp("", "forgecell-capture-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(temp)
	index := filepath.Join(temp, "index")
	if _, err = git(ctx, dir, index, nil, "read-tree", base); err != nil {
		return out, err
	}
	for _, file := range out.Changes {
		if err := readiness.ValidatePath(file); err != nil {
			return out, err
		}
		if err := regularAncestors(dir, file); err != nil {
			return out, err
		}
		violation := !allowedSet[file]
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(file)))
		if os.IsNotExist(err) {
			if _, err = git(ctx, dir, index, nil, "update-index", "--force-remove", "--", file); err != nil {
				return out, err
			}
		} else if err != nil {
			return out, err
		} else {
			mode, data, err := workingBlob(dir, file)
			if err != nil {
				return out, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				violation = true
			}

			object, err := git(ctx, dir, index, data, "hash-object", "-w", "--no-filters", "--stdin")
			if err != nil {
				return out, err
			}
			// Separate argv arguments avoid ambiguities in paths containing commas.
			if _, err = git(ctx, dir, index, nil, "update-index", "--add", "--cacheinfo", mode, strings.TrimSpace(object), file); err != nil {
				return out, err
			}
		}
		if violation {
			out.Violations = append(out.Violations, file)
		}
	}
	tree, err := git(ctx, dir, index, nil, "write-tree")
	out.Tree = strings.TrimSpace(tree)
	return out, err
}

// Compare raw source bytes, independent of index assume-unchanged/skip-worktree
// flags and candidate-controlled .gitattributes clean filters.
func workingBlob(root, file string) (string, []byte, error) {
	target := filepath.Join(root, filepath.FromSlash(file))
	info, err := os.Lstat(target)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		value, err := os.Readlink(target)
		return "120000", []byte(value), err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("source must be a regular file: %s", file)
	}
	if info.Size() > 10_000_000 {
		return "", nil, fmt.Errorf("source file exceeds capture limit: %s", file)
	}
	mode := "100644"
	if info.Mode()&0111 != 0 {
		mode = "100755"
	}
	data, err := os.ReadFile(target)
	if len(data) > 10_000_000 {
		return "", nil, fmt.Errorf("source file grew beyond capture limit")
	}
	return mode, data, err
}
