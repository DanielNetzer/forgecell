package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

const (
	// runMarker marks a per-start directory as Forgecell's own; only a marked
	// directory is ever removed.
	runMarker = ".forgecell-plugin-run"
	runPrefix = "forgecell-plugin-"
	// maxStderrBytes bounds the stderr kept for diagnostics; stderrLookahead more
	// is kept so a secret straddling the bound is still found and replaced whole.
	maxStderrBytes  = 64 << 10
	stderrLookahead = 4 << 10
	defaultPath     = "/usr/bin:/bin"
	defaultLang     = "C"
	minSecretLength = 4
)

// Options configure one plugin start.
type Options struct {
	// Root is an existing directory under which each start creates a private,
	// marked directory holding the plugin's HOME and its verified executable.
	Root string
	// LookupEnv reads the caller's environment; nil means os.LookupEnv. Only
	// PATH, LANG and the manifest's needs.env names are read.
	LookupEnv func(string) (string, bool)
	// MaxMessageBytes lowers the MCP message bound; zero keeps the default.
	MaxMessageBytes int
}

// Process is one running plugin with its MCP session.
type Process struct {
	manifest Manifest
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	stdin    *os.File
	stdout   *os.File
	stderrR  *os.File
	client   *mcp.Client
	dir      string
	secrets  []string
	stderr   *capture
	done     chan struct{}
	once     sync.Once
}

// Start verifies every pinned file, copies the verified server executable into
// a fresh private directory and starts it there in its own process group, with
// an environment of exactly PATH, HOME (a fresh directory), LANG and the set
// needs.env names. Canceling ctx kills the process group.
func Start(ctx context.Context, m Manifest, opts Options) (*Process, error) {
	if err := supported(); err != nil {
		return nil, err
	}
	if _, err := m.Verify(); err != nil {
		return nil, err
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	dir, err := makeRunDir(opts.Root)
	if err != nil {
		return nil, err
	}
	p, err := start(ctx, m, opts, dir, lookup)
	if err != nil {
		removeRunDir(dir)
		return nil, err
	}
	return p, nil
}

func start(ctx context.Context, m Manifest, opts Options, dir string, lookup func(string) (string, bool)) (*Process, error) {
	home, bin := filepath.Join(dir, "home"), filepath.Join(dir, "bin")
	for _, d := range []string{home, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, mcp.Fail(mcp.Failed, "", "the plugin directory cannot be prepared")
		}
	}
	server := filepath.Join(bin, "server")
	if err := pinned.Copy(m.Server.Executable, server); err != nil {
		if strings.Contains(err.Error(), "changed") {
			return nil, mcp.Fail(mcp.ManifestChanged, "", "the server changed while it was copied")
		}
		return nil, mcp.Fail(mcp.NotInstalled, "", "the server cannot be copied")
	}
	argv := []string{server}
	if m.Server.Interpreter != nil {
		argv = []string{m.Server.Interpreter.Path, server}
	}
	argv = append(argv, m.Server.Args...)
	env, secrets := environment(m.Needs.Env, home, lookup)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, mcp.Fail(mcp.Failed, "", "pipes cannot be created")
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		return nil, mcp.Fail(mcp.Failed, "", "pipes cannot be created")
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return nil, mcp.Fail(mcp.Failed, "", "pipes cannot be created")
	}
	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = home, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	configure(cmd)
	err = cmd.Start()
	closeAll(stdinR, stdoutW, stderrW)
	if err != nil {
		cancel()
		closeAll(stdinW, stdoutR, stderrR)
		return nil, mcp.Fail(mcp.NotInstalled, "", "the server cannot be started")
	}
	p := &Process{
		manifest: m, cmd: cmd, cancel: cancel, stdin: stdinW, stdout: stdoutR, stderrR: stderrR,
		client: mcp.NewClient(stdoutR, stdinW, opts.MaxMessageBytes),
		dir:    dir, secrets: secrets, stderr: &capture{done: make(chan struct{})}, done: make(chan struct{}),
	}
	go p.stderr.run(stderrR)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// environment builds exactly PATH, HOME, LANG and the needs.env names that are
// set. It returns the needs.env values too, so diagnostics can redact them.
func environment(needs []string, home string, lookup func(string) (string, bool)) ([]string, []string) {
	path, ok := lookup("PATH")
	if !ok || path == "" {
		path = defaultPath
	}
	lang, ok := lookup("LANG")
	if !ok || lang == "" {
		lang = defaultLang
	}
	env := []string{"PATH=" + path, "HOME=" + home, "LANG=" + lang}
	var secrets []string
	for _, name := range needs {
		if value, ok := lookup(name); ok {
			env = append(env, name+"="+value)
			if len(value) >= minSecretLength {
				secrets = append(secrets, value)
			}
		}
	}
	return env, secrets
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// Initialize runs the MCP handshake and requires the server to declare the
// manifest's contract.
func (p *Process) Initialize(ctx context.Context) (mcp.ServerInfo, error) {
	info, err := p.client.Initialize(ctx)
	if err != nil {
		return mcp.ServerInfo{}, err
	}
	if info.Contract != p.manifest.Contract {
		return info, mcp.Fail(mcp.ContractMismatch, "initialize", "the server declares a different contract")
	}
	return info, nil
}

// ListTools returns the server's tools/list.
func (p *Process) ListTools(ctx context.Context) ([]mcp.ToolInfo, error) {
	return p.client.ListTools(ctx)
}

// Call calls a contract tool within its message bound and the given timeout
// (the contract default when zero) and decodes the output strictly.
func (p *Process) Call(ctx context.Context, t *contract.Tool, args any, timeout time.Duration) (any, error) {
	if timeout <= 0 {
		timeout = t.Timeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := p.client.CallTool(ctx, t.Name, args, t.MessageBytes())
	if err != nil {
		return nil, err
	}
	return t.Decode(raw)
}

// CallRaw calls a tool with arbitrary arguments and returns its structured
// output undecoded, for conformance checks that send malformed input.
func (p *Process) CallRaw(ctx context.Context, name string, args any, maxBytes int, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return p.client.CallTool(ctx, name, args, maxBytes)
}

// Exited reports whether the plugin process has exited.
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Shutdown closes the plugin's stdin and waits for it to exit. If grace fires
// first, the whole process group is killed. Afterwards it removes the marked
// per-start directory. A nil grace waits for the plugin to exit on its own.
func (p *Process) Shutdown(grace <-chan time.Time) error {
	var err error
	p.once.Do(func() {
		// Closing stdout too means a server blocked writing a response nobody
		// will read gets EPIPE instead of hanging until grace.
		p.stdin.Close()
		p.stdout.Close()
		killed := false
		select {
		case <-p.done:
		case <-grace:
			p.cancel()
			killed = true
			<-p.done
		}
		if !killed {
			select {
			case <-p.stderr.done:
			case <-grace:
			}
		}
		p.stderrR.Close()
		<-p.stderr.done
		p.cancel()
		err = removeRunDir(p.dir)
	})
	return err
}

// Diagnostics returns the plugin's stderr for the ledger: bounded, with the
// per-start directory replaced, secrets and declared credential values
// redacted, and control characters other than newlines and tabs replaced. It is
// complete after Shutdown and is never display text.
func (p *Process) Diagnostics() string {
	raw, dropped := p.stderr.snapshot()
	s := strings.ReplaceAll(string(raw), p.dir, "<plugin-dir>")
	for _, secret := range p.secrets {
		s = strings.ReplaceAll(s, secret, safetext.SecretMark)
	}
	truncated := dropped > 0 || len(s) > maxStderrBytes
	s = safetext.Multiline(safetext.CutRedacted(s, maxStderrBytes), maxStderrBytes)
	if truncated {
		s += fmt.Sprintf("\n[stderr truncated after %d bytes]", maxStderrBytes)
	}
	return s
}

// capture keeps the first maxStderrBytes+stderrLookahead bytes of a stream and
// counts the rest.
type capture struct {
	mu      sync.Mutex
	buf     []byte
	dropped int64
	done    chan struct{}
}

func (c *capture) run(r io.Reader) {
	defer close(c.done)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		c.mu.Lock()
		keep := min(n, maxStderrBytes+stderrLookahead-len(c.buf))
		c.buf = append(c.buf, chunk[:keep]...)
		c.dropped += int64(n - keep)
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *capture) snapshot() ([]byte, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...), c.dropped
}

// makeRunDir creates a fresh 0700 directory under root, atomically and with a
// unique name, and marks it as Forgecell's.
func makeRunDir(root string) (string, error) {
	fail := func() (string, error) {
		return "", mcp.Fail(mcp.Failed, "", "the plugin root directory is unusable")
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fail()
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fail()
	}
	if info, err := os.Lstat(resolved); err != nil || !info.IsDir() {
		return fail()
	}
	dir, err := os.MkdirTemp(resolved, runPrefix)
	if err != nil {
		return fail()
	}
	marker, err := os.OpenFile(filepath.Join(dir, runMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err == nil {
		_, err = marker.WriteString("forgecell plugin run\n")
		if closeErr := marker.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		os.Remove(filepath.Join(dir, runMarker))
		os.Remove(dir)
		return fail()
	}
	return dir, nil
}

// removeRunDir removes a per-start directory only when it is a real directory
// (not a symlink) owned by the current user, named like one, and still holding
// its marker as a regular file.
func removeRunDir(dir string) error {
	refuse := errors.New("the plugin directory is not a marked Forgecell directory; it was left in place")
	if !strings.HasPrefix(filepath.Base(dir), runPrefix) {
		return refuse
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByMe(info) {
		return refuse
	}
	marker, err := os.Lstat(filepath.Join(dir, runMarker))
	if err != nil || !marker.Mode().IsRegular() || !ownedByMe(marker) {
		return refuse
	}
	return os.RemoveAll(dir)
}
