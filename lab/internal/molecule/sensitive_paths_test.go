package molecule

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// The golden copy of the v2 sensitive-path list. A change to the list is a reviewed
// edit of this copy, of sensitivePathListVersion and of sensitive_paths.go together.
func TestHardening129SensitivePathListV2IsPinned(t *testing.T) {
	if sensitivePathListVersion != "v2" {
		t.Fatalf("sensitive-path list version %q: a list change needs a reviewed version bump and test edit", sensitivePathListVersion)
	}
	wantPrefixes := []string{
		".github/", ".claude/", ".forgecell/", ".githooks/", ".husky/", "scripts/",
		".cursor/", ".continue/", ".windsurf/", ".aider/",
		".circleci/", ".buildkite/", ".gitlab/", ".gitea/", ".forgejo/", ".woodpecker/", ".teamcity/", ".azuredevops/", ".azure-pipelines/",
		".devcontainer/", ".vscode/",
		".cargo/", ".bundle/", ".mvn/", ".gradle/", ".yarn/", "vendor/",
	}
	if !reflect.DeepEqual(sensitivePathPrefixes, wantPrefixes) {
		t.Fatalf("sensitive directories changed: %q", sensitivePathPrefixes)
	}
	wantBaseNames := []string{
		".gitlab-ci.yml", "go.mod", "go.sum", "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "bun.lockb", "Makefile",
		".gitmodules", ".gitattributes", "CODEOWNERS",
		".mcp.json", "CLAUDE.md", "AGENTS.md", "GEMINI.md", ".cursorrules", ".windsurfrules", ".aider.conf.yml",
		".npmrc", ".yarnrc", ".yarnrc.yml", "pnpm-workspace.yaml", ".pypirc", "pip.conf", ".condarc", "NuGet.Config", ".tool-versions", "bunfig.toml", "gradle.properties", "gradle-wrapper.properties",
		"Dockerfile", "Containerfile", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", ".dockerignore", "devcontainer.json", "skaffold.yaml", "Tiltfile",
		".travis.yml", "Jenkinsfile", "azure-pipelines.yml", "azure-pipelines.yaml", "bitbucket-pipelines.yml", ".drone.yml", ".woodpecker.yml", ".woodpecker.yaml", "appveyor.yml", ".appveyor.yml", "cloudbuild.yaml", "cloudbuild.yml", "buildkite.yml", "codecov.yml", ".codecov.yml", "renovate.json", ".renovaterc", ".renovaterc.json", "dependabot.yml",
		".pre-commit-config.yaml", ".pre-commit-hooks.yaml", ".lintstagedrc", "lint-staged.config.js", "commitlint.config.js", ".huskyrc", ".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json",
		".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml", ".prettierrc", ".prettierrc.json", ".prettierrc.yml", ".prettierrc.yaml", ".prettierrc.js", "prettier.config.js", ".stylelintrc", ".shellcheckrc", ".editorconfig", ".rubocop.yml", ".flake8", "ruff.toml", ".ruff.toml", "mypy.ini", "tox.ini", "lefthook.yml", "lefthook.yaml", ".lefthook.yml",
		"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "noxfile.py", "Cargo.toml", "build.rs", "Gemfile", "Rakefile", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradlew", "gradlew.bat", "mvnw", "mvnw.cmd",
		"composer.json", "CMakeLists.txt", "meson.build", "justfile", "Taskfile.yml", "Taskfile.yaml", "deno.json", "deno.jsonc", "Package.swift", "Podfile", "mix.exs", "rebar.config", "stack.yaml", "package.yaml", "pubspec.yaml", "shard.yml", "Directory.Build.props", "Directory.Build.targets", "global.json", "build.sbt", "Dockerrun.aws.json",
		"GNUmakefile", ".envrc", "Procfile", "render.yaml", "Vagrantfile", "Brewfile", "BUILD.bazel", "BUILD", "WORKSPACE", "MODULE.bazel", ".bazelrc", ".babelrc", "conftest.py", ".rspec",
	}
	if !reflect.DeepEqual(sensitiveBaseNames, wantBaseNames) {
		t.Fatalf("sensitive file names changed: %q", sensitiveBaseNames)
	}
	if want := []string{"go.work", "dockerfile.", "containerfile.", "docker-compose.", "eslint.config.", ".golangci.", "lefthook", "jest.config.", "vitest.config.", "webpack.config.", "babel.config.", "next.config.", "vite.config.", "gulpfile."}; !reflect.DeepEqual(sensitiveBasePrefixes, want) {
		t.Fatalf("sensitive file name prefixes changed: %q", sensitiveBasePrefixes)
	}
	if want := []string{".lock", ".csproj", ".fsproj", ".vbproj", ".sln", ".gemspec", ".gradle", ".dockerfile", ".mk", ".tf"}; !reflect.DeepEqual(sensitiveExtensions, want) {
		t.Fatalf("sensitive extensions changed: %q", sensitiveExtensions)
	}
	for _, prefix := range sensitivePathPrefixes {
		if !strings.HasSuffix(prefix, "/") || prefix != strings.ToLower(prefix) {
			t.Fatalf("directory %q must be lower case and end in a slash", prefix)
		}
	}
	for _, prefix := range sensitiveBasePrefixes {
		if prefix != strings.ToLower(prefix) {
			t.Fatalf("file name prefix %q must be lower case", prefix)
		}
	}
	required := []string{"go.work", ".gitmodules", ".gitattributes", "CODEOWNERS", ".mcp.json", "CLAUDE.md", "AGENTS.md", ".cursor/", "vendor/", ".circleci/", "Dockerfile", ".npmrc", ".pre-commit-config.yaml", ".golangci.yml", "pyproject.toml", "Cargo.toml"}
	listed := append(append(append([]string{}, sensitivePathPrefixes...), sensitiveBaseNames...), sensitiveBasePrefixes...)
	for _, name := range required {
		found := false
		for _, entry := range listed {
			found = found || strings.EqualFold(entry, name)
		}
		if !found {
			t.Fatalf("the v2 list lacks %q", name)
		}
	}
}

func TestHardening129SensitivePathsMatchAtAnyDepthIgnoringCase(t *testing.T) {
	for _, file := range []string{
		"scripts/x.sh", "lab/scripts/x.sh", "pkg/.claude/settings.json", "Pkg/.CLAUDE/x", "a/b/.github/workflows/x.yml", "lab/.HUSKY/pre-commit", "x/.githooks/pre-push", "web/vendor/x.go", "vendor/x.go",
		"go.work", "lab/go.work", "lab/go.work.sum", ".gitmodules", "sub/.gitattributes", ".github/CODEOWNERS", "docs/CODEOWNERS", ".mcp.json", "x/CLAUDE.md", "agents.md", "docs/AGENTS.MD", ".cursor/rules/x.mdc", "a/.cursor/b",
		"Dockerfile", "svc/Dockerfile.prod", "svc/dockerfile.dev", "docker-compose.override.yml", "svc/compose.yaml", ".circleci/config.yml", "Jenkinsfile", "ci/azure-pipelines.yml", ".npmrc", "web/.yarnrc.yml", ".pre-commit-config.yaml",
		".golangci.yml", "lab/.golangci.yaml", "eslint.config.mjs", ".eslintrc.json", "Cargo.toml", "svc/pyproject.toml", "app/App.csproj", "Gemfile", "pom.xml", "svc/build.gradle.kts", "lefthook.yml", "poetry.lock", "svc/uv.lock",
		".forgecell/x", "web/package.json", "lab/go.mod", "lab/MAKEFILE", ".devcontainer/devcontainer.json", ".vscode/tasks.json", "svc/.cargo/config.toml",
		"GNUmakefile", "build/rules.mk", "x/.envrc", "Procfile", "deploy/render.yaml", "Vagrantfile", "Brewfile", "infra/main.tf", "svc/BUILD.bazel", "pkg/BUILD", "WORKSPACE", "MODULE.bazel", ".bazelrc",
		"web/jest.config.ts", "vitest.config.mts", "webpack.config.js", "babel.config.json", ".babelrc", "next.config.mjs", "vite.config.ts", "gulpfile.js", "tests/conftest.py", ".rspec",
	} {
		if !sensitiveScopePath(file) {
			t.Errorf("%q is a control file and must be on the list", file)
		}
	}
	for _, file := range []string{
		"docs/atoms.md", "docs/github.md", "scriptsx/run.sh", "lab/scriptsy/a.go", ".githubx/foo", "lab/internal/molecule/gate_policy.go", "lab/internal/molecule/sensitive_paths.go", "locks/readme.md", "Makefile.md",
		"vendored/x.go", "my-vendor/x.go", "docs/claude.txt", "lab/internal/foo/readme.md", "code.txt", "request.json", "tools/check.sh", "tools/check_test.go", "docs/cursor/notes.md", "docs/docker.md", "internal/pinned/pinned.go",
	} {
		if sensitiveScopePath(file) {
			t.Errorf("%q must not be on the list", file)
		}
	}
}

func namedPlan(dir string, argv []string, scope ...string) readiness.Plan {
	var paths []readiness.ScopedPath
	for _, s := range scope {
		paths = append(paths, readiness.ScopedPath{Path: s})
	}
	return readiness.Plan{Analysis: readiness.Analysis{Scope: paths, Checks: []readiness.Check{{ID: "check", Dir: dir, Argv: argv}}}}
}

func TestHardening129ScopeFilesUnderAnythingACheckNamesAreDeclined(t *testing.T) {
	const checkout = "/work/ws"
	for name, tc := range map[string]struct {
		dir   string
		argv  []string
		scope string
	}{
		"directory argument":           {".", []string{"ls", "docs"}, "docs/atoms.md"},
		"directory with a slash":       {".", []string{"ls", "docs/"}, "docs/atoms.md"},
		"relative to the check dir":    {"lab", []string{"ls", "internal"}, "lab/internal/x.txt"},
		"case-insensitive":             {".", []string{"ls", "Docs"}, "docs/a.md"},
		"root":                         {".", []string{"sh", "-c", "test -z \"$(gofmt -l .)\""}, "docs/a.md"},
		"--flag=path":                  {".", []string{"tool", "--out=build/gen"}, "build/gen/x.txt"},
		"--flag=path with a dot":       {".", []string{"tool", "--config=./conf"}, "conf/a.yml"},
		"absolute inside the checkout": {".", []string{"ls", checkout + "/internal"}, "internal/x.txt"},
		"absolute checkout root":       {".", []string{"ls", checkout}, "docs/a.md"},
		"sh -c script path":            {".", []string{"sh", "-c", "cd tools && ./run.sh"}, "tools/data.txt"},
		"sh -c with quotes":            {".", []string{"/bin/sh", "-c", "ls 'tools'"}, "tools/data.txt"},
		"bash -ec":                     {".", []string{"bash", "-ec", "ls tools; true"}, "tools/data.txt"},
		"subshell in a script":         {".", []string{"sh", "-c", "echo $(ls tools)"}, "tools/data.txt"},
		"go test with a non-go file":   {".", []string{"go", "test", "./internal/pinned/"}, "internal/pinned/testdata/x.json"},
		"go test ./... non-go file":    {"lab", []string{"go", "test", "./..."}, "lab/README.md"},
		"a go file named elsewhere":    {".", []string{"ls", "internal/pinned"}, "internal/pinned/pinned.go"},
		"a go file named twice":        {".", []string{"sh", "-c", "go test ./internal/pinned/ && ls internal/pinned"}, "internal/pinned/pinned.go"},
		"go run is not a package arg":  {".", []string{"go", "run", "./tools"}, "tools/gen.go"},
		"go -C names its directory":    {".", []string{"go", "-C", "lab", "test", "./..."}, "lab/README.md"},
		"gofmt -w writes":              {".", []string{"gofmt", "-w", "internal"}, "internal/x/format.go"},
		"gofmt -l -w writes":           {".", []string{"gofmt", "-l", "-w", "internal"}, "internal/x/format.go"},
		"gofmt -l -d is not exempt":    {".", []string{"gofmt", "-l", "-d", "internal"}, "internal/x/format.go"},
		"gofmt without -l":             {".", []string{"gofmt", "internal"}, "internal/x/format.go"},
		"gofmt -l names a non-go file": {".", []string{"gofmt", "-l", "internal"}, "internal/x/testdata/in.txt"},

		"an exact file":                     {".", []string{"cat", "code.txt"}, "code.txt"},
		"an exact file read by a script":    {".", []string{"/bin/sh", "-c", "test \"$(cat code.txt)\" = changed"}, "code.txt"},
		"an exact go file":                  {".", []string{"cat", "internal/pinned/pinned.go"}, "internal/pinned/pinned.go"},
		"an exact file in the check dir":    {"lab", []string{"cat", "notes.txt"}, "lab/notes.txt"},
		"a script run by path":              {".", []string{"sh", "-c", "./hack/run.sh"}, "hack/run.sh"},
		"a script run by bash":              {".", []string{"sh", "-c", "bash hack/run.sh"}, "hack/run.sh"},
		"a script run after cd":             {".", []string{"sh", "-c", "cd lab && ./gen.sh"}, "lab/gen.sh"},
		"a script run after cd in the dir":  {"lab", []string{"sh", "-c", "cd tools && ./gen.sh"}, "lab/tools/gen.sh"},
		"a program named by a path":         {".", []string{"./hack/check"}, "hack/check"},
		"a program in a dir":                {"lab", []string{"./hack/check", "x"}, "lab/hack/check"},
		"go vet -vettool=":                  {".", []string{"go", "vet", "-vettool=./hack/vet", "./..."}, "hack/vet"},
		"go vet -vettool= go file":          {".", []string{"go", "vet", "-vettool=x.go", "./..."}, "x.go"},
		"go vet -vettool under it":          {".", []string{"go", "vet", "-vettool=./hack/vet", "./..."}, "hack/vet/main.go"},
		"go vet -vettool separate token":    {".", []string{"go", "vet", "-vettool", "hack/vet", "./..."}, "hack/vet"},
		"go test -toolexec=":                {".", []string{"go", "test", "-toolexec=./hack/t", "./..."}, "hack/t"},
		"go test -exec=":                    {".", []string{"go", "test", "-exec=hack/wrap", "./..."}, "hack/wrap"},
		"go test -exec separate token":      {".", []string{"go", "test", "-exec", "hack/wrap", "./..."}, "hack/wrap/run.go"},
		"go build -o":                       {".", []string{"go", "build", "-o", "out/bin", "./..."}, "out/bin"},
		"go test -coverprofile":             {".", []string{"go", "test", "-coverprofile=out/c.out", "./..."}, "out/c.out"},
		"go test -args values":              {".", []string{"go", "test", "./...", "-args", "-config=./conf.yml"}, "conf.yml"},
		"go -C is not a package argument":   {".", []string{"go", "-C", "lab", "test", "./..."}, "lab/internal/x.go"},
		"go -C=":                            {".", []string{"go", "-C=lab", "test", "./..."}, "lab/README.md"},
		"--config= file":                    {".", []string{"tool", "--config=./conf.yml"}, "conf.yml"},
		"--flag=path non-go file":           {".", []string{"tool", "--out=gen/data.json"}, "gen/data.json"},
		"-f=path":                           {".", []string{"tool", "-f=conf/a.yml"}, "conf/a.yml"},
		"an attached short flag value":      {".", []string{"tool", "-Ctools"}, "tools/a.txt"},
		"an attached include flag":          {".", []string{"cc", "-Iinclude"}, "include/a.h"},
		"an env assignment value":           {".", []string{"env", "CONF=conf/a.yml", "tool"}, "conf/a.yml"},
		"a leading assignment value":        {".", []string{"CONF=conf/a.yml", "tool"}, "conf/a.yml"},
		"a colon separated flag value":      {".", []string{"tool", "--path=a:b"}, "b/x.txt"},
		"a comma separated flag value":      {".", []string{"tool", "--files=a.txt,b.txt"}, "b.txt"},
		"a glob token names its directory":  {".", []string{"ls", "tools/da*"}, "tools/data.txt"},
		"a glob in a subdirectory":          {".", []string{"ls", "tools/*/x.sh"}, "tools/sub/x.sh"},
		"a leading glob names everything":   {".", []string{"ls", "*.md"}, "docs/a.md"},
		"a script glob names everything":    {".", []string{"sh", "-c", "ls tools/*.sh"}, "docs/a.md"},
		"a script glob question mark":       {".", []string{"sh", "-c", "ls tool?"}, "docs/a.md"},
		"a script glob bracket":             {".", []string{"sh", "-c", "ls tool[s]"}, "docs/a.md"},
		"a script variable":                 {".", []string{"sh", "-c", "ls $DIR"}, "docs/a.md"},
		"a script braced variable":          {".", []string{"sh", "-c", "ls ${DIR}"}, "docs/a.md"},
		"a script arithmetic expansion":     {".", []string{"sh", "-c", "echo $((1+1))"}, "docs/a.md"},
		"a script trailing dollar":          {".", []string{"sh", "-c", "echo cost$"}, "docs/a.md"},
		"a script backtick":                 {".", []string{"sh", "-c", "ls `pwd`"}, "docs/a.md"},
		"a script eval":                     {".", []string{"sh", "-c", "eval ls"}, "docs/a.md"},
		"a script xargs":                    {".", []string{"sh", "-c", "echo x | xargs ls"}, "docs/a.md"},
		"a script find -exec":               {".", []string{"sh", "-c", "find tools -exec ls {} ;"}, "docs/a.md"},
		"a script source":                   {".", []string{"sh", "-c", "source env.sh"}, "docs/a.md"},
		"a script dot":                      {".", []string{"sh", "-c", ". ./env.sh"}, "docs/a.md"},
		"a script with a go test and a var": {".", []string{"sh", "-c", "go test ./internal/pinned/ -run ^TestX$"}, "internal/pinned/pinned.go"},
		"sh without -c runs the file":       {".", []string{"sh", "hack/run.sh"}, "hack/run.sh"},
		"env wrapper around a script":       {".", []string{"env", "A=1", "sh", "hack/run.sh"}, "hack/run.sh"},
		"a path after the script":           {".", []string{"sh", "-c", "true", "x", "hack/arg.txt"}, "hack/arg.txt"},

		"GOFLAGS -toolexec via env":                {".", []string{"env", "GOFLAGS=-toolexec=./hack/t", "go", "test", "./..."}, "hack/t/main.go"},
		"GOFLAGS -exec via env":                    {".", []string{"env", "GOFLAGS=-exec=hack/wrap", "go", "test", "./..."}, "hack/wrap/main.go"},
		"GOFLAGS -vettool via env":                 {".", []string{"env", "GOFLAGS=-vettool=./hack/vet", "go", "vet", "./..."}, "hack/vet/main.go"},
		"GOFLAGS first of several via env":         {".", []string{"env", "GOFLAGS=-exec=hack/a -vettool=hack/b", "go", "test", "./..."}, "hack/a/main.go"},
		"GOFLAGS last of several via env":          {".", []string{"env", "GOFLAGS=-exec=hack/a -vettool=hack/b", "go", "test", "./..."}, "hack/b/main.go"},
		"GOFLAGS bare word after a flag via env":   {".", []string{"env", "GOFLAGS=-exec hack/wrap", "go", "test", "./..."}, "hack/wrap/main.go"},
		"GOFLAGS -toolexec via a leading prefix":   {".", []string{"GOFLAGS=-toolexec=./hack/t", "go", "test", "./..."}, "hack/t/main.go"},
		"GOFLAGS -toolexec via a script prefix":    {".", []string{"sh", "-c", "GOFLAGS=-toolexec=./hack/t go test ./..."}, "hack/t/main.go"},
		"GOFLAGS -exec via a script prefix":        {".", []string{"sh", "-c", "GOFLAGS=-exec=hack/wrap go test ./..."}, "hack/wrap/main.go"},
		"GOFLAGS -vettool via a script prefix":     {".", []string{"sh", "-c", "GOFLAGS=-vettool=./hack/vet go vet ./..."}, "hack/vet/main.go"},
		"GOFLAGS first quoted in a script":         {".", []string{"sh", "-c", "GOFLAGS='-exec=hack/a -vettool=hack/b' go test ./..."}, "hack/a/main.go"},
		"GOFLAGS last quoted in a script":          {".", []string{"sh", "-c", "GOFLAGS='-exec=hack/a -vettool=hack/b' go test ./..."}, "hack/b/main.go"},
		"GOFLAGS via env inside a script":          {".", []string{"sh", "-c", "env GOFLAGS=-toolexec=./hack/t go test ./..."}, "hack/t/main.go"},
		"python -c":                                {".", []string{"python", "-c", "import hack.gen"}, "hack/gen.py"},
		"python3 -c in a script":                   {".", []string{"sh", "-c", "python3 -c 'import hack.gen'"}, "hack/gen.py"},
		"python3.11 -c":                            {".", []string{"python3.11", "-c", "import hack.gen"}, "hack/gen.py"},
		"python -m":                                {".", []string{"python", "-m", "hack.gen"}, "hack/gen.py"},
		"python -c by absolute interpreter path":   {".", []string{"/usr/bin/python3", "-c", "import hack.gen"}, "hack/gen.py"},
		"python -c after a flag value":             {".", []string{"python", "-W", "ignore", "-c", "import hack.gen"}, "hack/gen.py"},
		"python -c behind env":                     {".", []string{"env", "A=1", "python", "-c", "import hack.gen"}, "hack/gen.py"},
		"node -e":                                  {".", []string{"node", "-e", "require('./hack/x')"}, "hack/x.js"},
		"node --eval=":                             {".", []string{"node", "--eval=require('./hack/x')"}, "hack/x.js"},
		"node -p":                                  {".", []string{"node", "-p", "require('./hack/x')"}, "hack/x.js"},
		"node -e in a script":                      {".", []string{"sh", "-c", "node -e \"require('./hack/x')\""}, "hack/x.js"},
		"perl -e":                                  {".", []string{"perl", "-e", "require './hack/x.pl'"}, "hack/x.pl"},
		"perl -E":                                  {".", []string{"perl", "-E", "say 1"}, "hack/x.pl"},
		"perl -ne":                                 {".", []string{"perl", "-ne", "print", "data.txt"}, "hack/x.pl"},
		"ruby -e":                                  {".", []string{"ruby", "-e", "require './hack/x'"}, "hack/x.rb"},
		"php -r":                                   {".", []string{"php", "-r", "require 'hack/x.php';"}, "hack/x.php"},
		"bun -e":                                   {".", []string{"bun", "-e", "import './hack/x'"}, "hack/x.ts"},
		"deno eval":                                {".", []string{"deno", "eval", "import './hack/x.ts'"}, "hack/x.ts"},
		"awk program":                              {".", []string{"awk", "{print}", "data.txt"}, "hack/x.awk"},
		"awk with a separator and a program":       {".", []string{"awk", "-F:", "{print $1}", "data.txt"}, "hack/x.awk"},
		"gawk -e":                                  {".", []string{"gawk", "-f", "x.awk", "-e", "BEGIN{}"}, "hack/x.awk"},
		"gofmt -l -cpuprofile value":               {".", []string{"gofmt", "-l", "-cpuprofile", "prof/cpu.go", "internal"}, "prof/cpu.go"},
		"gofmt -l -cpuprofile=value":               {".", []string{"gofmt", "-l", "-cpuprofile=prof/cpu.go", "internal"}, "prof/cpu.go"},
		"gofmt -l -r value":                        {".", []string{"gofmt", "-l", "-r", "tools/rule.go", "internal"}, "tools/rule.go"},
		"gofmt -l -cpuprofile value in a script":   {".", []string{"sh", "-c", "gofmt -l -cpuprofile prof/cpu.go internal"}, "prof/cpu.go"},
		"gofmt -l -cpuprofile value in a cd":       {".", []string{"sh", "-c", "cd lab && gofmt -l -cpuprofile prof/cpu.go internal"}, "lab/prof/cpu.go"},
		"gofmt -l -cpuprofile value, check in dir": {"lab", []string{"gofmt", "-l", "-cpuprofile", "prof/cpu.go", "internal"}, "lab/prof/cpu.go"},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkNamedScopePath(namedPlan(tc.dir, tc.argv, tc.scope), []string{checkout})
			if got == "" || !strings.Contains(got, tc.scope) || !strings.Contains(got, "which check check names") {
				t.Fatalf("a scope file under a named directory was not declined: %q", got)
			}
		})
	}
}

func TestHardening129GoPackageArgumentsExemptOnlyGoFiles(t *testing.T) {
	const checkout = "/work/ws"
	for name, tc := range map[string]struct {
		dir   string
		argv  []string
		scope []string
	}{
		"go test one package":            {".", []string{"go", "test", "./internal/pinned/"}, []string{"internal/pinned/pinned.go", "internal/pinned/pinned_test.go"}},
		"go test all":                    {"lab", []string{"go", "test", "-p", "1", "./..."}, []string{"lab/internal/a/a.go", "lab/internal/a/a_test.go"}},
		"go vet":                         {".", []string{"go", "vet", "./internal/..."}, []string{"internal/a/a.go"}},
		"go build":                       {".", []string{"go", "build", "./cmd/forgecell"}, []string{"cmd/forgecell/main.go"}},
		"go test in an sh script":        {".", []string{"sh", "-c", "go test -run 'TestX' ./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"unrelated directory":            {".", []string{"ls", "docs"}, []string{"internal/pinned/pinned.go"}},
		"another exact file":             {".", []string{"cat", "code.txt"}, []string{"request.json"}},
		"absolute outside":               {".", []string{"ls", "/elsewhere/internal"}, []string{"internal/x.txt"}},
		"a script reading another file":  {".", []string{"/bin/sh", "-c", "test \"$(cat package.json)\" = x"}, []string{"code.txt", "request.json"}},
		"flag without a path":            {".", []string{"go", "test", "-run=TestX", "./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"flag values that are not paths": {".", []string{"go", "test", "-p", "1", "-count=1", "-timeout", "5m", "-run", "TestX.*", "./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"go test -json and -race":        {".", []string{"go", "test", "-json", "-race", "./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"go test with env":               {".", []string{"env", "GOFLAGS=-mod=mod", "go", "test", "./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"a command substitution":         {".", []string{"sh", "-c", "test -z \"$(gofmt -l internal)\""}, []string{"internal/x/format.go"}},
		"gofmt -l directories":           {"lab", []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}, []string{"lab/internal/x/format.go", "lab/internal/x/format_test.go"}},
		"gofmt -l root":                  {".", []string{"gofmt", "-l", "."}, []string{"internal/x/format.go"}},
		"gofmt -l in a script":           {".", []string{"sh", "-c", `test -z "$(gofmt -l .)"`}, []string{"internal/x/format.go"}},
		"gofmt -s -l":                    {".", []string{"gofmt", "-s", "-l", "internal"}, []string{"internal/x/format.go"}},

		"gofmt -l -cpuprofile for another file": {".", []string{"gofmt", "-l", "-cpuprofile", "prof.out", "internal"}, []string{"internal/x/format.go"}},
		"gofmt -l -r for another file":          {".", []string{"gofmt", "-l", "-r", "a -> b", "internal"}, []string{"internal/x/format.go"}},
		"GOFLAGS that name another file":        {".", []string{"env", "GOFLAGS=-exec=hack/wrap", "go", "test", "./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"GOFLAGS that are not paths":            {".", []string{"sh", "-c", "GOFLAGS=-mod=mod go test ./internal/pinned/"}, []string{"internal/pinned/pinned.go"}},
		"python running a script":               {".", []string{"python", "hack/gen.py"}, []string{"docs/a.md"}},
		"python version":                        {".", []string{"python", "--version"}, []string{"docs/a.md"}},
		"python script with arguments":          {".", []string{"python", "-u", "hack/gen.py", "--out", "build"}, []string{"docs/a.md"}},
		"node running a script":                 {".", []string{"node", "hack/x.js"}, []string{"docs/a.md"}},
		"awk with a program file":               {".", []string{"awk", "-f", "tools/x.awk", "data.txt"}, []string{"docs/a.md"}},
		"an unrelated program":                  {".", []string{"pythonic", "-c", "x"}, []string{"docs/a.md"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := checkNamedScopePath(namedPlan(tc.dir, tc.argv, tc.scope...), []string{checkout}); got != "" {
				t.Fatalf("declined without cause: %s", got)
			}
		})
	}
}

func TestHardening129PolicyDeclinesSensitiveAndNamedScopePaths(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	assertPolicyApproves(t, ready)
	scoped := func(t *testing.T, file string) Record {
		t.Helper()
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = file
		rebindPlanDigest(t, &r)
		return r
	}
	for _, file := range []string{"lab/scripts/x.sh", "pkg/.claude/settings.json", "Pkg/.CLAUDE/x", "a/.github/workflows/x.yml", "web/vendor/x.go", "go.work", "docs/CODEOWNERS", ".mcp.json", "CLAUDE.md", ".cursor/rules/a.mdc", "svc/Dockerfile", ".circleci/config.yml", ".npmrc", ".golangci.yml", "Cargo.toml"} {
		t.Run("declines "+file, func(t *testing.T) {
			got := assertPolicyDeclines(t, scoped(t, file), "scope-path-sensitive")
			if !strings.Contains(got.Detail, file) || !strings.Contains(got.Detail, "v2") {
				t.Fatalf("detail does not name the path and list version: %q", got.Detail)
			}
		})
	}
	for _, file := range []string{"docs/atoms.md", "scriptsx/run.sh", ".githubx/foo", "lab/internal/molecule/gate_policy.go"} {
		t.Run("approves "+file, func(t *testing.T) { assertPolicyApproves(t, scoped(t, file)) })
	}
	t.Run("declines a scope file under a directory a check names", func(t *testing.T) {
		r := cloneRecord(t, ready)
		plan := &r.Readiness.Plans[0].Plan
		plan.Analysis.Scope[0].Path = "tools/data.txt"
		plan.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "ls tools"}
		rebindPlanDigest(t, &r)
		got := assertPolicyDeclines(t, r, "scope-path-check-argument")
		if !strings.Contains(got.Detail, "tools/data.txt") || !strings.Contains(got.Detail, "check") || !strings.Contains(got.Detail, "human must review") {
			t.Fatalf("detail does not name the file, the directory and the check: %q", got.Detail)
		}
	})
	t.Run("an exact file named by a check keeps its own reason", func(t *testing.T) {
		r := cloneRecord(t, ready)
		plan := &r.Readiness.Plans[0].Plan
		plan.Analysis.Scope[0].Path = "tools/check.sh"
		plan.Analysis.Checks[0].Argv = []string{"/bin/sh", "tools/check.sh"}
		rebindPlanDigest(t, &r)
		assertPolicyDeclines(t, r, "scope-path-check-definition")
	})
}

func TestHardening129PolicyDeclinesScopeFilesAChecksNamesOrRuns(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	assertPolicyApproves(t, ready)
	for name, tc := range map[string]struct {
		file string
		argv []string
	}{
		"a script run by a nested bash":       {"hack/run.sh", []string{"/bin/sh", "-c", "bash hack/run.sh"}},
		"a vettool flag":                      {"hack/vet", []string{"go", "vet", "-vettool=./hack/vet", "./..."}},
		"a toolexec flag":                     {"hack/t", []string{"go", "test", "-toolexec=./hack/t", "./..."}},
		"a config flag":                       {"conf.yml", []string{"tool", "--config=./conf.yml"}},
		"an expansion names everything":       {"docs/a.md", []string{"/bin/sh", "-c", "ls $DIR"}},
		"a glob in a script names everything": {"docs/a.md", []string{"/bin/sh", "-c", "ls tools/*.sh"}},
		"inline code names everything":        {"docs/a.md", []string{"python", "-c", "import hack.gen"}},
		"a GOFLAGS exec flag in env":          {"hack/wrap/main.go", []string{"env", "GOFLAGS=-exec=hack/wrap", "go", "test", "./..."}},
		"a GOFLAGS toolexec flag in a script": {"hack/t/main.go", []string{"/bin/sh", "-c", "GOFLAGS=-toolexec=./hack/t go test ./..."}},
	} {
		t.Run(name, func(t *testing.T) {
			r := cloneRecord(t, ready)
			plan := &r.Readiness.Plans[0].Plan
			plan.Analysis.Scope[0].Path = tc.file
			plan.Analysis.Checks[0].Argv = tc.argv
			rebindPlanDigest(t, &r)
			got := assertPolicyDeclines(t, r, "scope-path-check-argument")
			if !strings.Contains(got.Detail, tc.file) || !strings.Contains(got.Detail, "which check") || !strings.Contains(got.Detail, "human must review") {
				t.Fatalf("detail does not name the file and the check: %q", got.Detail)
			}
		})
	}
}

func TestHardening129PolicyEvidenceListsNewFileDefinitions(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	const newFile = "pinned/pinned_test.go"
	plain := EvaluateGatePolicy(ready, formula.ScopeApproval)
	for _, e := range plain.Evidence {
		if strings.HasSuffix(e.Pointer, "/newFiles") {
			t.Fatalf("a plan without new-file definitions must not record the entry: %+v", e)
		}
	}
	r := cloneRecord(t, ready)
	plan := &r.Readiness.Plans[0].Plan
	plan.Analysis.Scope = append(plan.Analysis.Scope, readiness.ScopedPath{Path: newFile, Reason: "Tests of the new package", Evidence: []string{"issue"}})
	plan.Analysis.Checks[0].Definitions = append(plan.Analysis.Checks[0].Definitions, newFile)
	plan.NewFiles = []string{newFile}
	rebindPlanDigest(t, &r)
	got := EvaluateGatePolicy(r, formula.ScopeApproval)
	if got.Outcome != PolicyApprove {
		t.Fatalf("a candidate check that also cites a new file must stay approvable: %+v", got)
	}
	found := false
	for _, e := range got.Evidence {
		if e.Pointer == "/readiness/plans/0/plan/newFiles" {
			found = true
			if e.Observed != "1 new-file definitions: "+newFile {
				t.Fatalf("new-file evidence observed %q", e.Observed)
			}
		}
	}
	if !found {
		t.Fatalf("the policy evidence must list the new-file definitions: %+v", got.Evidence)
	}
}

func TestHardening129ScopeGateShowsTheListWhenAPolicyMayApprove(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	f, err := formula.Parse([]byte(ready.FormulaSnapshot.YAML))
	if err != nil {
		t.Fatal(err)
	}
	note, ok := scopeGateSensitivePathsNote(f)
	if !ok {
		t.Fatal("a Formula whose scope gate declares auto-when-ready must show the list")
	}
	for _, want := range []string{"v2", ".mcp.json", "go.work", "vendor/", "CODEOWNERS", "Dockerfile", ".circleci/", "--flag=path", "sh -c", "GNUmakefile", "Procfile", ".tf", "jest.config.", "glob patterns", "names or runs"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the scope gate note lacks %q: %s", want, note)
		}
	}
	_, human := scopeReadyRecord(t, defaultScopeAtom, defaultReviewAtom)
	f, err = formula.Parse([]byte(human.FormulaSnapshot.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok = scopeGateSensitivePathsNote(f); ok {
		t.Fatal("a human scope gate has no policy list to show")
	}
}
