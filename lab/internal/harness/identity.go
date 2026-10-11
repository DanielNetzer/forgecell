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

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)
var protocolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

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
// Every field reads "unknown" unless manifest.json is a small regular file with a
// release version. The manifest is an unsigned local claim, a compatibility hint
// and not a trust anchor.
type Manifest struct {
	Version  string
	Protocol string
	Digest   string
	Found    bool
}

func unknownManifest() Manifest {
	return Manifest{Version: unknownIdentityValue, Protocol: unknownIdentityValue}
}

// ReadManifest reads dir/manifest.json without executing anything.
func ReadManifest(dir string) Manifest {
	m := unknownManifest()
	file := filepath.Join(dir, "manifest.json")
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxManifestBytes {
		return m
	}
	f, err := os.Open(file)
	if err != nil {
		return m
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	var v struct {
		Version         string `json:"version"`
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err != nil || len(raw) > maxManifestBytes || json.Unmarshal(raw, &v) != nil || !releaseVersionPattern.MatchString(v.Version) {
		return m
	}
	sum := sha256.Sum256(raw)
	m.Version, m.Found, m.Digest = v.Version, true, hex.EncodeToString(sum[:])
	if protocolVersionPattern.MatchString(v.ProtocolVersion) {
		m.Protocol = v.ProtocolVersion
	}
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
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxManifestBytes {
		return "", false
	}
	raw, err := os.ReadFile(file)
	if err != nil || !bytes.Contains(raw, []byte(managedLauncherMark)) {
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
func ResolveLauncher(saved string) LauncherIdentity {
	id := LauncherIdentity{Saved: saved, Manifest: unknownManifest()}
	running, runningErr := os.Executable()
	if runningErr == nil {
		id.Running = running
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
		return fmt.Sprintf("Saved launcher %s is a different Forgecell executable than the running %s and its adapter protocol cannot be established (its manifest.json is missing, unreadable or has no protocolVersion); the binding is blocked and the saved launcher was not executed. %s.", other, running, rebind)
	}
	return ""
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
	p := Provenance{ForgecellVersion: RunningVersion(), ExecutableSHA256: unknownIdentityValue, ProtocolVersion: AdapterProtocolVersion}
	running, err := os.Executable()
	if err != nil {
		return p
	}
	p.ExecutableSHA256 = fileSHA256(running)
	if m := ReadManifest(filepath.Dir(running)); m.Found {
		p.ManifestSHA256 = m.Digest
		if p.ForgecellVersion == unknownIdentityValue {
			p.ForgecellVersion = m.Version
		}
	}
	if len(command) == 4 && command[1] == "__adapter" {
		p.SavedLauncher = command[0]
		p.LauncherClass = ResolveLauncher(command[0]).Class
	} else if len(command) > 0 {
		p.LauncherClass = "custom"
	}
	return p
}

func fileSHA256(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return unknownIdentityValue
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxExecutableBytes {
		return unknownIdentityValue
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(f, maxExecutableBytes+1)); err != nil || n > maxExecutableBytes {
		return unknownIdentityValue
	}
	return hex.EncodeToString(h.Sum(nil))
}
