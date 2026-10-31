package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// RunRootName is the Lab directory under which every plugin start makes its
// private, marked run directory.
const RunRootName = "plugin-runs"

// PrepareRunRoot returns the Lab's plugin run root, LabDir/plugin-runs, and
// creates it with mode 0700 when it is absent. The Lab directory must be a
// clean absolute path that is a real directory owned by the current user, not
// a symlink. An existing run root is never repaired: one that is a symlink,
// not a directory, not owned by the current user or open to others is
// refused, as Options.check refuses it. The errors name no path.
func PrepareRunRoot(labDir string) (string, error) {
	bad := func(why string) error { return refuse("", stepOptions, mcp.Failed, why) }
	if labDir == "" || !filepath.IsAbs(labDir) || filepath.Clean(labDir) != labDir {
		return "", bad("the Lab directory must be a clean absolute path")
	}
	info, err := os.Lstat(labDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByMe(info) {
		return "", bad("the Lab directory must be a real directory owned by the current user")
	}
	root := filepath.Join(labDir, RunRootName)
	switch err := os.Mkdir(root, 0o700); {
	case err == nil:
		// The umask may have narrowed the mode; only the directory this call
		// created, and checked to be that directory, is widened back to 0700.
		created, statErr := os.Lstat(root)
		if statErr != nil || created.Mode()&os.ModeSymlink != 0 || !created.IsDir() || os.Chmod(root, 0o700) != nil {
			return "", bad("the run root could not be created as a private directory")
		}
	case !errors.Is(err, fs.ErrExist):
		return "", bad("the run root could not be created")
	}
	if err := (Options{LabDir: labDir, RunRoot: root}).check(); err != nil {
		return "", err
	}
	return root, nil
}

// TrackerFor returns the plugin tracker the Formula's intake.source names,
// with intake.config as the {config} of every call, or false when intake uses
// the built-in GitHub Issues tracker, so the caller keeps its own path. It
// sets opts' LabDir, Ref and RunRoot itself: the run root is the Lab's own,
// from PrepareRunRoot, never the user's temporary directory or home.
func TrackerFor(labDir string, f formula.Formula, opts Options) (tracker.Keyed, bool, error) {
	p, ok := f.TrackerPlugin()
	if !ok {
		if f.Intake.Source != "" && f.Intake.Source != formula.DefaultIntakeSource {
			return nil, false, refuse("", stepOptions, mcp.Failed, "intake.source names no declared plugin")
		}
		return nil, false, nil
	}
	root, err := PrepareRunRoot(labDir)
	if err != nil {
		return nil, true, err
	}
	opts.LabDir, opts.Ref, opts.RunRoot = labDir, Ref{Manifest: p.Manifest, SHA256: p.SHA256}, root
	var config any
	if f.Intake.Config != nil {
		config = f.Intake.Config
	}
	t, err := NewTracker(opts, config)
	if err != nil {
		return nil, true, err
	}
	return t, true, nil
}

// ForgeFor returns the plugin forge the Formula's forge block names, with
// forge.project as the {project} of every call, or false when no forge block is
// declared, so the caller keeps forge.GitHub. A forge block that names no
// declared plugin is refused, never taken as GitHub. It sets opts' LabDir, Ref
// and RunRoot itself, as TrackerFor does.
func ForgeFor(labDir string, f formula.Formula, opts Options) (forge.Changes, bool, error) {
	p, project, ok := f.ForgePlugin()
	if !ok {
		if f.Forge != nil {
			return nil, false, refuse("", stepOptions, mcp.Failed, "forge.plugin names no declared plugin")
		}
		return nil, false, nil
	}
	root, err := PrepareRunRoot(labDir)
	if err != nil {
		return nil, true, err
	}
	opts.LabDir, opts.Ref, opts.RunRoot = labDir, Ref{Manifest: p.Manifest, SHA256: p.SHA256}, root
	fg, err := NewForge(opts, project)
	if err != nil {
		return nil, true, err
	}
	return fg, true, nil
}

// DescribeReviewer runs reviewer_describe on a reviewer plugin with config, a
// JSON object of at most MaxConfigBytes (nil is the empty object), as the
// {config} of the call. It is the reviewer counterpart of Tracker.Describe and
// Forge.Describe, for plugin diagnosis.
func DescribeReviewer(ctx context.Context, opts Options, config any) (contract.ReviewerDescription, error) {
	if config == nil {
		config = map[string]any{}
	}
	raw, err := json.Marshal(config)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || compact.Len() > MaxConfigBytes || compact.Bytes()[0] != '{' {
		return contract.ReviewerDescription{}, refuse("", stepOptions, mcp.SchemaInvalid, "the reviewer config must be a JSON object of at most 4 KiB")
	}
	var out contract.ReviewerDescription
	err = run(ctx, opts, "reviewer", func(s *session) error {
		d, err := callAs[contract.ReviewerDescription](ctx, s, "reviewer_describe", map[string]any{"config": json.RawMessage(compact.Bytes())})
		out = d
		return err
	})
	return out, err
}
