package molecule

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// The sensitive-path list auto-when-ready never approves a scope against: CI,
// tooling, dependency and agent control files, where an edit changes what later
// runs or agents execute. It is the only copy, versioned and pinned by a test, and
// the scope gate shows it for human review, so any change is a reviewed edit of
// the list, its version and that test. Matching is case-insensitive. A directory
// entry matches as a whole path segment at any depth; a base name, base-name
// prefix or extension matches the last path element at any depth.
const sensitivePathListVersion = "v2"

var (
	sensitivePathPrefixes = []string{
		// v1 directories.
		".github/", ".claude/", ".forgecell/", ".githooks/", ".husky/", "scripts/",
		// Agent configuration.
		".cursor/", ".continue/", ".windsurf/", ".aider/",
		// Other CI systems and build bots.
		".circleci/", ".buildkite/", ".gitlab/", ".gitea/", ".forgejo/", ".woodpecker/", ".teamcity/", ".azuredevops/", ".azure-pipelines/",
		// Container and editor task definitions.
		".devcontainer/", ".vscode/",
		// Package-manager configuration and vendored code.
		".cargo/", ".bundle/", ".mvn/", ".gradle/", ".yarn/", "vendor/",
	}
	sensitiveBaseNames = []string{
		// v1 files.
		".gitlab-ci.yml", "go.mod", "go.sum", "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "bun.lockb", "Makefile",
		// Git control files.
		".gitmodules", ".gitattributes", "CODEOWNERS",
		// Agent configuration.
		".mcp.json", "CLAUDE.md", "AGENTS.md", "GEMINI.md", ".cursorrules", ".windsurfrules", ".aider.conf.yml",
		// Package-manager configuration.
		".npmrc", ".yarnrc", ".yarnrc.yml", "pnpm-workspace.yaml", ".pypirc", "pip.conf", ".condarc", "NuGet.Config", ".tool-versions", "bunfig.toml", "gradle.properties", "gradle-wrapper.properties",
		// Container files.
		"Dockerfile", "Containerfile", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", ".dockerignore", "devcontainer.json", "skaffold.yaml", "Tiltfile",
		// Other CI systems.
		".travis.yml", "Jenkinsfile", "azure-pipelines.yml", "azure-pipelines.yaml", "bitbucket-pipelines.yml", ".drone.yml", ".woodpecker.yml", ".woodpecker.yaml", "appveyor.yml", ".appveyor.yml", "cloudbuild.yaml", "cloudbuild.yml", "buildkite.yml", "codecov.yml", ".codecov.yml", "renovate.json", ".renovaterc", ".renovaterc.json", "dependabot.yml",
		// Hook and linter configuration.
		".pre-commit-config.yaml", ".pre-commit-hooks.yaml", ".lintstagedrc", "lint-staged.config.js", "commitlint.config.js", ".huskyrc", ".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json",
		".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml", ".prettierrc", ".prettierrc.json", ".prettierrc.yml", ".prettierrc.yaml", ".prettierrc.js", "prettier.config.js", ".stylelintrc", ".shellcheckrc", ".editorconfig", ".rubocop.yml", ".flake8", "ruff.toml", ".ruff.toml", "mypy.ini", "tox.ini", "lefthook.yml", "lefthook.yaml", ".lefthook.yml",
		// Other ecosystems' manifests and build entry points.
		"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "noxfile.py", "Cargo.toml", "build.rs", "Gemfile", "Rakefile", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradlew", "gradlew.bat", "mvnw", "mvnw.cmd",
		"composer.json", "CMakeLists.txt", "meson.build", "justfile", "Taskfile.yml", "Taskfile.yaml", "deno.json", "deno.jsonc", "Package.swift", "Podfile", "mix.exs", "rebar.config", "stack.yaml", "package.yaml", "pubspec.yaml", "shard.yml", "Directory.Build.props", "Directory.Build.targets", "global.json", "build.sbt", "Dockerrun.aws.json",
		// Build, environment and deployment entry points.
		"GNUmakefile", ".envrc", "Procfile", "render.yaml", "Vagrantfile", "Brewfile", "BUILD.bazel", "BUILD", "WORKSPACE", "MODULE.bazel", ".bazelrc", ".babelrc", "conftest.py", ".rspec",
	}
	// sensitiveBasePrefixes match the start of the base name: go.work and go.work.sum,
	// Dockerfile variants, compose overrides and per-tool configuration families.
	sensitiveBasePrefixes = []string{"go.work", "dockerfile.", "containerfile.", "docker-compose.", "eslint.config.", ".golangci.", "lefthook", "jest.config.", "vitest.config.", "webpack.config.", "babel.config.", "next.config.", "vite.config.", "gulpfile."}
	sensitiveExtensions   = []string{".lock", ".csproj", ".fsproj", ".vbproj", ".sln", ".gemspec", ".gradle", ".dockerfile", ".mk", ".tf"}
)

// sensitiveScopePath reports whether a validated, clean repository path is on the
// sensitive-path list.
func sensitiveScopePath(p string) bool {
	lower := strings.ToLower(p)
	for _, prefix := range sensitivePathPrefixes {
		if strings.HasPrefix(lower, prefix) || strings.Contains(lower, "/"+prefix) {
			return true
		}
	}
	base := path.Base(lower)
	for _, name := range sensitiveBaseNames {
		if base == strings.ToLower(name) {
			return true
		}
	}
	for _, prefix := range sensitiveBasePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return slices.Contains(sensitiveExtensions, path.Ext(base))
}

// sensitivePathListSummary renders the whole list for the scope gate.
func sensitivePathListSummary() string {
	return fmt.Sprintf("directories (any depth): %s; file names (any depth): %s; file name prefixes: %s; extensions: %s",
		strings.Join(sensitivePathPrefixes, " "), strings.Join(sensitiveBaseNames, " "), strings.Join(sensitiveBasePrefixes, " "), strings.Join(sensitiveExtensions, " "))
}

// scopeGateSensitivePathsNote is the note a human reads at the scope gate when the
// Formula lets a policy approve it: the exact list the policy declines against, and
// the check-argument rule it applies on top.
func scopeGateSensitivePathsNote(f formula.Formula) (string, bool) {
	roles, err := f.ExecutionRoles()
	if err != nil {
		return "", false
	}
	gate, ok := roles[formula.ScopeApproval]
	if !ok {
		return "", false
	}
	if id, declared := gate.DeclaresPolicy(); !declared || id != formula.PolicyAutoWhenReady {
		return "", false
	}
	return fmt.Sprintf("Scope policy auto-when-ready never approves a scope that touches sensitive-path list %s, nor a scope file that a check names or runs, or that lies under a directory, package, --flag=path value, absolute checkout path or sh -c script path it names; a shell script that uses expansions, backticks, eval, xargs, -exec, source or glob patterns, and an interpreter given inline code (python -c, node -e, perl -e, awk programs), are treated as naming the whole repository. Only .go files named through go test, go vet or go build package arguments, or through the paths of a read-only gofmt -l, are exempt; a human decides the rest. List %s: %s.", sensitivePathListVersion, sensitivePathListVersion, sensitivePathListSummary()), true
}

// newFilesNote tells a human reading the scope gate which candidate checks are
// defined only by a file the plan itself creates, so they have no frozen anchor.
func newFilesNote(p readiness.Plan) (string, bool) {
	checks := readiness.NewFileOnlyChecks(p.Analysis.Checks, p.NewFiles)
	if len(checks) == 0 {
		return "", false
	}
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = fmt.Sprintf("%s (defined by %s)", c.ID, strings.Join(c.Definitions, ", "))
	}
	return fmt.Sprintf("%d candidate check(s) are defined only by a new file this plan adds: %s. A new file is absent from the frozen repository tree, so such a check has no frozen anchor: it is candidate evidence the coding run writes itself, and never independent acceptance.", len(checks), strings.Join(parts, "; ")), true
}

// namedRoot is a file or directory a check names: the token as written, the
// lower-case repository path it resolves to, and whether it is a go test, go vet or
// go build package argument or a read-only gofmt -l path, the only way a .go file
// may be named. The repository root names everything.
type namedRoot struct {
	token string
	dir   string
	pkg   bool
}

func (n namedRoot) covers(file string) bool {
	return n.dir == "." || file == n.dir || strings.HasPrefix(file, n.dir+"/")
}

var (
	shellPrograms = []string{"sh", "bash", "dash", "zsh", "ash", "ksh"}
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// goPathFlags take a path, program or directory: a value is a named root, never a package argument.
	goPathFlags = map[string]bool{"exec": true, "toolexec": true, "vettool": true, "o": true, "C": true, "coverprofile": true, "cpuprofile": true, "memprofile": true, "blockprofile": true, "mutexprofile": true, "trace": true, "outputdir": true, "modfile": true, "overlay": true, "pkgdir": true}
	// goValueFlags take a value that is not a path, so it is neither a root nor a package argument.
	goValueFlags = map[string]bool{"run": true, "bench": true, "skip": true, "fuzz": true, "count": true, "p": true, "parallel": true, "timeout": true, "cpu": true, "covermode": true, "coverpkg": true, "tags": true, "shuffle": true, "fuzztime": true, "fuzzminimizetime": true, "benchtime": true, "vet": true, "mod": true, "buildvcs": true, "ldflags": true, "gcflags": true, "asmflags": true, "gccgoflags": true, "compiler": true, "installsuffix": true}
)

// rootCollector gathers the roots one check names. Every token is read relative to
// each directory the check may run from: the repository root, the check directory
// and any directory a cd or go -C enters, so the guess is never narrower than any of them.
type rootCollector struct {
	checkouts []string
	bases     []string
	roots     []namedRoot
}

// checkNamedRoots collects every file and directory a check names or runs through
// its arguments, --flag=value values, absolute paths inside a checkout and the
// commands and words of an sh -c script.
func checkNamedRoots(c readiness.Check, checkouts []string) []namedRoot {
	rc := &rootCollector{checkouts: checkouts, bases: []string{"."}}
	if dir := path.Clean(c.Dir); dir != "." && dir != ".." && !strings.HasPrefix(dir, "../") && !path.IsAbs(dir) {
		rc.bases = append(rc.bases, dir)
	}
	rc.command(c.Argv)
	return rc.roots
}

func (rc *rootCollector) whole(reason string) {
	rc.roots = append(rc.roots, namedRoot{token: "the whole repository (" + reason + ")", dir: "."})
}

func (rc *rootCollector) add(token string, pkg bool) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsRune(token, 0) {
		return
	}
	token = cutGlob(token)
	if path.IsAbs(token) {
		for _, root := range rc.checkouts {
			if root == "" {
				continue
			}
			if r, err := filepath.Rel(root, token); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
				rc.roots = append(rc.roots, namedRoot{token: token, dir: strings.ToLower(trimPattern(filepath.ToSlash(r))), pkg: pkg})
				return
			}
		}
		return
	}
	for _, base := range rc.bases {
		dir := path.Join(base, trimPattern(token))
		if dir == ".." || strings.HasPrefix(dir, "../") {
			continue
		}
		rc.roots = append(rc.roots, namedRoot{token: token, dir: strings.ToLower(dir), pkg: pkg})
	}
}

// addValue names a flag or assignment value, and each part of a colon or comma
// separated list.
func (rc *rootCollector) addValue(value string) {
	rc.add(value, false)
	if strings.ContainsAny(value, ":,") {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ':' || r == ',' }) {
			rc.add(part, false)
		}
	}
}

// flag names the value of --flag=value, -f=value and an attached short value such
// as -Ctools or -I../x, whatever its extension.
func (rc *rootCollector) flag(token string) {
	if _, value, ok := strings.Cut(token, "="); ok {
		rc.addValue(value)
		return
	}
	if !strings.HasPrefix(token, "--") && len(token) > 2 {
		rc.addValue(token[2:])
	}
}

// options names the path values of a list of options such as an environment
// variable's value, where a bare word is read as a value too.
func (rc *rootCollector) options(parts []string) {
	for _, part := range parts {
		if strings.HasPrefix(part, "-") && part != "-" {
			rc.flag(part)
		} else {
			rc.add(part, false)
		}
	}
}

func (rc *rootCollector) words(tokens []string, pkg bool) {
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") && token != "-" {
			rc.flag(token)
			continue
		}
		rc.add(token, pkg)
	}
}

// enterDirs adds the directories a cd or go -C may leave later commands in.
func (rc *rootCollector) enterDirs(dirs ...string) {
	next := slices.Clone(rc.bases)
	for _, dir := range dirs {
		if path.IsAbs(dir) || dir == "" {
			continue
		}
		for _, base := range rc.bases {
			if joined := path.Join(base, dir); joined != ".." && !strings.HasPrefix(joined, "../") && !slices.Contains(next, joined) {
				next = append(next, joined)
			}
		}
	}
	rc.bases = next
}

// stripEnv names the values of leading VAR=value assignments and of an env wrapper
// and returns the command that follows them.
func (rc *rootCollector) stripEnv(argv []string) []string {
	for len(argv) > 0 {
		switch {
		case envAssignment.MatchString(argv[0]):
			_, value, _ := strings.Cut(argv[0], "=")
			argv = argv[1:]
			if strings.HasPrefix(value, "-") {
				rc.options(strings.Fields(value))
			} else {
				rc.addValue(value)
			}
			// A quoted value that holds spaces reaches a script as its first word and
			// the options after it, such as GOFLAGS='-exec=x -vettool=y'.
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") && argv[0] != "-" {
				rc.flag(argv[0])
				argv = argv[1:]
			}
		case path.Base(argv[0]) == "env":
			argv = argv[1:]
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				flag := argv[0]
				argv = argv[1:]
				if (flag == "-u" || flag == "-C" || flag == "-S") && len(argv) > 0 {
					rc.add(argv[0], false)
					argv = argv[1:]
				} else {
					rc.flag(flag)
				}
			}
		default:
			return argv
		}
	}
	return argv
}

func (rc *rootCollector) command(argv []string) {
	argv = rc.stripEnv(argv)
	if len(argv) == 0 {
		return
	}
	head, tokens := argv[0], argv[1:]
	if strings.Contains(head, "/") {
		rc.add(head, false)
	}
	program := path.Base(head)
	if runsInlineCode(program, tokens) {
		rc.whole("an interpreter run on inline code")
		return
	}
	switch {
	case program == "cd" && len(tokens) > 0:
		rc.add(tokens[0], false)
		rc.enterDirs(tokens[0])
	case slices.Contains(shellPrograms, program):
		if !rc.shell(tokens) {
			rc.words(tokens, false)
		}
	case program == "go":
		rc.goCommand(tokens)
	case program == "gofmt" && readOnlyGofmt(tokens):
		rc.gofmtWords(tokens)
	default:
		rc.words(tokens, false)
	}
}

// shell reads an sh -c script and the arguments after it, and reports whether the
// command was one.
func (rc *rootCollector) shell(tokens []string) bool {
	for i := 0; i+1 < len(tokens); i++ {
		if strings.HasPrefix(tokens[i], "-") && !strings.HasPrefix(tokens[i], "--") && strings.Contains(tokens[i], "c") {
			rc.script(tokens[i+1])
			rc.words(tokens[i+2:], false)
			return true
		}
	}
	return false
}

func (rc *rootCollector) script(script string) {
	if reason := opaqueScript(script); reason != "" {
		rc.whole("an sh -c script that " + reason)
		return
	}
	for _, command := range splitScript(script) {
		rc.command(command)
	}
}

// goCommand reads go test, go vet and go build, whose package arguments may name
// .go files, apart from the values of path flags such as -exec, -toolexec,
// -vettool, -o and -C. Any other go command names everything it is given.
func (rc *rootCollector) goCommand(tokens []string) {
	i := 0
	var entered []string
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		name, value, hasValue := strings.Cut(strings.TrimLeft(tokens[i], "-"), "=")
		if name == "C" {
			if !hasValue && i+1 < len(tokens) {
				i++
				value = tokens[i]
			}
			rc.add(value, false)
			entered = append(entered, value)
		}
		i++
	}
	if i >= len(tokens) || (tokens[i] != "test" && tokens[i] != "vet" && tokens[i] != "build") {
		rc.words(tokens, false)
		return
	}
	rc.enterDirs(entered...)
	afterArgs := false
	for j := i + 1; j < len(tokens); j++ {
		token := tokens[j]
		switch {
		case afterArgs:
			rc.words([]string{token}, false)
		case token == "-args" || token == "--args":
			afterArgs = true
		case strings.HasPrefix(token, "-") && token != "-":
			name, value, hasValue := strings.Cut(strings.TrimLeft(token, "-"), "=")
			name = strings.TrimPrefix(name, "test.")
			switch {
			case goPathFlags[name]:
				if !hasValue && j+1 < len(tokens) {
					j++
					value = tokens[j]
				}
				rc.addValue(value)
			case goValueFlags[name]:
				if !hasValue {
					j++
				}
			case hasValue:
				rc.flag(token)
			}
		default:
			rc.add(token, true)
		}
	}
}

// readOnlyGofmt reports whether gofmt only lists the files it would change: -l is
// present and neither -w nor -d is, with any value. Its path arguments then only
// read .go files, which is how a plan's own formatter check names its scope.
func readOnlyGofmt(tokens []string) bool {
	list := false
	for _, token := range tokens {
		if !strings.HasPrefix(token, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(token, "-"), "=")
		switch name {
		case "l":
			list = true
		case "w", "d":
			return false
		}
	}
	return list
}

// gofmtFlagsWithValue are the gofmt flags that take an argument.
var gofmtFlagsWithValue = map[string]bool{"cpuprofile": true, "r": true}

// gofmtWords names the arguments of a read-only gofmt -l. Only bare path words are
// read-only package words; the value of a flag that takes an argument, such as
// -cpuprofile, is a file the command writes or reads and is a named root.
func (rc *rootCollector) gofmtWords(tokens []string) {
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if !strings.HasPrefix(token, "-") || token == "-" {
			rc.add(token, true)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(token, "-"), "=")
		if !gofmtFlagsWithValue[name] {
			continue
		}
		if !hasValue && i+1 < len(tokens) {
			i++
			value = tokens[i]
		}
		rc.addValue(value)
	}
}

// inlineCodeFlags lists, per interpreter, the short options that hand it code (or a
// module to import from the repository) on the command line.
var inlineCodeFlags = map[string]string{
	"python": "cm", "node": "ep", "perl": "eE", "ruby": "e", "php": "r", "bun": "ep", "deno": "",
	"awk": "e", "gawk": "e", "mawk": "e", "nawk": "e",
}

// runsInlineCode reports whether a command is an interpreter given code on its
// command line: python -c or -m, node -e, perl -e, ruby -e, php -r, bun -e, deno
// eval, or an awk program. Such code can import or open any file, so the check names
// the whole repository. Any option scanned is over-read on purpose: a false match
// only sends the scope to a human.
func runsInlineCode(program string, tokens []string) bool {
	program = strings.TrimRight(program, "0123456789.")
	letters, known := inlineCodeFlags[program]
	if !known {
		return false
	}
	if strings.HasSuffix(program, "awk") {
		hasProgramFile := false
		for _, token := range tokens {
			hasProgramFile = hasProgramFile || strings.HasPrefix(token, "-f") || strings.HasPrefix(token, "--file")
		}
		if !hasProgramFile {
			return true
		}
	}
	for _, token := range tokens {
		switch {
		case token == "--":
			return false
		case program == "deno" && token == "eval":
			return true
		case strings.HasPrefix(token, "--"):
			name, _, _ := strings.Cut(token, "=")
			if name == "--eval" || name == "--print" || name == "--source" {
				return true
			}
		case strings.HasPrefix(token, "-") && len(token) > 1 && strings.ContainsAny(token[1:], letters):
			return true
		}
	}
	return false
}

// trimPattern turns a Go package pattern such as ./internal/... into its directory.
func trimPattern(token string) string {
	token = strings.TrimSuffix(strings.TrimSuffix(token, "..."), "/")
	if token == "" {
		return "."
	}
	return token
}

// cutGlob trims a token at its first glob metacharacter, back to the directory that
// holds it, so a pattern names the directory it expands in. A leading pattern names
// the working directory.
func cutGlob(token string) string {
	i := strings.IndexAny(token, "*?[")
	if i < 0 {
		return token
	}
	return token[:strings.LastIndex(token[:i], "/")+1]
}

// opaqueScript reports why a shell script cannot be read statically: an expansion,
// backticks, eval, xargs, find -exec, sourcing or a glob pattern can name any path.
// A command substitution $(...) is read as the commands it runs.
func opaqueScript(script string) string {
	switch {
	case strings.ContainsAny(script, "*?["):
		return "uses a glob pattern"
	case strings.Contains(script, "`"):
		return "uses backticks"
	}
	for i := 0; i < len(script); i++ {
		if script[i] != '$' {
			continue
		}
		if i+1 >= len(script) || script[i+1] != '(' || (i+2 < len(script) && script[i+2] == '(') {
			return "uses a variable or arithmetic expansion"
		}
	}
	for _, command := range splitScript(script) {
		if command[0] == "." {
			return "sources a file"
		}
		for _, word := range command {
			switch word {
			case "eval", "xargs", "source", "-exec", "-execdir":
				return "runs " + word
			}
		}
	}
	return ""
}

// splitScript splits a shell script into simple commands and their words. It is a
// bounded reading for naming paths, not a shell parser: quotes and expansions are
// dropped, and command separators, pipes, subshells and backticks start a command.
func splitScript(script string) [][]string {
	flattened := strings.Map(func(r rune) rune {
		switch r {
		case ';', '|', '&', '(', ')', '`':
			return '\n'
		case '"', '\'', '$', '{', '}', '<', '>':
			return ' '
		}
		return r
	}, script)
	var out [][]string
	for _, line := range strings.Split(flattened, "\n") {
		if words := strings.Fields(line); len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// checkNamedScopePath returns a description of the first scope file that a check
// names or runs, or that lies under a directory it names, or "" when none does. A
// .go file is exempt only when every root covering it is a go test, go vet or go
// build package argument or a read-only gofmt -l path.
func checkNamedScopePath(plan readiness.Plan, checkouts []string) string {
	for _, c := range plan.Analysis.Checks {
		roots := checkNamedRoots(c, checkouts)
		for _, s := range plan.Analysis.Scope {
			file := strings.ToLower(s.Path)
			for _, root := range roots {
				if !root.covers(file) {
					continue
				}
				if root.pkg && strings.HasSuffix(file, ".go") {
					continue
				}
				return fmt.Sprintf("scope path %q is named by, or lies under, %q, which check %s names", s.Path, root.token, c.ID)
			}
		}
	}
	return ""
}
