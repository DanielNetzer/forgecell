package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// FingerprintInputs are the per-run inputs that an isolated native Claude run can
// still read. All are optional: without a ProjectRoot no project settings are
// hashed, and without a Model the run is unpinned.
type FingerprintInputs struct {
	// ProjectRoot is the source checkout (the git work tree root) the run's
	// workspace is created from. A ProjectRoot needs a BaseCommit.
	ProjectRoot string
	// BaseCommit is the full commit id the workspace is checked out at. The project
	// settings a run loads (--setting-sources project) are the ones committed
	// there, not whatever the source checkout's working tree holds, so they are read
	// from this commit.
	BaseCommit string
	// Model is the Formula-pinned model passed with --model.
	Model string
}

const gitTimeout = 20 * time.Second

// maxClaudeConfigFiles bounds the files examined under .claude.
const maxClaudeConfigFiles = 5000

var commitID = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// projectSettingsFiles are the project settings files a --setting-sources project
// run can load. settings.local.json is gitignored by Claude Code convention, so a
// workspace holds it only when it is committed, which this read sees too.
var projectSettingsFiles = []string{".claude/settings.json", ".claude/settings.local.json"}

func runGit(dir string, stdin []byte, args ...string) (string, error) {
	if stdin == nil {
		stdin = []byte{}
	}
	r := process.Run(context.Background(), process.Options{Argv: append([]string{"git"}, args...), Dir: dir, Stdin: stdin, Timeout: gitTimeout, MaxOutputBytes: 8 << 20})
	if !r.OK || r.Overflow || r.Truncated {
		return "", fmt.Errorf("git %s failed: %s %s", args[0], r.Error, strings.TrimSpace(r.Stderr))
	}
	return r.Stdout, nil
}

func requireCommit(commit string) error {
	if !commitID.MatchString(commit) {
		return fmt.Errorf("project Claude settings are read from a base commit, which must be a full commit id")
	}
	return nil
}

// committedProjectSettings digests the project settings files recorded at commit.
// An absent file contributes nothing, so absent settings have a stable digest that
// differs from every present one.
func committedProjectSettings(root, commit string) (string, error) {
	if err := requireCommit(commit); err != nil {
		return "", err
	}
	args := append([]string{"ls-tree", "--full-tree", "-z", commit, "--"}, projectSettingsFiles...)
	listing, err := runGit(root, nil, args...)
	if err != nil {
		return "", fmt.Errorf("cannot fingerprint project Claude settings: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(listing))), nil
}

// claudeBehaviorPath reports whether a repository path under .claude can change what
// a --setting-sources project run does: settings files, hooks and agents. A symlink
// standing in for .claude, .claude/hooks or .claude/agents counts too.
func claudeBehaviorPath(p string) bool {
	if match, _ := path.Match(".claude/settings*.json", p); match {
		return true
	}
	return p == ".claude" || p == ".claude/hooks" || p == ".claude/agents" || strings.HasPrefix(p, ".claude/hooks/") || strings.HasPrefix(p, ".claude/agents/")
}

func splitNUL(s string) []string {
	return slices.DeleteFunc(strings.Split(s, "\x00"), func(v string) bool { return v == "" })
}

// ClaudeProjectConfigAtCommit lists the files committed at commit that a
// --setting-sources project run would load or act on: .claude/settings*.json,
// .claude/hooks/** and .claude/agents/**. Hooks and permission rules there run or
// widen what the Lab approved, so such a run is refused rather than trusted.
func ClaudeProjectConfigAtCommit(root, commit string) ([]string, error) {
	if err := requireCommit(commit); err != nil {
		return nil, err
	}
	listing, err := runGit(root, nil, "ls-tree", "-r", "-z", "--full-tree", "--name-only", commit, "--", ".claude")
	if err != nil {
		return nil, err
	}
	found := slices.DeleteFunc(splitNUL(listing), func(p string) bool { return !claudeBehaviorPath(p) })
	slices.Sort(found)
	return found, nil
}

// walkClaude lists the non-directory entries under dir/.claude as slash paths
// relative to dir. A directory is never followed through a symlink.
func walkClaude(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(filepath.Join(dir, ".claude"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == filepath.Join(dir, ".claude") {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if files = append(files, filepath.ToSlash(rel)); len(files) > maxClaudeConfigFiles {
			return fmt.Errorf("more than %d entries under .claude", maxClaudeConfigFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ClaudeProjectConfigInDir is ClaudeProjectConfigAtCommit for a directory that has
// no base commit, such as the one a formula-improvement run starts in.
func ClaudeProjectConfigInDir(dir string) ([]string, error) {
	files, err := walkClaude(dir)
	if err != nil {
		return nil, err
	}
	found := slices.DeleteFunc(files, func(p string) bool { return !claudeBehaviorPath(p) })
	slices.Sort(found)
	return found, nil
}

// ClaudeTreeChanges lists the paths under .claude in the work tree at dir that
// differ from commit: created, deleted, or with different bytes. An empty list
// means .claude is byte-identical to the commit. It reads the work tree directly,
// so gitignored files are seen too.
func ClaudeTreeChanges(dir, commit string) ([]string, error) {
	if err := requireCommit(commit); err != nil {
		return nil, err
	}
	listing, err := runGit(dir, nil, "ls-tree", "-r", "-z", "--full-tree", commit, "--", ".claude")
	if err != nil {
		return nil, err
	}
	type entry struct{ mode, object string }
	base := map[string]entry{}
	for _, line := range splitNUL(listing) {
		meta, name, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unreadable git tree entry under .claude")
		}
		if fields[1] == "blob" {
			base[name] = entry{fields[0], fields[2]}
		}
	}
	files, err := walkClaude(dir)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	var regular []string
	for _, name := range files {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			changed[name] = true
			continue
		}
		want, tracked := base[name]
		switch {
		case !tracked:
			changed[name] = true
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil || want.mode != "120000" {
				changed[name] = true
				break
			}
			content, err := runGit(dir, nil, "cat-file", "blob", want.object)
			changed[name] = err != nil || content != target
		case !info.Mode().IsRegular() || strings.Contains(name, "\n") || strings.HasPrefix(name, "\""):
			changed[name] = true
		default:
			regular = append(regular, name)
		}
	}
	if len(regular) > 0 {
		hashed, err := runGit(dir, []byte(strings.Join(regular, "\n")+"\n"), "hash-object", "--stdin-paths")
		if err != nil {
			return nil, err
		}
		objects := strings.Fields(hashed)
		if len(objects) != len(regular) {
			return nil, fmt.Errorf("git hashed %d of %d files under .claude", len(objects), len(regular))
		}
		for i, name := range regular {
			if objects[i] != base[name].object {
				changed[name] = true
			}
		}
	}
	for name := range base {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			changed[name] = true
		}
	}
	out := make([]string, 0, len(changed))
	for name, differs := range changed {
		if differs {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Fingerprint binds approval to the provider executable and configuration, and for
// native adapters to the adapter protocol version and argv contract. The Forgecell
// build that serves a native adapter is provenance, not approval: upgrades within
// one protocol keep the approval. It hashes configuration, never copies its
// credentials into the ledger. It covers only what the run can read: for a native
// Claude binding that is the executable, the pinned model, the ANTHROPIC_*
// environment and, through FingerprintWith, the project settings committed at the
// base commit. User-level Claude settings, hooks and plugins are excluded by the
// run's isolation, so editing them does not change the fingerprint.
func Fingerprint(command []string) (string, error) {
	return FingerprintWith(command, FingerprintInputs{})
}

// FingerprintWith is Fingerprint with the per-run inputs of an isolated Claude run.
func FingerprintWith(command []string, in FingerprintInputs) (string, error) {
	return fingerprintWith(command, AdapterProtocolVersion, in)
}

func fingerprint(command []string, protocol string) (string, error) {
	return fingerprintWith(command, protocol, FingerprintInputs{})
}

func fingerprintWith(command []string, protocol string, in FingerprintInputs) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("missing harness command")
	}
	files := []string{}
	environment := map[string]string{}
	model, project := "", ""
	addExecutable := func(name string) error {
		file, err := exec.LookPath(name)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	}
	contract := command
	native := len(command) == 4 && command[1] == "__adapter"
	if native {
		contract = command[1:]
	} else if err := addExecutable(command[0]); err != nil {
		return "", err
	}
	if native {
		if !filepath.IsAbs(command[3]) {
			return "", fmt.Errorf("native harness provider %q must be an absolute path", command[3])
		}
		if err := addExecutable(command[3]); err != nil {
			return "", err
		}
		switch command[2] {
		case "codex":
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			root := os.Getenv("CODEX_HOME")
			if root == "" {
				root = filepath.Join(home, ".codex")
			}
			files = append(files, filepath.Join(root, "config.toml"))
		case "claude-code":
			if in.ProjectRoot != "" {
				var err error
				if project, err = committedProjectSettings(in.ProjectRoot, in.BaseCommit); err != nil {
					return "", err
				}
			}
			model = in.Model
			for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_BASE_URL"} {
				environment[key] = os.Getenv(key)
			}
		}
	}
	digest, err := fingerprintFiles(files)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(struct {
		Files       string
		Command     []string
		Protocol    string
		Environment map[string]string
		Model       string `json:",omitempty"`
		Project     string `json:",omitempty"`
	}{digest, contract, protocol, environment, model, project})
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
func fingerprintFiles(files []string) (string, error) {
	h := sha256.New()
	for _, file := range files {
		fmt.Fprintf(h, "%s\x00", file)
		f, err := os.Open(file)
		if os.IsNotExist(err) {
			fmt.Fprint(h, "absent\x00")
			continue
		}
		if err != nil {
			return "", fmt.Errorf("cannot fingerprint harness configuration: %w", err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1000000000 {
			f.Close()
			return "", fmt.Errorf("unbounded or non-regular harness input")
		}
		n, err := io.Copy(h, io.LimitReader(f, 1000000001))
		f.Close()
		if err != nil || n > 1000000000 {
			return "", fmt.Errorf("cannot fingerprint complete harness input")
		}
		fmt.Fprint(h, "\x00")
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
