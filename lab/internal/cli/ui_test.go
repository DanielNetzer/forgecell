package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets the test read output while Run is still writing it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var uiURL = regexp.MustCompile(`http://127\.0\.0\.1:[0-9]+/\?t=[0-9a-f]{64}`)

// startUI runs forgecell ui with args until the returned cancel is called.
func startUI(t *testing.T, args ...string) (url string, stop func() (int, string)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var out, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, append([]string{"ui"}, args...), strings.NewReader(""), &out, &stderr, "ui-test")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for url = uiURL.FindString(out.String()); url == ""; url = uiURL.FindString(out.String()) {
		select {
		case code := <-done:
			cancel()
			t.Fatalf("ui exited with %d before serving: %s %s", code, out.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("ui printed no URL: %q %q", out.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return url, func() (int, string) {
		cancel()
		select {
		case code := <-done:
			return code, out.String() + stderr.String()
		case <-time.After(10 * time.Second):
			t.Fatal("ui did not stop after cancellation")
			return -1, ""
		}
	}
}

func TestUINoOpenStartsServesAndStops(t *testing.T) {
	opened := false
	previous := openBrowser
	openBrowser = func(string) error { opened = true; return nil }
	t.Cleanup(func() { openBrowser = previous })
	// No real git config is read.
	previousName := gitIdentity
	gitIdentity = func(context.Context, string, ...string) (string, error) { return "Test Operator\n", nil }
	t.Cleanup(func() { gitIdentity = previousName })

	url, stop := startUI(t, "--no-open", "--lab", t.TempDir())
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	base := strings.SplitN(url, "/?", 2)[0]
	res, err = client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Inside your Lab.") {
		t.Fatalf("index: %d", res.StatusCode)
	}
	res, err = http.Get(base + "/api/lab")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("api without the token cookie: %d", res.StatusCode)
	}
	code, output := stop()
	if code != 0 || !strings.Contains(output, "Lab UI stopped.") {
		t.Fatalf("stop: %d %s", code, output)
	}
	if opened {
		t.Fatal("--no-open opened a browser")
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("the UI still answers after stopping")
	}
}

// The browser is opened on a private launch page, never on the token URL, and
// the page is gone once the UI stops.
func TestUIOpensTheBrowserOnALaunchPageAndServesDemo(t *testing.T) {
	var mu sync.Mutex
	var opened, page string
	var mode os.FileMode
	previous := openBrowser
	openBrowser = func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = u
		if info, err := os.Stat(u); err == nil {
			mode = info.Mode().Perm()
		}
		raw, _ := os.ReadFile(u)
		page = string(raw)
		return nil
	}
	t.Cleanup(func() { openBrowser = previous })

	url, stop := startUI(t, "--demo", "--lab", t.TempDir())
	// The URL is printed before the browser opens, so wait for the launch page.
	launched := ""
	for deadline := time.Now().Add(10 * time.Second); launched == ""; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		launched = opened
		mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the browser was never opened")
		}
	}
	// The page goes with the token: the first exchange deletes it, before stop.
	res, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Dir(launched)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launch page %q outlived the first exchange", launched)
		}
	}
	code, output := stop()
	mu.Lock()
	defer mu.Unlock()
	if code != 0 || !strings.Contains(output, "Sample data") || !strings.Contains(output, "The link works once.") {
		t.Fatalf("demo: code %d, output %s", code, output)
	}
	if opened == "" || strings.Contains(opened, "?t=") || !filepath.IsAbs(opened) || mode != 0600 || !strings.Contains(page, url) {
		t.Fatalf("opened %q (mode %v) for %q: %s", opened, mode, url, page)
	}
	if _, err := os.Stat(filepath.Dir(opened)); !os.IsNotExist(err) {
		t.Fatalf("the launch page outlived the UI: %v", err)
	}
}

func TestUILaunchPageIsPrivateAndRemovable(t *testing.T) {
	url := "http://127.0.0.1:1234/?t=" + strings.Repeat("a", 64) + `&x="<b>`
	path, remove, err := launchPage(url)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("launch page %v %v", info, err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("launch directory %v %v", dir, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `content="0;url=http://127.0.0.1:1234/?t=`+strings.Repeat("a", 64)+`&amp;x=&#34;&lt;b&gt;"`) || strings.Contains(string(raw), "<b>") || strings.Contains(string(raw), "<script") {
		t.Fatalf("launch page %s", raw)
	}
	remove()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("remove left %s: %v", path, err)
	}
}

// ui --demo reads no Lab, so it never resolves the default one.
func TestUIDemoNeverResolvesTheDefaultLab(t *testing.T) {
	previous := resolveLab
	resolveLab = func(context.Context, string) (labResolution, error) {
		t.Error("ui --demo resolved the default Lab")
		return labResolution{}, fmt.Errorf("not resolvable here")
	}
	t.Cleanup(func() { resolveLab = previous })
	previousName := gitIdentity
	gitIdentity = func(context.Context, string, ...string) (string, error) {
		t.Error("ui --demo read git config")
		return "", fmt.Errorf("not here")
	}
	t.Cleanup(func() { gitIdentity = previousName })
	_, stop := startUI(t, "--demo", "--no-open")
	if code, output := stop(); code != 0 || !strings.Contains(output, "no Lab is read") {
		t.Fatalf("demo: %d %s", code, output)
	}
	for args, want := range map[string]bool{"--demo": true, "-demo": true, "--demo=true": true, "--demo=false": false, "--demo --demo=0": false, "--demo=0 --demo": true, "-- --demo": false, "--no-open": false} {
		if got := demoRequested(strings.Fields(args)); got != want {
			t.Errorf("demoRequested(%q) = %t", args, got)
		}
	}
}

// The avatar name is read through the gitIdentity seam, in the source checkout:
// git config user.name first; when that is unset, empty or oversized, the name
// part of git var GIT_AUTHOR_IDENT, never its email. A failure, an empty value
// or an oversized one gives "" so the UI shows its fallback, and nothing is
// printed.
func TestUIOperatorNameReadsGitConfigThroughTheSeam(t *testing.T) {
	previous := gitIdentity
	t.Cleanup(func() { gitIdentity = previous })
	unset := fmt.Errorf("exit status 1 in /Users/someone")
	type read struct {
		out string
		err error
	}
	for name, c := range map[string]struct {
		config, ident read
		want          string
	}{
		"configured":          {config: read{out: "  Ada Lovelace\n"}, ident: read{out: "Other <other@example.invalid> 1700000000 +0000\n"}, want: "Ada Lovelace"},
		"empty":               {config: read{out: ""}, ident: read{err: unset}, want: ""},
		"blank":               {config: read{out: " \n"}, ident: read{out: " \n"}, want: ""},
		"error":               {config: read{out: "Ada", err: unset}, ident: read{err: unset}, want: ""},
		"oversized":           {config: read{out: strings.Repeat("a", maxGitUserNameBytes+1)}, ident: read{err: unset}, want: ""},
		"at bound":            {config: read{out: strings.Repeat("a", maxGitUserNameBytes)}, want: strings.Repeat("a", maxGitUserNameBytes)},
		"ident when unset":    {config: read{err: unset}, ident: read{out: "Ada Lovelace <ada@example.invalid> 1700000000 +0000\n"}, want: "Ada Lovelace"},
		"ident when empty":    {config: read{out: "\n"}, ident: read{out: "Ada Lovelace <ada@example.invalid> 1700000000 +0000\n"}, want: "Ada Lovelace"},
		"ident oversized":     {config: read{err: unset}, ident: read{out: strings.Repeat("a", maxGitUserNameBytes) + " <a@example.invalid>"}, want: ""},
		"ident no name":       {config: read{err: unset}, ident: read{out: " <ada@example.invalid> 1700000000 +0000\n"}, want: ""},
		"ident email only":    {config: read{err: unset}, ident: read{out: "ada@example.invalid 1700000000 +0000\n"}, want: ""},
		"ident email in name": {config: read{err: unset}, ident: read{out: "ada@example.invalid <ada@example.invalid> 1700000000 +0000\n"}, want: ""},
		"ident error":         {config: read{err: unset}, ident: read{out: "Ada <ada@example.invalid>", err: unset}, want: ""},
	} {
		var calls [][]string
		gitIdentity = func(_ context.Context, d string, args ...string) (string, error) {
			if d != "/src/checkout" {
				t.Errorf("%s: read in %q", name, d)
			}
			calls = append(calls, args)
			if strings.Join(args, " ") == "config --get user.name" {
				return c.config.out, c.config.err
			}
			if strings.Join(args, " ") == "var GIT_AUTHOR_IDENT" {
				return c.ident.out, c.ident.err
			}
			t.Errorf("%s: unexpected git %q", name, args)
			return "", unset
		}
		got := operatorName(context.Background(), "/src/checkout")
		if got != c.want || strings.Contains(got, "@") {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if configured := c.want != "" && c.config.err == nil && strings.TrimSpace(c.config.out) == c.want; configured && len(calls) != 1 {
			t.Errorf("%s: read the ident although user.name was set: %q", name, calls)
		}
	}
}

// With a real git whose config has no user.name, the avatar name comes from the
// author identity git would commit with, and its email never appears. HOME and
// the global and system configs point into the test's own temp dir.
func TestUIOperatorNameFallsBackToTheAuthorIdent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("GIT_AUTHOR_NAME", "Ada Lovelace")
	t.Setenv("GIT_AUTHOR_EMAIL", "ada@example.invalid")
	if _, err := gitIdentity(context.Background(), dir, gitUserNameArgs...); err == nil {
		t.Fatal("the test config has a user.name")
	}
	if got := operatorName(context.Background(), dir); got != "Ada Lovelace" || strings.Contains(got, "example.invalid") {
		t.Fatalf("operator %q", got)
	}
}

// The git output buffer keeps at most its limit, drains the rest and records
// the overflow, so an oversized name is refused rather than cut.
func TestUIOperatorBoundedBufferRefusesOverflow(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	for _, chunk := range []string{"abc", "defgh", "ijk"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %q: %d %v", chunk, n, err)
		}
	}
	if !b.overflow || b.b.String() != "abcdefgh" {
		t.Fatalf("buffer %q overflow %t", b.b.String(), b.overflow)
	}
	fits := &boundedBuffer{limit: 8}
	fits.Write([]byte("abcdefgh"))
	if fits.overflow || fits.b.String() != "abcdefgh" {
		t.Fatalf("a value at the bound overflowed: %q", fits.b.String())
	}
}

func TestUIRejectsBadArguments(t *testing.T) {
	previousName := gitIdentity
	gitIdentity = func(context.Context, string, ...string) (string, error) { return "", fmt.Errorf("not read here") }
	t.Cleanup(func() { gitIdentity = previousName })
	for _, args := range [][]string{{"ui", "--lab", t.TempDir(), "extra"}, {"ui", "--lab", t.TempDir(), "--port", "70000"}} {
		var out, stderr bytes.Buffer
		if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "ui-test"); code == 0 {
			t.Errorf("%v was accepted: %s", args, out.String())
		}
	}
}
