package harness

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	maxAllowlistEntries    = 512
	maxAllowlistEntryBytes = 512
	maxAllowlistComponents = 100
)

var baseAllowlist = []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)"}

// The Node preset is the pre-derivation Claude Code allowance. It is retained
// byte-for-byte, only for repositories that declare a package.json component.
var nodeAllowlist = []string{"Bash(npm test)", "Bash(npm run test *)", "Bash(npm run typecheck)", "Bash(npm run build)", "Bash(node --test *)"}

// Directory flags let a command for a nested component run from the checkout
// root, because the permission pattern matches the exact command text.
var directoryFlags = map[string]string{"go": "-C", "npm": "--prefix", "pnpm": "--dir", "yarn": "--cwd"}

var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
var safeArgument = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+-]+$`)
var wordSplit = regexp.MustCompile(`[^a-z0-9]+`)

// Shells, privilege and wrapper commands, and networked or publishing tools are
// never granted, even when a reviewed check names them. Git is limited to the
// two read-only base entries.
var deniedCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true, "tcsh": true, "fish": true,
	"env": true, "xargs": true, "sudo": true, "su": true, "doas": true, "eval": true, "exec": true, "command": true, "nohup": true,
	"git": true, "gh": true, "curl": true, "wget": true, "ssh": true, "scp": true, "sftp": true, "rsync": true, "nc": true, "ncat": true, "telnet": true, "ftp": true,
	"npx": true, "pnpx": true, "bunx": true, "uvx": true, "pipx": true, "python": true, "python3": true, "perl": true, "ruby": true, "awk": true,
	"find": true, "timeout": true, "nice": true, "busybox": true, "docker": true, "socat": true, "openssl": true,
}

// deniedWords is a best-effort lint over argument words, not a security
// boundary: every derived entry still runs repository code. It removes common
// publishing, installation and execution-delegation forms from derived checks.
var deniedWords = map[string]bool{
	"publish": true, "unpublish": true, "push": true, "deploy": true, "release": true, "login": true, "logout": true, "adduser": true, "upload": true,
	"install": true, "get": true, "add": true, "link": true, "version": true, "exec": true, "toolexec": true,
}

type formulaComponent struct {
	Path            string     `yaml:"path"`
	Manifest        string     `yaml:"manifest"`
	SuggestedChecks [][]string `yaml:"suggestedChecks"`
}

func formulaComponents(text string) ([]formulaComponent, error) {
	var doc struct {
		RepositoryContext struct {
			Components []formulaComponent `yaml:"components"`
		} `yaml:"repositoryContext"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("Formula repositoryContext is unreadable: %w", err)
	}
	if len(doc.RepositoryContext.Components) > maxAllowlistComponents {
		return nil, fmt.Errorf("Formula repositoryContext has more than %d components", maxAllowlistComponents)
	}
	return doc.RepositoryContext.Components, nil
}

// DeriveCodingAllowlist returns the exact Claude Code tool allowance for a coding
// attempt: fixed base entries, the Node preset for package.json components, and
// exact argv checks taken from the approved Formula's repositoryContext and the
// plan's checks. Unsafe or publishing commands are omitted, never escaped.
func DeriveCodingAllowlist(formulaYAML string, checks []readiness.Check) ([]string, error) {
	components, err := formulaComponents(formulaYAML)
	if err != nil {
		return nil, err
	}
	entries := append([]string{}, baseAllowlist...)
	node := false
	for _, c := range components {
		if path.Base(c.Manifest) == "package.json" {
			node = true
		}
		for _, argv := range c.SuggestedChecks {
			if entry, ok := renderEntry(c.Path, argv); ok {
				entries = append(entries, entry)
			}
		}
	}
	if node {
		entries = append(entries, nodeAllowlist...)
	}
	for _, c := range checks {
		if entry, ok := renderEntry(c.Dir, c.Argv); ok {
			entries = append(entries, entry)
		}
	}
	return EffectiveCodingAllowlist(entries)
}

// EffectiveCodingAllowlist validates entries and returns the canonical list the
// provider receives: base entries, then Node preset entries, then the rest sorted.
// The result is idempotent. Nil input yields the base list.
func EffectiveCodingAllowlist(entries []string) ([]string, error) {
	seen := map[string]bool{}
	for _, entry := range entries {
		if !validAllowlistEntry(entry) {
			return nil, fmt.Errorf("unsupported coding allowlist entry %q", entry)
		}
		seen[entry] = true
	}
	out := []string{}
	for _, entry := range baseAllowlist {
		out = append(out, entry)
		delete(seen, entry)
	}
	for _, entry := range nodeAllowlist {
		if seen[entry] {
			out = append(out, entry)
			delete(seen, entry)
		}
	}
	rest := make([]string, 0, len(seen))
	for entry := range seen {
		rest = append(rest, entry)
	}
	sort.Strings(rest)
	out = append(out, rest...)
	if len(out) > maxAllowlistEntries {
		return nil, fmt.Errorf("coding allowlist exceeds %d entries", maxAllowlistEntries)
	}
	return out, nil
}

func renderEntry(dir string, argv []string) (string, bool) {
	scoped, ok := scopedArgv(dir, argv)
	if !ok || !safeArgv(scoped) {
		return "", false
	}
	entry := "Bash(" + strings.Join(scoped, " ") + ")"
	return entry, len(entry) <= maxAllowlistEntryBytes
}

func scopedArgv(dir string, argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	if dir == "" || dir == "." {
		return argv, true
	}
	flag, ok := directoryFlags[argv[0]]
	if !ok || readiness.ValidatePath(dir) != nil || !safeArgument.MatchString(dir) || (len(argv) > 1 && argv[1] == flag) {
		return nil, false
	}
	return append([]string{argv[0], flag, dir}, argv[1:]...), true
}

func safeArgv(argv []string) bool {
	if len(argv) == 0 || len(argv) > 64 || !commandName.MatchString(argv[0]) || deniedCommands[strings.ToLower(argv[0])] {
		return false
	}
	for _, arg := range argv[1:] {
		if !safeArgument.MatchString(arg) {
			return false
		}
		// Flag values (-o=/x, --out=../y, -C:..) are paths too, so check every
		// "=" or ":" separated part, not only the start of the argument.
		for _, part := range strings.FieldsFunc(arg, func(r rune) bool { return r == '=' || r == ':' }) {
			if strings.HasPrefix(part, "/") {
				return false
			}
			for _, segment := range strings.Split(part, "/") {
				if segment == ".." {
					return false
				}
			}
		}
		for _, word := range wordSplit.Split(strings.ToLower(arg), -1) {
			if deniedWords[word] {
				return false
			}
		}
	}
	return true
}

func validAllowlistEntry(entry string) bool {
	for _, known := range baseAllowlist {
		if entry == known {
			return true
		}
	}
	for _, known := range nodeAllowlist {
		if entry == known {
			return true
		}
	}
	body, ok := strings.CutPrefix(entry, "Bash(")
	if !ok || len(entry) > maxAllowlistEntryBytes {
		return false
	}
	body, ok = strings.CutSuffix(body, ")")
	return ok && safeArgv(strings.Split(body, " "))
}

func requestAllowlist(request map[string]any) ([]string, error) {
	var entries []string
	switch value := request["codingAllowlist"].(type) {
	case nil:
	case []string:
		entries = value
	case []any:
		for _, item := range value {
			entry, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("invalid coding allowlist entry")
			}
			entries = append(entries, entry)
		}
	default:
		return nil, fmt.Errorf("invalid coding allowlist")
	}
	return EffectiveCodingAllowlist(entries)
}
