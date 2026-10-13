package molecule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

const (
	extensionKind = "mcp-provider"

	// maxMCPToolEntry mirrors the bound the adapter enforces on mcp__<name>__<tool>.
	maxMCPToolEntry = 64
)

// AnalysisWithoutSafeModeDisclosure is the plain statement the scope gate and the
// ledger carry when analysis ran with extensions attached.
const AnalysisWithoutSafeModeDisclosure = "Analysis ran without safe mode (--safe-mode omitted, --setting-sources project added) because extensions were attached. User settings, plugins and hooks are excluded, but project-level Claude settings can load during analysis; built-in tools stay disabled."

// ExtensionTool is one tool a role may call and its declared access.
type ExtensionTool struct {
	Name   string `json:"name"`
	Access string `json:"access"`
}

// ExtensionCache is the recorded outcome of preparing the extension's cache. It
// never holds a path: the private server copy and the cache directory stay local.
type ExtensionCache struct {
	Key        string `json:"key"`
	Outcome    string `json:"outcome"`
	TreeSHA256 string `json:"treeSha256,omitempty"`
	Code       int    `json:"code"`
	ElapsedMS  int64  `json:"elapsedMs,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Note       string `json:"note,omitempty"`
}

// ExtensionRecord is the ledger evidence for one declared extension. Roles lists
// only the roles for which a server was actually passed to a harness. Tools is
// what each resolved role may call, so a role that is resolved but not yet
// attached (coding, until approval) shows its tools but no entry in Roles. No MCP
// request or response content is ever recorded.
type ExtensionRecord struct {
	Name           string                     `json:"name"`
	Kind           string                     `json:"kind"`
	ManifestSHA256 string                     `json:"manifestSha256"`
	Required       bool                       `json:"required,omitempty"`
	Network        string                     `json:"network"`
	Roles          []string                   `json:"roles"`
	Tools          map[string][]ExtensionTool `json:"tools,omitempty"`
	Skipped        map[string]string          `json:"skipped,omitempty"`
	Cache          ExtensionCache             `json:"cache"`
}

// Active reports whether the extension is attached to a role or resolved for one.
func (e ExtensionRecord) Active() bool {
	if len(e.Roles) > 0 {
		return true
	}
	for role, tools := range e.Tools {
		if len(tools) > 0 && e.Skipped[role] == "" {
			return true
		}
	}
	return false
}

// AnalysisExtensionsAttached names the extensions a server was passed for during
// analysis.
func (r Record) AnalysisExtensionsAttached() []string {
	var names []string
	for _, e := range r.Extensions {
		if slices.Contains(e.Roles, string(formula.AnalysisHarness)) {
			names = append(names, e.Name)
		}
	}
	return names
}

// Lines renders the extension for the scope gate and the ledger.
func (e ExtensionRecord) Lines() []string {
	attached := "not attached to any role yet"
	if len(e.Roles) > 0 {
		attached = "attached to " + strings.Join(e.Roles, ", ")
	}
	required := ""
	if e.Required {
		required = ", required"
	}
	lines := []string{fmt.Sprintf("Extension %s (%s%s; manifest SHA-256 %s): %s", e.Name, e.Kind, required, e.ManifestSHA256, attached)}
	for _, role := range []string{string(formula.AnalysisHarness), string(formula.CodingHarness)} {
		if tools, ok := e.Tools[role]; ok {
			parts := make([]string, len(tools))
			for i, t := range tools {
				parts[i] = t.Name + " (" + t.Access + ")"
			}
			lines = append(lines, fmt.Sprintf("  %s tools: %s", role, strings.Join(parts, ", ")))
		}
		if reason := e.Skipped[role]; reason != "" {
			lines = append(lines, fmt.Sprintf("  %s not attached: %s", role, reason))
		}
	}
	cache := fmt.Sprintf("  cache: %s", e.Cache.Outcome)
	if e.Cache.Code >= 0 && e.Cache.Outcome != string(extension.CacheReused) {
		cache += fmt.Sprintf(" (prepare exit %d, %d ms, timed out %t)", e.Cache.Code, e.Cache.ElapsedMS, e.Cache.TimedOut)
	}
	if e.Cache.Reason != "" {
		cache += ": " + e.Cache.Reason
	}
	lines = append(lines, cache)
	if e.Cache.Note != "" {
		lines = append(lines, "  cache note: "+e.Cache.Note)
	}
	lines = append(lines, "  network claim (declared by the manifest, not enforced by Forgecell): "+e.Network)
	return lines
}

func renderExtensions(b *strings.Builder, r Record) {
	fmt.Fprintln(b, "\n## Extensions")
	for _, e := range r.Extensions {
		lines := e.Lines()
		fmt.Fprintf(b, "\n- %s\n", lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(b, "  %s\n", strings.TrimLeft(line, " "))
		}
	}
	if len(r.AnalysisExtensionsAttached()) > 0 {
		fmt.Fprintf(b, "\n%s\n", AnalysisWithoutSafeModeDisclosure)
	}
}

// extensionEvidence is the gate-policy evidence naming the active extensions. It
// is absent when no extension was recorded.
func extensionEvidence(r Record) (PolicyEvidence, bool) {
	if len(r.Extensions) == 0 {
		return PolicyEvidence{}, false
	}
	var active, inactive []string
	for _, e := range r.Extensions {
		if e.Active() {
			active = append(active, e.Name)
		} else {
			inactive = append(inactive, e.Name)
		}
	}
	observed := fmt.Sprintf("%d active extensions: %s", len(active), strings.Join(active, ", "))
	if len(inactive) > 0 {
		observed += "; unavailable, not active: " + strings.Join(inactive, ", ")
	}
	return PolicyEvidence{Pointer: "/extensions", Observed: observed}, true
}

// extensionOutcome is what resolving one extension for one role found. A
// non-empty reason means no server is attached for that role.
type extensionOutcome struct {
	network string
	cache   *ExtensionCache
	tools   []ExtensionTool
	server  *harness.MCPServer
	reason  string
	// release gives up the lease on the cache entry the server was built from. It
	// is nil when no entry is held.
	release func()
}

func declaredNetwork(m extension.Manifest) string {
	if m.Prepare == nil {
		return "no prepare step"
	}
	return m.Prepare.Network
}

func cacheRecord(c extension.CacheResult) ExtensionCache {
	return ExtensionCache{Key: c.Key, Outcome: string(c.Outcome), TreeSHA256: c.TreeSHA256, Code: c.Code, ElapsedMS: c.ElapsedMS, TimedOut: c.TimedOut, Reason: c.Reason, Note: c.Note}
}

// serverArgs substitutes {{CACHE}} and refuses any placeholder left over, because
// {{SOURCE}} exists only while the prepare step runs. A suffix after {{CACHE}}/
// must be clean and stay inside the cache.
func serverArgs(args []string, cacheDir string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "{{CACHE}}":
			arg = cacheDir
		case strings.HasPrefix(arg, "{{CACHE}}/"):
			rest := strings.TrimPrefix(arg, "{{CACHE}}/")
			if !extension.CleanPlaceholderSuffix(rest) {
				return nil, fmt.Errorf("server argument %q needs a clean relative path after {{CACHE}}/ that stays inside the cache", arg)
			}
			arg = cacheDir + "/" + rest
		}
		if strings.Contains(arg, "{{") {
			return nil, fmt.Errorf("server argument %q uses a placeholder that cannot be resolved while the server runs ({{SOURCE}} exists only during prepare)", arg)
		}
		out = append(out, arg)
	}
	return out, nil
}

func noToolsReason(m extension.Manifest, role formula.HarnessRole) string {
	if !slices.Contains(m.Roles, string(role)) {
		return fmt.Sprintf("the manifest does not list the %s role", role)
	}
	return fmt.Sprintf("the manifest has no tools the %s role may use (analysis gets read-only tools only)", role)
}

// serverEnvBinary launches every extension server with an explicit, minimal
// environment, so a server never inherits the Claude process environment (model
// credentials, cloud credentials, proxies with credentials).
var serverEnvBinary = "/usr/bin/env"

// isolatedServer wraps the private server command in `env -i` with a fixed
// environment. HOME and TMPDIR point into the private directory.
func isolatedServer(name, privateDir, command string, argv, tools []string) (harness.MCPServer, error) {
	if strings.Contains(command, "=") {
		return harness.MCPServer{}, fmt.Errorf("the private server path %q contains '=', which env would read as a variable assignment; set TMPDIR to a directory without '=' in its path", command)
	}
	tmp := filepath.Join(privateDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return harness.MCPServer{}, err
	}
	args := append([]string{"-i", "PATH=/usr/bin:/bin", "HOME=" + privateDir, "TMPDIR=" + tmp, "LANG=C.UTF-8", command}, argv...)
	return harness.MCPServer{Name: name, Command: serverEnvBinary, Args: args, Tools: tools}, nil
}

// copyServer places verified private copies of the server executable and its
// interpreter under privateDir/<name>, and returns the server launched through
// `env -i` with absolute, clean private paths.
func copyServer(m extension.Manifest, name, privateDir string, args, tools []string) (harness.MCPServer, error) {
	base := filepath.Join(privateDir, name)
	execDir, interpDir := filepath.Join(base, "executable"), filepath.Join(base, "interpreter")
	for _, dir := range []string{execDir, interpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return harness.MCPServer{}, err
		}
	}
	execCopy := filepath.Join(execDir, filepath.Base(m.Server.Executable.Path))
	if err := pinned.Copy(m.Server.Executable, execCopy); err != nil {
		return harness.MCPServer{}, fmt.Errorf("the server executable cannot be trusted: %w", err)
	}
	command, argv := execCopy, args
	if m.Server.Interpreter != nil {
		interpCopy := filepath.Join(interpDir, filepath.Base(m.Server.Interpreter.Path))
		if err := pinned.Copy(*m.Server.Interpreter, interpCopy); err != nil {
			return harness.MCPServer{}, fmt.Errorf("the server interpreter cannot be trusted: %w", err)
		}
		command, argv = interpCopy, append([]string{execCopy}, args...)
	}
	if !filepath.IsAbs(command) || filepath.Clean(command) != command {
		return harness.MCPServer{}, fmt.Errorf("the private server path %q is not a clean absolute path", command)
	}
	return isolatedServer(name, privateDir, command, argv, tools)
}

func resolveExtension(ctx context.Context, r *Record, name string, ref formula.ExtensionRef, role formula.HarnessRole, binding formula.Binding, support harness.Capability, privateDir string) extensionOutcome {
	out := extensionOutcome{network: "unknown"}
	key := extension.CacheKey(name, ref.SHA256, r.Workspace.BaseCommit)
	unavailable := func(reason string) extensionOutcome {
		out.reason = reason
		out.cache = &ExtensionCache{Key: key, Outcome: string(extension.CacheUnavailable), Code: -1, Reason: reason}
		return out
	}
	m, _, err := extension.Load(r.LabDir, ref.Manifest, ref.SHA256)
	if err != nil {
		return unavailable(err.Error())
	}
	out.network = declaredNetwork(m)
	if m.Name != name {
		return unavailable(fmt.Sprintf("the manifest names the extension %q but the Formula declares %q", m.Name, name))
	}
	if support.State != "supported" {
		return unavailable(fmt.Sprintf("the %s role's %s binding cannot attach extensions: %s", role, binding.Binding, support.Reason))
	}
	if info, statErr := os.Stat(serverEnvBinary); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return unavailable(fmt.Sprintf("%s is required to start extension servers with a clean environment and is not an executable file", serverEnvBinary))
	}
	tools := m.ToolsFor(string(role))
	if len(tools) == 0 {
		out.reason = noToolsReason(m, role)
		return out
	}
	names := make([]string, len(tools))
	records := make([]ExtensionTool, len(tools))
	for i, t := range tools {
		if len("mcp__"+name+"__"+t.Name) > maxMCPToolEntry {
			out.reason = fmt.Sprintf("the tool entry mcp__%s__%s is longer than %d characters", name, t.Name, maxMCPToolEntry)
			return out
		}
		names[i], records[i] = t.Name, ExtensionTool{Name: t.Name, Access: t.Access}
	}
	result := extension.PrepareCache(ctx, r.LabDir, name, ref.SHA256, m, r.Workspace.SourceRoot, r.Workspace.BaseCommit)
	cache := cacheRecord(result)
	out.cache = &cache
	if result.Outcome == extension.CacheUnavailable {
		out.reason = "the cache is unavailable: " + result.Reason
		return out
	}
	// The entry stays leased only when a server is built from it; every other
	// path gives the lease up before returning.
	args, err := serverArgs(m.Server.Args, result.Dir)
	if err != nil {
		result.Release()
		out.reason = err.Error()
		return out
	}
	out.tools = records
	if privateDir == "" {
		result.Release()
		return out
	}
	server, err := copyServer(m, name, privateDir, args, names)
	if err != nil {
		result.Release()
		out.reason = err.Error()
		out.tools = nil
		return out
	}
	out.server = &server
	out.release = result.Release
	return out
}

func recordExtension(r *Record, name string, ref formula.ExtensionRef, role formula.HarnessRole, out extensionOutcome) {
	i := slices.IndexFunc(r.Extensions, func(e ExtensionRecord) bool { return e.Name == name })
	existing := i >= 0
	if !existing {
		r.Extensions = append(r.Extensions, ExtensionRecord{Name: name, Kind: extensionKind, Network: out.network, Roles: []string{}})
		i = len(r.Extensions) - 1
	}
	e := &r.Extensions[i]
	e.ManifestSHA256, e.Required = ref.SHA256, ref.Required
	if out.network != "unknown" {
		e.Network = out.network
	}
	switch {
	case out.cache == nil:
		if !existing {
			e.Cache = ExtensionCache{Key: extension.CacheKey(name, ref.SHA256, r.Workspace.BaseCommit), Outcome: string(extension.CacheUnavailable), Code: -1, Reason: "not prepared: " + out.reason}
		}
	case !existing || e.Cache.Outcome == string(extension.CacheUnavailable):
		e.Cache = *out.cache
	case out.cache.Outcome != string(extension.CacheUnavailable):
		sameBuild := e.Cache.Outcome == string(extension.CacheBuilt) && out.cache.Outcome == string(extension.CacheReused) && e.Cache.Key == out.cache.Key && e.Cache.TreeSHA256 == out.cache.TreeSHA256
		if !sameBuild {
			e.Cache = *out.cache
		}
	}
	if out.tools != nil {
		if e.Tools == nil {
			e.Tools = map[string][]ExtensionTool{}
		}
		e.Tools[string(role)] = out.tools
	}
	switch {
	case out.server != nil:
		if !slices.Contains(e.Roles, string(role)) {
			e.Roles = append(e.Roles, string(role))
		}
		delete(e.Skipped, string(role))
	case out.reason != "":
		if e.Skipped == nil {
			e.Skipped = map[string]string{}
		}
		e.Skipped[string(role)] = out.reason
	default:
		delete(e.Skipped, string(role))
	}
}

// useExtensions resolves every extension the Formula binds to the role: it loads
// and verifies the manifest, prepares the cache for the recorded base commit (an
// existing entry is reused), and, when privateDir is set, copies the server into
// it and returns the servers to pass. With an empty privateDir it only resolves,
// so intake can show what coding would receive without attaching anything. An
// extension that is unavailable is recorded and left out; the error is returned
// only when a required extension is unavailable. Any cache lease the servers hold
// is released before it returns, so a caller that keeps the servers running must
// use leaseExtensions.
func useExtensions(ctx context.Context, r *Record, f formula.Formula, role formula.HarnessRole, privateDir string) ([]harness.MCPServer, error) {
	servers, release, err := leaseExtensions(ctx, r, f, role, privateDir)
	release()
	return servers, err
}

// leaseExtensions is useExtensions that keeps the cache entries leased, so that
// Prune leaves them alone, until the returned release is called. The release is
// never nil, and every lease is already given up when an error is returned.
func leaseExtensions(ctx context.Context, r *Record, f formula.Formula, role formula.HarnessRole, privateDir string) ([]harness.MCPServer, func(), error) {
	names := f.RoleExtensions(role)
	if len(names) == 0 {
		return nil, func() {}, nil
	}
	binding, _ := f.RoleBinding(role)
	support := harness.BindingExtensionCapability(binding.Command)
	var servers []harness.MCPServer
	var releases []func()
	release := func() {
		for _, give := range releases {
			give()
		}
		releases = nil
	}
	var required []error
	for _, name := range names {
		ref := f.Extensions[name]
		out := resolveExtension(ctx, r, name, ref, role, binding, support, privateDir)
		recordExtension(r, name, ref, role, out)
		if out.server != nil {
			servers = append(servers, *out.server)
		}
		if out.release != nil {
			releases = append(releases, out.release)
		}
		if out.reason != "" && ref.Required {
			required = append(required, fmt.Errorf("required extension %q is unavailable for the %s role: %s. Repair or re-pin the manifest %s through a reviewed Formula change, or set required to false for %q", name, role, out.reason, ref.Manifest, name))
		}
	}
	if err := errors.Join(required...); err != nil {
		release()
		return servers, func() {}, err
	}
	return servers, release, nil
}

// skipExtensions records every extension the Formula binds to the role as not
// attached, for a reason that applies to the whole role (analysis runs through a
// wrapper or a supplied function that cannot attach servers). Nothing is prepared.
// The error is returned only when a required extension is among them.
func skipExtensions(r *Record, f formula.Formula, role formula.HarnessRole, reason string) error {
	var required []error
	for _, name := range f.RoleExtensions(role) {
		ref := f.Extensions[name]
		out := extensionOutcome{network: "unknown", reason: reason}
		if m, _, err := extension.Load(r.LabDir, ref.Manifest, ref.SHA256); err == nil {
			out.network = declaredNetwork(m)
		}
		recordExtension(r, name, ref, role, out)
		if ref.Required {
			required = append(required, fmt.Errorf("required extension %q cannot be attached to the %s role: %s. Bind the %s role to a native adapter harness, remove %q from the %s role through a reviewed Formula change, or set required to false for %q", name, role, reason, role, name, role, name))
		}
	}
	return errors.Join(required...)
}

// attachExtensions creates a private, absolute directory outside the checkouts
// and the Lab, attaches the role's extensions into it and returns the servers and
// a cleanup that releases the cache leases and removes the directory. The cleanup
// is never nil.
func attachExtensions(ctx context.Context, r *Record, f formula.Formula, role formula.HarnessRole) ([]harness.MCPServer, func(), error) {
	noop := func() {}
	if len(f.RoleExtensions(role)) == 0 {
		return nil, noop, nil
	}
	made, err := os.MkdirTemp("", "forgecell-extension-servers-")
	if err != nil {
		return nil, noop, fmt.Errorf("cannot create a private directory for extension servers: %w", err)
	}
	dir, err := filepath.Abs(made)
	if err != nil {
		_ = os.RemoveAll(made)
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, forbidden := range []string{r.Workspace.Path, r.Workspace.SourceRoot, filepath.Join(r.LabDir, "extensions")} {
		if forbidden == "" {
			continue
		}
		if rel, e := filepath.Rel(forbidden, dir); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cleanup()
			return nil, noop, fmt.Errorf("the private extension directory %s lies inside %s; set TMPDIR to a directory outside the checkouts and the Lab", dir, forbidden)
		}
	}
	servers, release, err := leaseExtensions(ctx, r, f, role, dir)
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	// The cache entries stay leased until the role has finished with its servers.
	return servers, func() {
		release()
		cleanup()
	}, nil
}
