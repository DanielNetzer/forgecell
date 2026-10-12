package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	LauncherMissing      = "missing"
	LauncherRunning      = "running"
	LauncherCompatible   = "compatible"
	LauncherIncompatible = "incompatible"
	LauncherUnknown      = "unknown"
)

const (
	maxManifestBytes     = 4096
	maxExecutableBytes   = 1_000_000_000
	managedLauncherMark  = "# Forgecell managed native launcher v1"
	unknownIdentityValue = "unknown"
)

// ReleaseKind marks a manifest.json written by the Forgecell installer. Releases
// installed before the marker existed omit it, so it is optional; a different kind
// is some other tool's manifest.
const ReleaseKind = "forgecell-release"

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)
var protocolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidProtocolVersion reports whether s can be recorded as an adapter protocol version.
func ValidProtocolVersion(s string) bool { return protocolVersionPattern.MatchString(s) }

// selfExecutablePath names the running inode where the platform exposes it.
var selfExecutablePath = "/proc/self/exe"

var runningVersion atomic.Pointer[string]

// SetRunningVersion records the version the running executable was built as.
func SetRunningVersion(version string) { runningVersion.Store(&version) }

// RunningVersion is the running executable's version, or "unknown".
func RunningVersion() string {
	if v := runningVersion.Load(); v != nil && *v != "" {
		return *v
	}
	return unknownIdentityValue
}

// Manifest is the release manifest installed beside a Forgecell executable.
// Every field reads "unknown" unless manifest.json is a small regular file that
// decodes strictly (declared fields only) with a release version, a kind that is
// absent or the release kind, and a protocol version that is absent or valid. The
// manifest is an unsigned local claim, a compatibility hint and not a trust anchor.
type Manifest struct {
	Version  string
	Protocol string
	Digest   string
	// SHA256 is the executable digest the manifest claims, or "" when it records none.
	SHA256 string
	Found  bool
}

func unknownManifest() Manifest {
	return Manifest{Version: unknownIdentityValue, Protocol: unknownIdentityValue}
}

// manifestFields declares every key the installer writes; any other key makes the
// manifest foreign.
type manifestFields struct {
	Kind            string `json:"kind"`
	Version         string `json:"version"`
	SHA256          string `json:"sha256"`
	ProtocolVersion string `json:"protocolVersion"`
}

// openRegular opens file read-only without blocking and returns it only when the
// open handle itself is a regular file within limit bytes. A path swapped for a
// FIFO after any earlier check cannot hang the caller, and unless follow is set a
// symlink is refused at open.
func openRegular(file string, limit int64, follow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(file, flags, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file within %d bytes", file, limit)
	}
	return f, nil
}

// ReadManifest reads dir/manifest.json without executing anything.
func ReadManifest(dir string) Manifest {
	m := unknownManifest()
	f, err := openRegular(filepath.Join(dir, "manifest.json"), maxManifestBytes, false)
	if err != nil {
		return m
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes {
		return m
	}
	var v manifestFields
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var trailing json.RawMessage
	if dec.Decode(&v) != nil || dec.Decode(&trailing) != io.EOF {
		return m
	}
	if v.Kind != "" && v.Kind != ReleaseKind || !releaseVersionPattern.MatchString(v.Version) {
		return m
	}
	m.Version, m.SHA256 = v.Version, v.SHA256
	if v.ProtocolVersion != "" {
		if !ValidProtocolVersion(v.ProtocolVersion) {
			return unknownManifest()
		}
		m.Protocol = v.ProtocolVersion
	}
	sum := sha256.Sum256(raw)
	m.Digest, m.Found = hex.EncodeToString(sum[:]), true
	return m
}

// LauncherIdentity classifies a saved native launcher against the running
// executable. The saved launcher is inspected and never executed.
type LauncherIdentity struct {
	Class      string
	Managed    bool
	Saved      string
	Path       string
	Executable string
	Manifest   Manifest
	Running    string
}

// managedTarget returns the active release executable a managed wrapper script
// hands over to, found from the installer's fixed layout without running it.
func managedTarget(file string) (string, bool) {
	f, err := openRegular(file, maxManifestBytes, false)
	if err != nil {
		return "", false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes || !bytes.Contains(raw, []byte(managedLauncherMark)) {
		return "", false
	}
	bin := filepath.Dir(file)
	if filepath.Base(bin) != "bin" {
		return "", false
	}
	return filepath.Join(filepath.Dir(bin), "current", "forgecell"), true
}

// ManagedLauncher reports whether bin/forgecell is the installer's managed
// launcher, so new bindings can name it instead of one build.
func ManagedLauncher(bin string) (string, bool) {
	if !filepath.IsAbs(bin) {
		return "", false
	}
	path := filepath.Join(bin, "forgecell")
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if _, ok := managedTarget(real); !ok {
		return "", false
	}
	return path, true
}

// ResolveLauncher resolves the saved launcher at run time: a managed launcher
// resolves to the active release, and any other launcher is read where it lies.
// A launcher that is not an absolute path is unknown: it would resolve against
// the working directory or PATH, so it is neither looked up nor read.
func ResolveLauncher(saved string) LauncherIdentity {
	id := LauncherIdentity{Saved: saved, Manifest: unknownManifest()}
	running, runningErr := os.Executable()
	if runningErr == nil {
		id.Running = running
	}
	if !filepath.IsAbs(saved) {
		id.Class = LauncherUnknown
		return id
	}
	path, err := exec.LookPath(saved)
	if err != nil {
		id.Class = LauncherMissing
		if strings.ContainsRune(saved, filepath.Separator) {
			id.Manifest = ReadManifest(filepath.Dir(saved))
		}
		return id
	}
	id.Path = path
	executable := path
	if real, e := filepath.EvalSymlinks(path); e == nil {
		executable = real
		if active, ok := managedTarget(real); ok {
			id.Managed, id.Executable = true, active
			resolved, e := filepath.EvalSymlinks(active)
			if e != nil {
				id.Class = LauncherMissing
				return id
			}
			executable = resolved
		}
	}
	id.Executable = executable
	id.Manifest = ReadManifest(filepath.Dir(executable))
	if runningErr == nil {
		a, ae := os.Stat(running)
		b, be := os.Stat(executable)
		if ae == nil && be == nil && os.SameFile(a, b) {
			id.Class = LauncherRunning
			return id
		}
	}
	switch id.Manifest.Protocol {
	case unknownIdentityValue:
		id.Class = LauncherUnknown
	case AdapterProtocolVersion:
		id.Class = LauncherCompatible
	default:
		id.Class = LauncherIncompatible
	}
	return id
}

// Runnable reports whether the running executable's in-process adapter may serve
// the saved binding.
func (id LauncherIdentity) Runnable() bool {
	return id.Class == LauncherRunning || id.Class == LauncherCompatible
}

// Describe states the classification with both paths, versions and protocols.
func (id LauncherIdentity) Describe(adapter string) string {
	rebind := fmt.Sprintf("Rebind through the reviewed proposal: forgecell init --lab DIR --harness %s", adapter)
	saved := id.Path
	if saved == "" {
		saved = id.Saved
	}
	other := fmt.Sprintf("%s (version %s, adapter protocol %s)", saved, id.Manifest.Version, id.Manifest.Protocol)
	running := fmt.Sprintf("%s (version %s, adapter protocol %s)", id.Running, RunningVersion(), AdapterProtocolVersion)
	switch id.Class {
	case LauncherMissing:
		if id.Path != "" {
			return fmt.Sprintf("Saved managed Forgecell launcher %s has no active release (%s is missing); running %s. %s.", saved, id.Executable, running, rebind)
		}
		return fmt.Sprintf("Saved Forgecell launcher %s was not found or is not executable; running %s. %s.", saved, running, rebind)
	case LauncherCompatible:
		return fmt.Sprintf("Saved launcher %s is a different Forgecell build than the running %s; the adapter protocol is compatible, so the running executable's adapter serves the binding. The saved launcher was not executed.", other, running)
	case LauncherIncompatible:
		return fmt.Sprintf("Saved launcher %s speaks a different adapter protocol than the running %s; the binding is blocked and the saved launcher was not executed. %s.", other, running, rebind)
	case LauncherUnknown:
		if !filepath.IsAbs(id.Saved) {
			return fmt.Sprintf("Saved launcher %q must be an absolute path to a Forgecell executable; a relative or bare name would resolve against the working directory or PATH, so it was not resolved, read or executed and the binding is blocked; running %s. %s.", id.Saved, running, rebind)
		}
		return fmt.Sprintf("Saved launcher %s is a different Forgecell executable than the running %s and its adapter protocol cannot be established (its manifest.json is missing, unreadable, not a Forgecell release manifest or has no protocolVersion); the binding is blocked and the saved launcher was not executed. %s.", other, running, rebind)
	}
	return ""
}

// ExecutableMatchesManifest reports whether the manifest beside the resolved
// executable records a SHA-256 and that digest is the executable's own. A manifest
// is an unsigned claim; a digest that matches the file it sits beside is what
// makes it more than a stray file.
func (id LauncherIdentity) ExecutableMatchesManifest() bool {
	if !id.Manifest.Found || id.Manifest.SHA256 == "" || id.Executable == "" {
		return false
	}
	got := fileSHA256(id.Executable)
	return got != unknownIdentityValue && got == id.Manifest.SHA256
}

// launcherResolver is how an attempt resolves its saved launcher; tests count calls.
var launcherResolver = ResolveLauncher

// SetLauncherResolver replaces how attempts resolve a saved launcher and returns
// the function that restores the previous resolver. It exists so tests in other
// packages can count the resolutions one attempt makes.
func SetLauncherResolver(resolve func(saved string) LauncherIdentity) (restore func()) {
	previous := launcherResolver
	launcherResolver = resolve
	return func() { launcherResolver = previous }
}

// ResolveAttempt resolves the saved launcher of a native command once, for the
// attempt to pass to RecordProvenanceWith and InvokeBindingWith. Any other command
// has no launcher and yields nil.
func ResolveAttempt(command []string) *LauncherIdentity {
	if len(command) != 4 || command[1] != "__adapter" {
		return nil
	}
	id := launcherResolver(command[0])
	return &id
}

// forSaved is the identity already resolved for this attempt when it describes
// the saved launcher, else a fresh resolution.
func (id *LauncherIdentity) forSaved(saved string) LauncherIdentity {
	if id != nil && id.Saved == saved {
		return *id
	}
	return launcherResolver(saved)
}

// Provenance identifies the Forgecell executable that served one harness attempt.
type Provenance struct {
	ForgecellVersion string `json:"forgecellVersion"`
	ExecutableSHA256 string `json:"executableSha256"`
	ManifestSHA256   string `json:"manifestSha256,omitempty"`
	ProtocolVersion  string `json:"protocolVersion"`
	SavedLauncher    string `json:"savedLauncher,omitempty"`
	LauncherClass    string `json:"launcherClass,omitempty"`
}

// RecordProvenance describes the running executable for a harness attempt. It
// is observation only and never gates approval.
func RecordProvenance(command []string) Provenance {
	return RecordProvenanceWith(command, nil)
}

// RecordProvenanceWith is RecordProvenance for an attempt that already resolved
// the saved launcher: the recorded class is that identity's, so the class the
// attempt was gated on is the class it records. A nil or unrelated identity is
// resolved here.
func RecordProvenanceWith(command []string, id *LauncherIdentity) Provenance {
	p := Provenance{ForgecellVersion: RunningVersion(), ExecutableSHA256: unknownIdentityValue, ProtocolVersion: AdapterProtocolVersion}
	running, err := os.Executable()
	p.ExecutableSHA256 = runningExecutableSHA256(running)
	if err != nil {
		return p
	}
	if m := ReadManifest(filepath.Dir(running)); m.Found {
		p.ManifestSHA256 = m.Digest
		if p.ForgecellVersion == unknownIdentityValue {
			p.ForgecellVersion = m.Version
		}
	}
	if len(command) == 4 && command[1] == "__adapter" {
		p.SavedLauncher = command[0]
		p.LauncherClass = id.forSaved(command[0]).Class
	} else if len(command) > 0 {
		p.LauncherClass = "custom"
	}
	return p
}

// runningExecutableSHA256 hashes the running inode through selfExecutablePath
// where it exists, so a path replaced after start is not misattributed, and the
// os.Executable path elsewhere.
func runningExecutableSHA256(fallback string) string {
	if f, err := openRegular(selfExecutablePath, maxExecutableBytes, true); err == nil {
		defer f.Close()
		return hashFile(f)
	}
	return fileSHA256(fallback)
}

func fileSHA256(file string) string {
	f, err := openRegular(file, maxExecutableBytes, true)
	if err != nil {
		return unknownIdentityValue
	}
	defer f.Close()
	return hashFile(f)
}

func hashFile(f *os.File) string {
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(f, maxExecutableBytes+1)); err != nil || n > maxExecutableBytes {
		return unknownIdentityValue
	}
	return hex.EncodeToString(h.Sum(nil))
}
