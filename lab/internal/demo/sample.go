package demo

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// The sample files carry a .txt suffix: a nested go.mod would make the directory a
// separate module that go:embed cannot reach, and a .go file would build into this
// package.

//go:embed sample/go.mod.txt
var sampleGoMod []byte

//go:embed sample/greeting.go.txt
var sampleGreeting []byte

const (
	// Repository is the GitHub-shaped identity of the sample. Its origin URL is
	// recorded so the real intake and delivery checks pass; it is never contacted.
	Repository = "forgecell-demo/greeting"
	// IssueNumber is the number of the built-in issue fixture.
	IssueNumber = 1
	baseBranch  = "main"
	originURL   = "https://github.com/" + Repository + ".git"

	// maxSampleFileBytes bounds each embedded sample file and each file the stub
	// harness compares against one.
	maxSampleFileBytes = 16 << 10
	sampleGitTimeout   = 30 * time.Second
	sampleCommitDate   = "2026-01-01T00:00:00Z"
)

const (
	issueTitle = "Add a Greeting(name) function with a test"
	issueBody  = "The sample package only has Hello. Add `Greeting(name string) string` to `greeting.go`, returning \"Hello, NAME!\" for a name and falling back to Hello() when the name is blank, and cover it with a table test in `greeting_test.go`."
)

// issueURL is the exact URL intake requires for the fixture.
func issueURL() string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", Repository, IssueNumber)
}

// fixtureIssue is the built-in issue. It replaces the GitHub read.
func fixtureIssue() molecule.Issue {
	return molecule.Issue{UpdatedAt: sampleCommitDate, Number: IssueNumber, Repo: Repository, Title: issueTitle, Body: issueBody, URL: issueURL(), State: "OPEN"}
}

// readFixtureIssue is the demo's Options.ReadIssue: it answers only for the
// fixture and never runs gh.
func readFixtureIssue(_ context.Context, repo string, number int64, _ string) (molecule.Issue, error) {
	if !strings.EqualFold(repo, Repository) || number != IssueNumber {
		return molecule.Issue{}, fmt.Errorf("the demo serves only issue #%d of %s", IssueNumber, Repository)
	}
	return fixtureIssue(), nil
}

// listFixtureIssues is the demo loop's issue listing: the fixture alone.
func listFixtureIssues(_ context.Context, repo string) (intake.OpenIssues, error) {
	if !strings.EqualFold(repo, Repository) {
		return intake.OpenIssues{}, fmt.Errorf("the demo lists only %s", Repository)
	}
	created, _ := time.Parse(time.RFC3339, sampleCommitDate)
	return intake.OpenIssues{Issues: []intake.OpenIssue{{Repository: Repository, Number: IssueNumber, Title: issueTitle, URL: issueURL(), CreatedAt: created}}}, nil
}

type sampleFile struct {
	name string
	data []byte
}

func sampleFiles() []sampleFile {
	return []sampleFile{{"go.mod", sampleGoMod}, {"greeting.go", sampleGreeting}}
}

// writeNewFile creates dir/name, which must not exist yet. name is one fixed path
// element, the file is never followed through a symbolic link, and the content is
// bounded.
func writeNewFile(dir, name string, data []byte, perm os.FileMode) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`+"\x00") {
		return fmt.Errorf("invalid sample file name %q", name)
	}
	if len(data) > maxSampleFileBytes {
		return fmt.Errorf("sample file %s exceeds %d bytes", name, maxSampleFileBytes)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeSample writes the embedded sample into dir, an empty directory the demo
// created.
func writeSample(dir string) error {
	for _, f := range sampleFiles() {
		if err := writeNewFile(dir, f.name, f.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// sampleGitEnv keeps the user's global and system Git configuration (hooks,
// templates, signing) out of the sample's own setup and pins its identity and
// dates, so the base commit is the same on every run.
func sampleGitEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Forgecell demo", "GIT_AUTHOR_EMAIL=demo@forgecell.invalid", "GIT_AUTHOR_DATE="+sampleCommitDate,
		"GIT_COMMITTER_NAME=Forgecell demo", "GIT_COMMITTER_EMAIL=demo@forgecell.invalid", "GIT_COMMITTER_DATE="+sampleCommitDate)
}

func sampleGit(ctx context.Context, dir string, args ...string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: dir, Env: sampleGitEnv(), Stdin: []byte{}, Timeout: sampleGitTimeout, MaxOutputBytes: 64 << 10})
	if !r.OK {
		return "", fmt.Errorf("git %s failed: %s %s", args[0], r.Error, strings.TrimSpace(r.Stderr))
	}
	return strings.TrimSpace(r.Stdout), nil
}

// initSampleRepository makes dir a Git repository whose main branch holds the
// sample, with a GitHub-shaped origin that is never fetched and a local
// remote-tracking ref for the delivery preview's base check. The commit is made
// with plumbing, so no hook runs. It returns the base commit.
func initSampleRepository(ctx context.Context, dir string) (string, error) {
	if err := writeSample(dir); err != nil {
		return "", err
	}
	steps := [][]string{
		{"init", "-q", "--template="},
		{"symbolic-ref", "HEAD", "refs/heads/" + baseBranch},
		{"add", "--"},
	}
	for _, f := range sampleFiles() {
		steps[2] = append(steps[2], f.name)
	}
	for _, step := range steps {
		if _, err := sampleGit(ctx, dir, step...); err != nil {
			return "", err
		}
	}
	tree, err := sampleGit(ctx, dir, "write-tree")
	if err != nil {
		return "", err
	}
	commit, err := sampleGit(ctx, dir, "commit-tree", tree, "-m", "Forgecell demo sample")
	if err != nil {
		return "", err
	}
	for _, step := range [][]string{
		{"update-ref", "refs/heads/" + baseBranch, commit},
		{"update-ref", "refs/remotes/origin/" + baseBranch, commit},
		{"remote", "add", "origin", originURL},
	} {
		if _, err := sampleGit(ctx, dir, step...); err != nil {
			return "", err
		}
	}
	return commit, nil
}
