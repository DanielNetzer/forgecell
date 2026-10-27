package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/ui"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// openBrowser opens a local file or URL in the default browser without a shell;
// tests replace it.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// maxGitUserNameBytes bounds each git identity read; a longer value is refused
// and the UI shows its neutral fallback.
const maxGitUserNameBytes = 1024

// gitUserNameTimeout bounds each git identity read at start-up.
const gitUserNameTimeout = 5 * time.Second

// The two fixed git reads behind the avatar: the configured name, then the
// author identity git itself would commit with (for example from the OS
// account's full name) when no name is configured.
var (
	gitUserNameArgs    = []string{"config", "--get", "user.name"}
	gitAuthorIdentArgs = []string{"var", "GIT_AUTHOR_IDENT"}
)

// gitIdentity runs git with one of the fixed argv above in dir, without a
// shell, with a bounded output and a timeout; tests replace it so no real git
// config is read.
var gitIdentity = func(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitUserNameTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out := &boundedBuffer{limit: maxGitUserNameBytes}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.overflow {
		return "", fmt.Errorf("git %s is longer than %d bytes", args[0], maxGitUserNameBytes)
	}
	return out.b.String(), nil
}

// boundedBuffer keeps at most limit bytes and records whether more were written.
type boundedBuffer struct {
	b        strings.Builder
	limit    int
	overflow bool
}

// Write reports the whole of p as written, so the process is drained rather than
// blocked; the excess is dropped.
func (w *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.limit - w.b.Len(); n > room {
		w.overflow = true
		p = p[:max(room, 0)]
	}
	w.b.Write(p)
	return n, nil
}

// operatorName is the source checkout's git config user.name for the avatar or,
// when that is unset or empty, the name part of git var GIT_AUTHOR_IDENT; it is
// "" when neither can be read, both are empty or a value is oversized. Errors
// are ignored and never printed: they can name local paths. The UI redacts and
// bounds the value and refuses any email in it.
func operatorName(ctx context.Context, dir string) string {
	if name := gitRead(ctx, dir, gitUserNameArgs); name != "" {
		return name
	}
	return identName(gitRead(ctx, dir, gitAuthorIdentArgs))
}

// gitRead is one trimmed identity read, or "" on an error or an oversized value.
func gitRead(ctx context.Context, dir string, args []string) string {
	out, err := gitIdentity(ctx, dir, args...)
	if err != nil || len(out) > maxGitUserNameBytes {
		return ""
	}
	return strings.TrimSpace(out)
}

// identName is the name before " <" in a git ident ("Name <email> 1700000000
// +0000"), so the email and the timestamp are never kept; an ident without that
// shape gives "".
func identName(ident string) string {
	name, _, ok := strings.Cut(ident, " <")
	if !ok || strings.ContainsAny(name, "<>@\n") {
		return ""
	}
	return strings.TrimSpace(name)
}

// runUI serves the Lab UI on 127.0.0.1 until ctx is cancelled (SIGINT or SIGTERM
// in the binary). It reads the Lab, and writes only through the engine: the
// human gates (exact scope approval and draft PR publication), starting intake
// or the loop, and stopping work it started. It never merges or deploys.
func runUI(ctx context.Context, args []string, defaultLab, version string, out, stderr io.Writer) int {
	return runUIChoosing(ctx, args, defaultLab, nil, version, out, stderr)
}

// runUIChoosing is runUI with the Labs to choose from when more than one
// matches the checkout's repository. A Lab with no approved Formula yet, or a
// chooser, is served as Set up your Lab (#263), whose every step is an engine
// call; the resource guard opens only once the Lab is ready.
func runUIChoosing(ctx context.Context, args []string, defaultLab string, choices []string, version string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	flags := flag.NewFlagSet("ui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lab := flags.String("lab", defaultLab, "Lab directory")
	port := flags.Int("port", 0, "Loopback port (default: a free port)")
	noOpen := flags.Bool("no-open", false, "Print the URL without opening a browser")
	demo := flags.Bool("demo", false, "Serve synthetic sample data instead of the Lab")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("ui accepts only --lab, --lab-name, --port, --no-open and --demo"))
	}
	explicitLab := false
	flags.Visit(func(f *flag.Flag) { explicitLab = explicitLab || f.Name == "lab" })
	if explicitLab {
		choices = nil
	}
	if !*demo && *lab == "" && len(choices) == 0 {
		return fail(fmt.Errorf("no Lab selected; use --lab DIR"))
	}
	options := ui.Options{LabDir: *lab, Version: version, Demo: *demo, Port: *port}
	setup := false
	if !*demo {
		// Work started from the UI runs from this checkout under the Lab's resource
		// guard, as forgecell run and loop do.
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		setup = len(choices) > 1 || ui.NeedsSetup(*lab)
		if setup && !explicitLab && os.Getenv("FORGECELL_LAB") == "" {
			// As forgecell init does, a Lab kept in the checkout is continued
			// explicitly, never set up again beside it.
			if legacy := existingCheckoutLab(); legacy != "" {
				return fail(fmt.Errorf("existing checkout Lab at %s; use --lab %s to continue it, or move its contents to the home Lab before retrying", safeText(legacy), shellWord(legacy)))
			}
		}
		if setup {
			options.Setup = uiSetupOptions(choices, stderr)
		} else {
			guard, err := newResourceGuard(*lab, stderr)
			if err != nil {
				return fail(err)
			}
			options.Resources = guard
		}
		options.SourceRoot = cwd
		// A Formula rebind from the UI saves the launcher init --rebind would, and
		// only one that resolves to a build this user owns under the managed
		// Forgecell home; otherwise the Formula view says the launcher is unknown.
		launcher := initLauncher()
		if launcher == "" {
			launcher, _ = os.Executable()
		}
		if root, e := resolvedCheckoutRoot(); e == nil {
			if home, e := forgecellHome(root); e == nil {
				options.Home = home
				options.Launcher = ui.ValidateLauncher(home, launcher)
			}
		}
		// The avatar's name; demo mode never reads git.
		options.Operator = operatorName(ctx, cwd)
	}
	s, err := ui.Listen(options)
	if err != nil {
		return fail(err)
	}
	switch {
	case *demo:
		fmt.Fprintln(out, "Forgecell Lab UI · Sample data (synthetic; no Lab is read)")
	case len(choices) > 1:
		fmt.Fprintf(out, "Forgecell Lab UI · Choose a Lab: %d Labs match this repository\n", len(choices))
		fmt.Fprintln(out, "Writes only through the engine, the same paths as forgecell init: creating the Lab and approving its first Formula only when you click Approve Formula. It never merges or deploys.")
	case setup:
		fmt.Fprintf(out, "Forgecell Lab UI · Set up your Lab %s\n", safeText(*lab))
		fmt.Fprintln(out, "Writes only through the engine, the same paths as forgecell init: creating the Lab and approving its first Formula only when you click Approve Formula. It never merges or deploys.")
	default:
		fmt.Fprintf(out, "Forgecell Lab UI · Lab %s\n", safeText(*lab))
		fmt.Fprintln(out, "Writes only through the engine: exact scope approval, draft PR publication, starting intake or the loop from this checkout, and stopping work it started. It never merges or deploys.")
	}
	// The URL is always printed, since a browser that cannot read the launch page
	// (a snap-confined one cannot read the temp directory) needs it pasted.
	fmt.Fprintln(out, s.URL)
	fmt.Fprintln(out, "The link works once. Serving on 127.0.0.1 only. Press Ctrl-C to stop.")
	if !*noOpen {
		page, remove, err := launchPage(s.URL)
		if err == nil {
			defer remove()
			// The page is spent with the token, so it goes at the first exchange.
			go func() {
				select {
				case <-s.Exchanged():
					remove()
				case <-ctx.Done():
				}
			}()
			err = openBrowser(page)
		}
		if err != nil {
			fmt.Fprintln(stderr, "forgecell: could not open a browser; open the single-use URL above:", err)
		}
	}
	if err := s.Serve(ctx); err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, "Lab UI stopped.")
	return 0
}

// launchPage writes a 0600 page that forwards to url into a new Forgecell temp
// directory and returns its path, so the browser is opened on a file and the
// launch token never appears in the opener's argv, which other local users can
// read. remove deletes the directory; it is also removed if Forgecell is killed
// by SIGINT or SIGTERM before remove runs.
func launchPage(url string) (path string, remove func(), err error) {
	dir, err := resources.MkdirTemp("", "forgecell-ui-")
	if err != nil {
		return "", nil, err
	}
	remove = sync.OnceFunc(func() { _ = resources.Remove(dir) })
	path = filepath.Join(dir, "open.html")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		target := html.EscapeString(url)
		_, err = io.WriteString(f, `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta http-equiv="refresh" content="0;url=`+target+`"><title>Forgecell Lab UI</title><a href="`+target+`">Open the Forgecell Lab UI</a>`)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		remove()
		return "", nil, err
	}
	return path, remove, nil
}

// demoRequested reports ui --demo among args before any "--", so a demo launch
// never resolves, or touches, the default Lab.
// The last occurrence wins, as it does for the flag itself.
func demoRequested(args []string) bool {
	demo := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || name != "demo" {
			continue
		}
		on, err := strconv.ParseBool(value)
		demo = !hasValue || (err == nil && on)
	}
	return demo
}
