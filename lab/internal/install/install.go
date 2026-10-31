// Package install activates verified native CLI releases without touching project Lab files.
package install

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

//go:embed notices.txt
var notices []byte
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)

// Protocol is the adapter protocol the release speaks; empty records the running binary's.
type Options struct{ Home, Bin, Source, Version, Protocol string }
type Result struct {
	Version string `json:"version"`
	Command string `json:"command"`
	Home    string `json:"home"`
}
type manifest struct {
	Kind            string `json:"kind,omitempty"`
	Version         string `json:"version"`
	SHA256          string `json:"sha256"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
}

func sum(raw []byte) string                { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func quote(s string) string                { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func normalize(o Options) (Options, error) { return normalizeDirs(o, true) }
func normalizeDirs(o Options, withBin bool) (Options, error) {
	user, err := os.UserHomeDir()
	if err != nil {
		return o, err
	}
	if o.Home == "" {
		o.Home = filepath.Join(user, ".forgecell")
	}
	if o.Bin == "" {
		o.Bin = filepath.Join(user, ".local", "bin")
	}
	o.Home, err = filepath.Abs(o.Home)
	if err != nil {
		return o, err
	}
	dirs := []string{o.Home}
	if withBin {
		o.Bin, err = filepath.Abs(o.Bin)
		if err != nil {
			return o, err
		}
		dirs = append(dirs, o.Bin)
	}
	for _, dir := range dirs {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return o, err
		}
	}
	o.Home, err = filepath.EvalSymlinks(o.Home)
	if err != nil || !withBin {
		return o, err
	}
	o.Bin, err = filepath.EvalSymlinks(o.Bin)
	return o, err
}
func point(link, target string) error {
	f, err := os.CreateTemp(filepath.Dir(link), ".link-")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	if err = os.Symlink(target, name); err != nil {
		return err
	}
	return os.Rename(name, link)
}
func target(home, name string) (string, error) {
	link := filepath.Join(home, name)
	st, err := os.Lstat(link)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("refusing unmanaged path: %s", link)
	}
	value, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(home, value)
	}
	value = filepath.Clean(value)
	if filepath.Dir(value) != filepath.Join(home, "releases") || !versionPattern.MatchString(filepath.Base(value)) {
		return "", fmt.Errorf("unmanaged release link: %s", link)
	}
	return value, nil
}

// ProbeTimeout bounds each run of a release executable to ask its version or
// protocol. Production never changes it; tests widen it so a loaded machine
// cannot fail a fixture that answers at once.
var ProbeTimeout = 15 * time.Second

// ProbeWaitDelay bounds how long a probe's pipes may take to drain after the
// release executable exits. Production never changes it; tests widen it for the
// same reason as ProbeTimeout.
var ProbeWaitDelay = process.DefaultWaitDelay

func smoke(ctx context.Context, file, version string) error {
	r := process.Run(ctx, process.Options{Argv: []string{file, "--version"}, Stdin: []byte{}, Timeout: ProbeTimeout, WaitDelay: ProbeWaitDelay, MaxOutputBytes: 100000})
	mismatch := strings.TrimSpace(r.Stdout) != version
	if !r.OK || mismatch {
		// Only the enumerated terminal state and a flag: never paths or child output.
		return fmt.Errorf("release smoke check failed; current CLI unchanged (probe %s, version mismatch %t)", r.TerminalState(), mismatch)
	}
	return nil
}
func lock(o Options) (func(), error) {
	if err := os.MkdirAll(o.Home, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(o.Home, ".install-lock")
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("install lock exists or cannot be created; verify no installer is active before recovering it: %w", err)
	}
	return func() { os.Remove(path) }, nil
}
func verify(dir string) (manifest, error) {
	var m manifest
	st, err := os.Lstat(dir)
	if err != nil {
		return m, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return m, fmt.Errorf("invalid release directory")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if !versionPattern.MatchString(m.Version) || filepath.Base(dir) != m.Version ||
		m.Kind != "" && m.Kind != harness.ReleaseKind ||
		m.ProtocolVersion != "" && !harness.ValidProtocolVersion(m.ProtocolVersion) {
		return m, fmt.Errorf("invalid installed release metadata")
	}
	file := filepath.Join(dir, "forgecell")
	st, err = os.Lstat(file)
	if err != nil {
		return m, err
	}
	if !st.Mode().IsRegular() || st.Size() > 100_000_000 {
		return m, fmt.Errorf("invalid installed executable")
	}
	raw, err = os.ReadFile(file)
	if err != nil {
		return m, err
	}
	if sum(raw) != m.SHA256 {
		return m, fmt.Errorf("installed release checksum mismatch")
	}
	return m, nil
}

// stageRelease verifies o.Source and stores it as Home/releases/<version> without
// touching current, previous, the launcher or the exposed command. The caller holds the
// install lock and has created Home/releases.
func stageRelease(ctx context.Context, o Options) (string, error) {
	st, err := os.Stat(o.Source)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > 100_000_000 {
		return "", fmt.Errorf("invalid release executable")
	}
	raw, err := os.ReadFile(o.Source)
	if err != nil {
		return "", err
	}
	protocol := o.Protocol
	if protocol == "" {
		protocol = harness.AdapterProtocolVersion
	}
	m := manifest{Kind: harness.ReleaseKind, Version: o.Version, SHA256: sum(raw), ProtocolVersion: protocol}
	stage, err := os.MkdirTemp(o.Home, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, "forgecell")
	if err = os.WriteFile(file, raw, 0755); err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(stage, "THIRD-PARTY-NOTICES.txt"), notices, 0644); err != nil {
		return "", err
	}
	meta, _ := json.Marshal(m)
	if err = os.WriteFile(filepath.Join(stage, "manifest.json"), meta, 0600); err != nil {
		return "", err
	}
	if err = smoke(ctx, file, o.Version); err != nil {
		return "", err
	}
	dest := filepath.Join(o.Home, "releases", o.Version)
	if _, e := os.Lstat(dest); e == nil {
		existing, e := verify(dest)
		if e != nil {
			return "", e
		}
		if existing.Version != m.Version || existing.SHA256 != m.SHA256 || (o.Protocol != "" && existing.ProtocolVersion != "" && existing.ProtocolVersion != m.ProtocolVersion) {
			return "", fmt.Errorf("release version already exists with different content")
		}
		if existing.Kind == "" || existing.ProtocolVersion == "" {
			protocol := ""
			if existing.ProtocolVersion == "" {
				// The staged copy is byte-identical to the stored release, so it reports
				// the stored release's protocol; the running installer's says nothing about it.
				if protocol = o.Protocol; protocol == "" {
					protocol, _ = builtProtocol(ctx, file)
				}
			}
			if err = backfillManifest(dest, existing, protocol); err != nil {
				return "", err
			}
		}
	} else if !os.IsNotExist(e) {
		return "", e
	} else if err = os.Rename(stage, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// backfillManifest fills the kind a release installed by an earlier build lacks, and
// its protocolVersion when one is established (protocol is empty when it is not, and
// the release then stays unknown), and replaces manifest.json atomically. Fields the
// release already records stay.
func backfillManifest(dir string, existing manifest, protocol string) error {
	next := existing
	if next.Kind == "" {
		next.Kind = harness.ReleaseKind
	}
	if next.ProtocolVersion == "" {
		next.ProtocolVersion = protocol
	}
	if next == existing {
		return nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Chmod(0600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "manifest.json"))
}

// Stage stores a verified release beside the active ones without activating it:
// current, previous, the launcher and the exposed command are left alone.
func Stage(ctx context.Context, o Options) (dir string, err error) {
	o, err = normalizeDirs(o, false)
	if err != nil {
		return "", err
	}
	if !versionPattern.MatchString(o.Version) {
		return "", fmt.Errorf("only versioned release builds can be installed")
	}
	unlock, err := lock(o)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = os.MkdirAll(filepath.Join(o.Home, "releases"), 0700); err != nil {
		return "", err
	}
	return stageRelease(ctx, o)
}

func Install(ctx context.Context, o Options) (result Result, err error) {
	o, err = normalize(o)
	if err != nil {
		return result, err
	}
	if !versionPattern.MatchString(o.Version) {
		return result, fmt.Errorf("only versioned release builds can be installed")
	}
	unlock, err := lock(o)
	if err != nil {
		return result, err
	}
	defer unlock()
	old, err := target(o.Home, "current")
	if err != nil {
		return result, err
	}
	if _, err = target(o.Home, "previous"); err != nil {
		return result, err
	}
	launcher := filepath.Join(o.Home, "bin", "forgecell")
	exposed := filepath.Join(o.Bin, "forgecell")
	wrapper := []byte("#!/bin/sh\n# Forgecell managed native launcher v1\nexport FORGECELL_HOME=" + quote(o.Home) + "\nexport FORGECELL_BIN=" + quote(o.Bin) + "\nexport FORGECELL_LAUNCHER=" + quote(filepath.Join(o.Home, "current", "forgecell")) + "\nexec " + quote(filepath.Join(o.Home, "current", "forgecell")) + " \"$@\"\n")
	if exposed != launcher {
		if st, e := os.Lstat(exposed); e == nil {
			v, e := os.Readlink(exposed)
			if e != nil || st.Mode()&os.ModeSymlink == 0 {
				return result, fmt.Errorf("refusing to replace another command: %s", exposed)
			}
			if !filepath.IsAbs(v) {
				v = filepath.Join(o.Bin, v)
			}
			if filepath.Clean(v) != launcher {
				return result, fmt.Errorf("unmanaged command link")
			}
		} else if !os.IsNotExist(e) {
			return result, e
		}
	}
	if st, e := os.Lstat(launcher); e == nil {
		raw, e := os.ReadFile(launcher)
		if e != nil || !st.Mode().IsRegular() || string(raw) != string(wrapper) {
			return result, fmt.Errorf("refusing unmanaged launcher: %s", launcher)
		}
	} else if !os.IsNotExist(e) {
		return result, e
	}
	for _, dir := range []string{filepath.Join(o.Home, "releases"), filepath.Dir(launcher), o.Bin} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return result, err
		}
	}
	dest, err := stageRelease(ctx, o)
	if err != nil {
		return result, err
	}
	if _, e := os.Lstat(launcher); os.IsNotExist(e) {
		if err = os.WriteFile(launcher, wrapper, 0755); err != nil {
			return result, err
		}
	}
	if old != "" && old != dest {
		if err = point(filepath.Join(o.Home, "previous"), old); err != nil {
			return result, err
		}
	}
	if exposed != launcher {
		if err = point(exposed, launcher); err != nil {
			return result, err
		}
	}
	if err = point(filepath.Join(o.Home, "current"), dest); err != nil {
		return result, err
	}
	return Result{o.Version, exposed, o.Home}, nil
}
func Rollback(ctx context.Context, o Options) (result Result, err error) {
	o, err = normalize(o)
	if err != nil {
		return result, err
	}
	unlock, err := lock(o)
	if err != nil {
		return result, err
	}
	defer unlock()
	old, err := target(o.Home, "current")
	if err != nil {
		return result, err
	}
	previous, err := target(o.Home, "previous")
	if err != nil {
		return result, err
	}
	if previous == "" {
		return result, fmt.Errorf("no previous release to restore")
	}
	m, err := verify(previous)
	if err != nil {
		return result, err
	}
	if err = smoke(ctx, filepath.Join(previous, "forgecell"), m.Version); err != nil {
		return result, err
	}
	if err = point(filepath.Join(o.Home, "current"), previous); err != nil {
		return result, err
	}
	if old != "" {
		if err = point(filepath.Join(o.Home, "previous"), old); err != nil {
			return result, err
		}
	}
	return Result{m.Version, filepath.Join(o.Bin, "forgecell"), o.Home}, nil
}
