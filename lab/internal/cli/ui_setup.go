package cli

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/ui"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// uiSetupOptions are the engine calls behind Set up your Lab (#263): the paths
// forgecell init and doctor use, never a file the UI writes itself. Labs are
// the matching Labs to choose from, if more than one. Tests replace it.
var uiSetupOptions = func(labs []string, stderr io.Writer) *ui.SetupOptions {
	launcher := initLauncher()
	if launcher == "" {
		// As forgecell init does, the running executable is the launcher; if even
		// that is unknown, Launcher stays empty and creating the Lab is refused.
		launcher, _ = os.Executable()
	}
	env := map[string]string{}
	for _, key := range []string{"CODEX_THREAD_ID", "CLAUDECODE", "CURSOR_AGENT", "TERM_PROGRAM"} {
		env[key] = os.Getenv(key)
	}
	return &ui.SetupOptions{
		Labs:     labs,
		Launcher: launcher,
		Env:      env,
		ReadRepository: func(ctx context.Context, cwd string) (onboarding.Repository, error) {
			return onboarding.ReadRepository(ctx, cwd, nil)
		},
		Discover: func(ctx context.Context, cwd string) []onboarding.Candidate {
			return onboarding.Discover(ctx, cwd, doctorRun)
		},
		Prepare: onboarding.Prepare,
		Review:  onboarding.Review,
		Resources: func(lab string) (*resources.Guard, error) {
			return newResourceGuard(lab, stderr)
		},
	}
}

// uiLabChoices lists the Labs that match the checkout's repository when more
// than one does, so forgecell ui offers a chooser where other commands ask for
// --lab DIR. It only reads; FORGECELL_LAB, a Lab name, no GitHub origin or a
// truncated or failed scan gives none, and the resolution error stands. An
// unreadable Lab is never offered, as resolution never considers one.
func uiLabChoices(ctx context.Context, name string) []string {
	if os.Getenv("FORGECELL_LAB") != "" || name != "" {
		return nil
	}
	root, err := resolvedCheckoutRoot()
	if err != nil {
		return nil
	}
	repo, _ := checkoutRepository(ctx, root)
	if repo == "" {
		return nil
	}
	home, err := forgecellHome(root)
	if err != nil {
		return nil
	}
	entries, truncated, err := scanLabs(filepath.Join(home, "labs"))
	if err != nil || truncated {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if e.Err == "" && strings.EqualFold(e.Repo, repo) {
			matches = append(matches, e.Dir)
		}
	}
	if len(matches) < 2 {
		return nil
	}
	return matches
}
