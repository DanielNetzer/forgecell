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

// runUI serves the Lab UI on 127.0.0.1 until ctx is cancelled (SIGINT or SIGTERM
// in the binary). It reads the Lab, and writes only through the engine's human
// gates: exact scope approval and draft PR publication. It never merges or
// deploys.
func runUI(ctx context.Context, args []string, defaultLab, version string, out, stderr io.Writer) int {
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
	if !*demo && *lab == "" {
		return fail(fmt.Errorf("no Lab selected; use --lab DIR"))
	}
	s, err := ui.Listen(ui.Options{LabDir: *lab, Version: version, Demo: *demo, Port: *port})
	if err != nil {
		return fail(err)
	}
	if *demo {
		fmt.Fprintln(out, "Forgecell Lab UI · Sample data (synthetic; no Lab is read)")
	} else {
		fmt.Fprintf(out, "Forgecell Lab UI · Lab %s\n", safeText(*lab))
		fmt.Fprintln(out, "Writes only through the engine's human gates: exact scope approval and draft PR publication. It never merges or deploys.")
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
