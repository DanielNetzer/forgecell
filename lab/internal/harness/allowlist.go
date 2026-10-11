package harness

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
	"path"
	"regexp"
	"slices"
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
// plan's checks. Unsafe or publishing commands are omitted, never escaped. When a
// required check runs gofmt -l (or go fmt), the approved scope adds exact
// formatter entries for its .go files; without such a check the result is
// independent of scope.
func DeriveCodingAllowlist(formulaYAML string, checks []readiness.Check, scope ...readiness.ScopedPath) ([]string, error) {
	return deriveAllowlist(formulaYAML, checks, scope, true)
}

// DerivePreFormatterAllowlist is the derivation that ran before formatter
// entries existed. It exists only so plans approved earlier keep verifying.
func DerivePreFormatterAllowlist(formulaYAML string, checks []readiness.Check) ([]string, error) {
	return deriveAllowlist(formulaYAML, checks, nil, false)
}

// MatchCodingAllowlist reports whether stored is an allowance the plan may run
// under: the current derivation, or the pre-formatter one, which is a subset of
// it. Any other stored list is drift and does not match. A returned list equals stored.
func MatchCodingAllowlist(formulaYAML string, checks []readiness.Check, scope []readiness.ScopedPath, stored []string) ([]string, bool, error) {
	legacy, err := DerivePreFormatterAllowlist(formulaYAML, checks)
	if err != nil {
		return nil, false, err
	}
	derived, derr := DeriveCodingAllowlist(formulaYAML, checks, scope...)
	if derr == nil && slices.Equal(derived, stored) {
		return derived, true, nil
	}
	if slices.Equal(legacy, stored) {
		return legacy, true, nil
	}
	return nil, false, derr
}

func deriveAllowlist(formulaYAML string, checks []readiness.Check, scope []readiness.ScopedPath, formatters bool) ([]string, error) {
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
	if formatters {
		entries = append(entries, formatterEntries(checks, scope)...)
	}
	return EffectiveCodingAllowlist(entries)
}

var gofmtPathToken = regexp.MustCompile(`^(?:\.|(?:\./)?[A-Za-z0-9_][A-Za-z0-9_./-]*)$`)

const (
	gofmtShellPrefix = `test -z "$(gofmt -l `
	gofmtShellSuffix = `)"`
)

// gofmtCheckPaths recognises exactly `gofmt -l PATHS` and the shell form
// `sh -c 'test -z "$(gofmt -l PATHS)"'`. Anything else fails closed.
func gofmtCheckPaths(argv []string) ([]string, bool) {
	var args []string
	switch {
	case len(argv) >= 3 && argv[0] == "gofmt" && argv[1] == "-l":
		args = argv[2:]
	case len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c":
		body, ok := strings.CutPrefix(argv[2], gofmtShellPrefix)
		if !ok {
			return nil, false
		}
		if body, ok = strings.CutSuffix(body, gofmtShellSuffix); !ok {
			return nil, false
		}
		args = strings.Split(body, " ")
	default:
		return nil, false
	}
	for _, arg := range args {
		if !gofmtPathToken.MatchString(arg) || slices.Contains(strings.Split(arg, "/"), "..") {
			return nil, false
		}
	}
	return args, true
}

// gofmtListEntry joins each path argument to the check directory, so the exact
// entry runs from the workspace root.
func gofmtListEntry(dir string, args []string) (string, bool) {
	if dir != "" && dir != "." && (readiness.ValidatePath(dir) != nil || !safeArgument.MatchString(dir)) {
		return "", false
	}
	argv := []string{"gofmt", "-l"}
	for _, arg := range args {
		joined := path.Join(dir, arg)
		if readiness.ValidatePath(joined) != nil {
			return "", false
		}
		argv = append(argv, joined)
	}
	return renderEntry("", argv)
}

// formatterEntries returns exact formatter entries for required gofmt or go fmt
// checks: one per approved scope path that ends in .go. No pattern is granted.
func formatterEntries(checks []readiness.Check, scope []readiness.ScopedPath) []string {
	var files []string
	for _, s := range scope {
		if strings.HasSuffix(s.Path, ".go") && readiness.ValidatePath(s.Path) == nil {
			files = append(files, s.Path)
		}
	}
	sort.Strings(files)
	var entries []string
	for _, c := range checks {
		if !c.Required {
			continue
		}
		if args, ok := gofmtCheckPaths(c.Argv); ok {
			list, ok := gofmtListEntry(c.Dir, args)
			if !ok {
				continue
			}
			entries = append(entries, list)
			for _, file := range files {
				if entry, ok := renderEntry("", []string{"gofmt", "-w", file}); ok {
					entries = append(entries, entry)
				}
			}
		} else if len(c.Argv) >= 2 && c.Argv[0] == "go" && c.Argv[1] == "fmt" {
			if _, ok := renderEntry(c.Dir, c.Argv); !ok {
				continue
			}
			for _, file := range files {
				rel := file
				if c.Dir != "" && c.Dir != "." {
					var under bool
					if rel, under = strings.CutPrefix(file, c.Dir+"/"); !under {
						continue
					}
				}
				if entry, ok := renderEntry(c.Dir, []string{"go", "fmt", rel}); ok {
					entries = append(entries, entry)
				}
			}
		}
	}
	return entries
}

// EffectiveCodingAllowlist validates entries and returns the canonical list the
// provider receives: base entries, then Node preset entries, then the rest sorted.
// The result is idempotent. Nil input yields the base list.
// LegacyCodingAllowlist is the allowance plans approved before derivation ran
// under: the previous fixed Node entries, plus the read-only base.
func LegacyCodingAllowlist() ([]string, error) {
	return EffectiveCodingAllowlist(nodeAllowlist)
}

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
