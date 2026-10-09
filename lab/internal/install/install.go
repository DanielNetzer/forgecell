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

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

//go:embed notices.txt
var notices []byte
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)

type Options struct{ Home, Bin, Source, Version string }
type Result struct {
	Version string `json:"version"`
	Command string `json:"command"`
	Home    string `json:"home"`
}
type manifest struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func sum(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func normalize(o Options) (Options, error) {
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
	o.Bin, err = filepath.Abs(o.Bin)
	if err != nil {
		return o, err
	}
	for _, dir := range []string{o.Home, o.Bin} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return o, err
		}
	}
	o.Home, err = filepath.EvalSymlinks(o.Home)
	if err != nil {
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
func smoke(ctx context.Context, file, version string) error {
	r := process.Run(ctx, process.Options{Argv: []string{file, "--version"}, Stdin: []byte{}, Timeout: 15 * time.Second, MaxOutputBytes: 100000})
	if !r.OK || strings.TrimSpace(r.Stdout) != version {
		return fmt.Errorf("release smoke check failed; current CLI unchanged")
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
	if !versionPattern.MatchString(m.Version) || filepath.Base(dir) != m.Version {
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
	st, err := os.Stat(o.Source)
	if err != nil {
		return result, err
	}
	if !st.Mode().IsRegular() || st.Size() > 100_000_000 {
		return result, fmt.Errorf("invalid release executable")
	}
	raw, err := os.ReadFile(o.Source)
	if err != nil {
		return result, err
	}
	m := manifest{o.Version, sum(raw)}
	stage, err := os.MkdirTemp(o.Home, ".install-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, "forgecell")
	if err = os.WriteFile(file, raw, 0755); err != nil {
		return result, err
	}
	if err = os.WriteFile(filepath.Join(stage, "THIRD-PARTY-NOTICES.txt"), notices, 0644); err != nil {
		return result, err
	}
	meta, _ := json.Marshal(m)
	if err = os.WriteFile(filepath.Join(stage, "manifest.json"), meta, 0600); err != nil {
		return result, err
	}
	if err = smoke(ctx, file, o.Version); err != nil {
		return result, err
	}
	dest := filepath.Join(o.Home, "releases", o.Version)
	if _, e := os.Lstat(dest); e == nil {
		existing, e := verify(dest)
		if e != nil {
			return result, e
		}
		if existing != m {
			return result, fmt.Errorf("release version already exists with different content")
		}
	} else if !os.IsNotExist(e) {
		return result, e
	} else if err = os.Rename(stage, dest); err != nil {
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
