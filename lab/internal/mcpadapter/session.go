// Package mcpadapter serves the keyed tracker and change-request forge
// interfaces from a plugin's MCP server. Every engine operation runs one
// pinned, scrubbed plugin process: load and verify the manifest, start the
// server under a Lab-owned run root, initialize with the contract id, check
// tools/list against the contract, make the call or calls, and shut the process
// down on every path. Every call validates its input against the contract
// before sending and decodes its output strictly, and each one yields a
// CallRecord that holds digests, never content.
package mcpadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

const (
	defaultGrace     = 2 * time.Second
	defaultHandshake = 30 * time.Second
	maxHandshake     = 2 * time.Minute
)

// Steps named in errors before or around the tool calls.
const (
	stepOptions  = "options"
	stepLoad     = "load"
	stepStart    = "start"
	stepShutdown = "shutdown"
)

// Ref is a pinned plugin manifest: a Lab-relative path and the SHA-256 of its
// bytes, as the Formula's plugins block names it.
type Ref struct {
	Manifest string
	SHA256   string
}

// Options configure the plugin process of every operation.
type Options struct {
	// LabDir is the Lab directory, a clean absolute path; Ref.Manifest is
	// relative to it.
	LabDir string
	Ref    Ref
	// RunRoot is the Forgecell-owned directory under LabDir in which each start
	// makes its private, marked run directory. The caller creates it with mode
	// 0700; the adapter refuses one that is a symlink, not owned by the current
	// user, open to others or outside LabDir, and never uses the user's own
	// temporary directory or home.
	RunRoot string
	// LookupEnv reads the environment for PATH, LANG and the manifest's
	// needs.env names; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Observer, when set, receives the record of every tool call.
	Observer func(CallRecord)
	// Timeout, when positive, lowers every tool's contract timeout to it; it
	// never raises one.
	Timeout time.Duration
	// Handshake bounds initialize and tools/list each; zero means 30 s, and it
	// is capped at 2 minutes.
	Handshake time.Duration
	// Grace returns the channel that ends the shutdown grace period, after
	// which the plugin's process group is killed; nil means 2 s.
	Grace func() <-chan time.Time
}

// CallRecord is the ledger record of one tool call: who was called with what
// digest of arguments and result, how long it took and the fixed reason it
// failed, if it did, including a refusal of a decoded answer. It never holds
// request or response content. ResultSHA256 is empty when no result arrived;
// DurationMS is 0 for a call refused before it was sent. Cleanup is set on the
// last call of an operation whose plugin could not be cleaned up afterwards,
// and CleanupDiagnostics then holds the adapter's own fixed account of it.
// Records reach the Observer after the plugin was shut down.
type CallRecord struct {
	Plugin             string     `json:"plugin"`
	Kind               string     `json:"kind"`
	ManifestSHA256     string     `json:"manifestSha256"`
	Tool               string     `json:"tool"`
	ArgsSHA256         string     `json:"argsSha256"`
	ResultSHA256       string     `json:"resultSha256,omitempty"`
	DurationMS         int64      `json:"durationMs"`
	Reason             mcp.Reason `json:"reason,omitempty"`
	Cleanup            mcp.Reason `json:"cleanup,omitempty"`
	CleanupDiagnostics string     `json:"cleanupDiagnostics,omitempty"`
}

// cleanupDiagnostics is the fixed account of a failed cleanup; the removal
// error itself names local paths, so it is never kept.
const cleanupDiagnostics = "the plugin's run directory could not be removed after shutdown"

// Error is a failed operation. Error() is display-safe: the plugin and tool or
// step names and one fixed mcp reason, never server text, stderr or a local
// path. Diagnostics holds the bounded, redacted detail for the ledger and is
// never display text. mcp.ReasonOf reports Reason.
type Error struct {
	Plugin string
	// Tool is the contract tool, MCP method or step that failed.
	Tool   string
	Reason mcp.Reason
	// Remote is true when the server answered with a well-formed MCP error.
	Remote      bool
	Diagnostics string
	// cleanupOnly is true when the operation succeeded and only the plugin's
	// cleanup failed.
	cleanupOnly bool
}

// cleanupOnly reports whether err is only a cleanup failure after a
// successful operation.
func cleanupOnly(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.cleanupOnly
}

func (e *Error) Error() string {
	s := "plugin"
	if e.Plugin != "" {
		s += " " + e.Plugin
	}
	if e.Tool != "" {
		s += " " + e.Tool
	}
	return s + ": " + string(e.Reason)
}

// Unwrap exposes the fixed reason as an *mcp.Error, without diagnostics.
func (e *Error) Unwrap() error {
	return &mcp.Error{Reason: e.Reason, Tool: e.Tool, Remote: e.Remote}
}

// session is one running plugin within one operation.
type session struct {
	opts     Options
	manifest plugin.Manifest
	contract *contract.Contract
	process  *plugin.Process
	// records are the operation's call records, held until shutdown so a
	// refusal of a decoded answer and a cleanup failure reach them; an
	// operation makes at most a few calls.
	records []CallRecord
}

// fail builds an Error from any error: an *mcp.Error keeps its reason, remote
// flag and diagnostics; any other error is Failed with no diagnostics, since
// its text is not known to be display- or ledger-safe.
func fail(pluginName, tool string, err error) *Error {
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	e := &Error{Plugin: pluginName, Tool: tool, Reason: mcp.ReasonOf(err)}
	var m *mcp.Error
	if errors.As(err, &m) {
		e.Remote, e.Diagnostics = m.Remote, m.Diagnostics
	}
	return e
}

func refuse(pluginName, tool string, reason mcp.Reason, diagnostics string) *Error {
	return &Error{Plugin: pluginName, Tool: tool, Reason: reason, Diagnostics: mcp.Diagnose(diagnostics)}
}

// run runs op on a fresh plugin process of kind. Shutdown runs on every path
// once the process started, including a panic in op or in the Observer, which
// is re-raised afterwards. A failure gets the plugin's redacted stderr as
// diagnostics. If op succeeded but the run directory could not be removed, run
// returns an Error for the shutdown step that cleanupOnly reports, and marks
// the last call record; read callers return their output with it. The records
// reach the Observer after Shutdown.
func run(ctx context.Context, opts Options, kind string, op func(*session) error) (err error) {
	if err := opts.check(); err != nil {
		return err
	}
	m, _, err := plugin.Load(opts.LabDir, opts.Ref.Manifest, opts.Ref.SHA256)
	if err != nil {
		return fail("", stepLoad, err)
	}
	c, ok := contract.Lookup(kind)
	if !ok || m.Kind != kind || m.Contract != c.ID {
		return refuse(m.Name, stepLoad, mcp.ContractMismatch, "the plugin is not a "+kind+" plugin")
	}
	p, err := plugin.Start(ctx, m, plugin.Options{Root: opts.RunRoot, LookupEnv: opts.LookupEnv})
	if err != nil {
		return fail(m.Name, stepStart, err)
	}
	s := &session{opts: opts, manifest: m, contract: c, process: p}
	defer func() {
		cleanup := p.Shutdown(s.grace())
		if cleanup != nil && len(s.records) > 0 {
			last := &s.records[len(s.records)-1]
			last.Cleanup, last.CleanupDiagnostics = mcp.Failed, cleanupDiagnostics
		}
		var e *Error
		switch {
		case errors.As(err, &e):
			e.Diagnostics = joinDiagnostics(e.Diagnostics, p.Diagnostics())
		case err != nil:
			err = &Error{Plugin: m.Name, Reason: mcp.ReasonOf(err), Diagnostics: p.Diagnostics()}
		case cleanup != nil:
			err = &Error{Plugin: m.Name, Tool: stepShutdown, Reason: mcp.Failed, Diagnostics: cleanupDiagnostics, cleanupOnly: true}
		}
		if opts.Observer != nil {
			for _, rec := range s.records {
				opts.Observer(rec)
			}
		}
	}()
	if err := s.handshake(ctx); err != nil {
		return err
	}
	return op(s)
}

// joinDiagnostics keeps both the failure's own detail and the plugin's stderr;
// each is already bounded and redacted.
func joinDiagnostics(own, stderr string) string {
	switch {
	case own == "":
		return stderr
	case stderr == "":
		return own
	}
	return own + "\n" + stderr
}

func (s *session) grace() <-chan time.Time {
	if s.opts.Grace != nil {
		return s.opts.Grace()
	}
	return time.After(defaultGrace)
}

func (s *session) handshakeTimeout() time.Duration {
	if s.opts.Handshake <= 0 {
		return defaultHandshake
	}
	return min(s.opts.Handshake, maxHandshake)
}

// handshake initializes the session, which requires the manifest's contract id,
// and requires tools/list to advertise every required contract tool with an
// input schema compatible with the contract's. Nothing is called before it
// passes.
func (s *session) handshake(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, s.handshakeTimeout())
	_, err := s.process.Initialize(hctx)
	cancel()
	if err != nil {
		return fail(s.manifest.Name, "initialize", err)
	}
	hctx, cancel = context.WithTimeout(ctx, s.handshakeTimeout())
	tools, err := s.process.ListTools(hctx)
	cancel()
	if err != nil {
		return fail(s.manifest.Name, "tools/list", err)
	}
	listed := map[string]mcp.ToolInfo{}
	for _, t := range tools {
		listed[t.Name] = t
	}
	for i := range s.contract.Tools {
		t := &s.contract.Tools[i]
		info, ok := listed[t.Name]
		if !ok {
			if t.Required {
				return refuse(s.manifest.Name, t.Name, mcp.ContractMismatch, "tools/list does not list a required contract tool")
			}
			continue
		}
		if err := t.CheckAdvertised(info.InputSchema); err != nil {
			return refuse(s.manifest.Name, t.Name, mcp.ContractMismatch, "the advertised input schema is not compatible with the contract")
		}
	}
	return nil
}

// timeout is the tool's contract timeout, lowered to Options.Timeout when that
// is set.
func (s *session) timeout(t *contract.Tool) time.Duration {
	if s.opts.Timeout > 0 && s.opts.Timeout < t.Timeout() {
		return s.opts.Timeout
	}
	return t.Timeout()
}

// call validates args against the tool's input schema, sends exactly the bytes
// it validated within the tool's message bound and timeout, and decodes the
// structured output strictly. A refused input is never sent.
func (s *session) call(ctx context.Context, name string, args any) (any, error) {
	t, ok := s.contract.Tool(name)
	if !ok {
		return nil, refuse(s.manifest.Name, name, mcp.ContractMismatch, "the tool is not part of the contract")
	}
	rec := CallRecord{Plugin: s.manifest.Name, Kind: s.manifest.Kind, ManifestSHA256: s.opts.Ref.SHA256, Tool: name}
	raw, err := json.Marshal(args)
	if err == nil {
		rec.ArgsSHA256 = digest(raw)
		if t.ValidateInputJSON(raw) != nil {
			err = errors.New("refused")
		}
	}
	if err != nil {
		// The arguments may hold Issue or change-request text, so the detail
		// names no value.
		rec.Reason = mcp.SchemaInvalid
		s.observe(rec)
		return nil, refuse(s.manifest.Name, name, mcp.SchemaInvalid, "the arguments do not match the contract input schema; nothing was sent")
	}
	start := time.Now()
	out, err := s.process.CallRaw(ctx, name, json.RawMessage(raw), t.MessageBytes(), s.timeout(t))
	var value any
	if err == nil {
		rec.ResultSHA256 = digest(out)
		value, err = t.Decode(out)
	}
	rec.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Reason = mcp.ReasonOf(err)
	}
	s.observe(rec)
	if err != nil {
		return nil, fail(s.manifest.Name, name, err)
	}
	return value, nil
}

func (s *session) observe(rec CallRecord) {
	s.records = append(s.records, rec)
}

// refuseAnswer refuses the decoded answer of the session's last call, which
// must be tool: its record carries reason, as a failed call's would.
func (s *session) refuseAnswer(tool string, reason mcp.Reason, diagnostics string) *Error {
	if n := len(s.records); n > 0 && s.records[n-1].Tool == tool && s.records[n-1].Reason == "" {
		s.records[n-1].Reason = reason
	}
	return refuse(s.manifest.Name, tool, reason, diagnostics)
}

// callAs calls a tool and returns its decoded output as T.
func callAs[T any](ctx context.Context, s *session, name string, args any) (T, error) {
	var zero T
	v, err := s.call(ctx, name, args)
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, s.refuseAnswer(name, mcp.SchemaInvalid, "the decoded output has an unexpected type")
	}
	return out, nil
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// check refuses options whose run root is not a Forgecell-owned directory
// under the Lab: it must be a clean absolute path inside LabDir, reached
// without symlinks, a real directory owned by the current user with no access
// for group or others. The errors name no path.
func (o Options) check() error {
	bad := func(why string) error { return refuse("", stepOptions, mcp.Failed, why) }
	if o.LabDir == "" || !filepath.IsAbs(o.LabDir) || filepath.Clean(o.LabDir) != o.LabDir {
		return bad("the Lab directory must be a clean absolute path")
	}
	if o.RunRoot == "" || !filepath.IsAbs(o.RunRoot) || filepath.Clean(o.RunRoot) != o.RunRoot {
		return bad("the run root must be a clean absolute path")
	}
	rel, err := filepath.Rel(o.LabDir, o.RunRoot)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return bad("the run root must be inside the Lab directory")
	}
	at := o.LabDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return bad("the run root must be a real directory reached without symlinks")
		}
	}
	info, err := os.Lstat(o.RunRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByMe(info) {
		return bad("the run root must be a 0700 directory owned by the current user")
	}
	return nil
}

// ownedByMe reports whether the file's owner is the effective user. It reads
// the Uid of the platform's stat record, so it is false where there is none;
// plugins run only on Linux and macOS.
func ownedByMe(info os.FileInfo) bool {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return false
	}
	uid := v.Elem().FieldByName("Uid")
	euid := os.Geteuid()
	return uid.IsValid() && uid.CanUint() && euid >= 0 && uid.Uint() == uint64(euid)
}
