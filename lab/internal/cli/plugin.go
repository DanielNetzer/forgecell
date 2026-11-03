package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
)

const pluginUsage = "usage: forgecell plugin test <manifest> [--json] | forgecell plugin list [--lab DIR] [--lab-name NAME] [--json] | forgecell plugin doctor [NAME] [--lab DIR] [--lab-name NAME] [--json]"

// The usage of plugin new and plugin pin, printed by those commands only, so
// the usage that test, list and doctor print stays as it was.
const (
	pluginNewUsage = "usage: forgecell plugin new --kind tracker|forge|reviewer --lang go|python|typescript [--dir DIR] <name>"
	pluginPinUsage = "usage: forgecell plugin pin <dir> --server <path> [--interpreter <path>]"
)

// pluginTestOptions configures plugin test runs; tests replace it to keep runs
// under their temporary directory and to fire the shutdown grace at once.
var pluginTestOptions = func() conformance.Options {
	return conformance.Options{Root: os.TempDir(), LookupEnv: os.LookupEnv}
}

// runPlugin runs `plugin test <manifest> [--json]`: the contract-mode
// conformance kit, offline, or with --live the contract checks and then the
// live checks against --target (see pluginTestLiveUsage). It exits 0 when
// every check passes, 2 when one fails and 1 on bad usage. It needs no Lab. `plugin list` is runPluginList,
// `plugin doctor` is runPluginDoctor, `plugin new` is runPluginNew and
// `plugin pin` is runPluginPin.
func runPlugin(ctx context.Context, args []string, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "new" {
		return runPluginNew(args[1:], out, stderr)
	}
	if len(args) > 0 && args[0] == "pin" {
		return runPluginPin(args[1:], out, stderr)
	}
	if len(args) > 0 && args[0] == "list" {
		return runPluginList(ctx, args[1:], out, stderr)
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runPluginDoctor(ctx, args[1:], out, stderr)
	}
	if len(args) == 0 || args[0] != "test" {
		fmt.Fprintln(stderr, "forgecell:", pluginUsage)
		return 1
	}
	t, usage, refusal := parsePluginTest(args[1:])
	switch {
	case usage:
		fmt.Fprintln(stderr, "forgecell:", pluginUsage)
		return 1
	case refusal != "":
		fmt.Fprintln(stderr, "forgecell:", refusal)
		fmt.Fprintln(stderr, "forgecell:", pluginTestLiveUsage)
		return 1
	}
	abs, err := filepath.Abs(t.manifest)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: the manifest path cannot be resolved")
		return 1
	}
	var live conformance.Live
	if t.live {
		if live, refusal = t.liveOptions(abs); refusal != "" {
			fmt.Fprintln(stderr, "forgecell:", refusal)
			fmt.Fprintln(stderr, "forgecell:", pluginTestLiveUsage)
			return 1
		}
	}
	opts := pluginTestOptions()
	if root, err := filepath.EvalSymlinks(opts.Root); err == nil {
		opts.Root = root
	}
	var report conformance.Report
	if t.live {
		report = conformance.RunLive(ctx, abs, opts, live)
	} else {
		report = conformance.Run(ctx, abs, opts)
	}
	if t.json {
		err = report.WriteJSON(out)
	} else {
		err = report.WriteText(out)
	}
	if err != nil {
		return 1
	}
	if !report.Passed {
		return 2
	}
	return 0
}

// pluginTestLiveUsage is printed only when live flags are refused, so the usage
// that test, list and doctor print stays as it was.
const pluginTestLiveUsage = "usage: forgecell plugin test <manifest> [--json] [--live --target T [--config FILE] [--change REF --commit SHA] [--allow-write --head BRANCH --base BRANCH]]"

// Bounds of a --config file: the contract's config maxProperties, and 64 KiB.
const (
	pluginConfigMaxBytes      = 64 << 10
	pluginConfigMaxProperties = 64
)

// pluginTestArgs are the parsed arguments of plugin test.
type pluginTestArgs struct {
	manifest   string
	json       bool
	live       bool
	allowWrite bool
	target     string
	config     string
	change     string
	commit     string
	head       string
	base       string
	// given records which of --live and the live flags were given.
	given map[string]bool
}

// liveFlags are every live flag other than --live.
var liveFlags = []string{"--target", "--config", "--change", "--commit", "--allow-write", "--head", "--base"}

// parsePluginTest parses plugin test's arguments. usage is true for the
// arguments contract mode always refused; refusal is the fixed reason a live
// flag is refused. Neither ever names a value.
func parsePluginTest(args []string) (pluginTestArgs, bool, string) {
	t := pluginTestArgs{given: map[string]bool{}}
	values := map[string]*string{"--target": &t.target, "--config": &t.config, "--change": &t.change, "--commit": &t.commit, "--head": &t.head, "--base": &t.base}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			t.json = true
		case arg == "--live" || arg == "--allow-write" || values[arg] != nil:
			if t.given[arg] {
				return t, false, arg + " is given more than once"
			}
			t.given[arg] = true
			if value := values[arg]; value != nil {
				if i+1 >= len(args) || args[i+1] == "" || args[i+1][0] == '-' {
					return t, false, arg + " needs a value"
				}
				i++
				*value = args[i]
			}
		case t.manifest == "" && arg != "" && arg[0] != '-':
			t.manifest = arg
		default:
			return t, true, ""
		}
	}
	if t.manifest == "" {
		return t, true, ""
	}
	t.live, t.allowWrite = t.given["--live"], t.given["--allow-write"]
	if !t.live {
		for _, flag := range liveFlags {
			if t.given[flag] {
				return t, false, flag + " needs --live"
			}
		}
		return t, false, ""
	}
	switch {
	case t.target == "":
		return t, false, "--live requires --target"
	case t.change != "" && t.commit == "":
		return t, false, "--change requires --commit"
	case t.commit != "" && t.change == "" && !t.allowWrite:
		return t, false, "--commit requires --change or --allow-write"
	case (t.head != "" || t.base != "") && !t.allowWrite:
		return t, false, "--head and --base need --allow-write"
	case t.allowWrite && (t.head == "" || t.base == "" || t.commit == ""):
		return t, false, "--allow-write requires --head, --base and --commit"
	}
	return t, false, ""
}

// liveOptions reads the manifest's kind and the --config file and returns the
// live options, or the fixed reason they are refused. --allow-write needs a
// readable forge manifest; a reviewer run needs --change and --commit.
func (t pluginTestArgs) liveOptions(manifest string) (conformance.Live, string) {
	live := conformance.Live{Target: t.target, Change: t.change, Commit: t.commit, AllowWrite: t.allowWrite, Head: t.head, Base: t.base}
	m, _, err := plugin.LoadFile(manifest)
	switch {
	case t.allowWrite && (err != nil || m.Kind != "forge"):
		return live, "--allow-write is only for a forge plugin"
	case err == nil && m.Kind == "reviewer" && (t.change == "" || t.commit == ""):
		return live, "a reviewer live run requires --change and --commit"
	}
	if t.config != "" {
		config, refusal := readPluginConfig(t.config)
		if refusal != "" {
			return live, refusal
		}
		live.Config = config
	}
	return live, ""
}

// readPluginConfig reads a --config file: a regular file, not a symlink, at
// most pluginConfigMaxBytes, holding one JSON object of at most
// pluginConfigMaxProperties properties with no repeated key. Refusals are
// fixed text that never names the path or a value.
func readPluginConfig(path string) (map[string]any, string) {
	const unreadable = "--config must name a readable regular file, not a symlink"
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, unreadable
	}
	if before.Size() > pluginConfigMaxBytes {
		return nil, "--config exceeds 64 KiB"
	}
	// O_NOFOLLOW refuses a symlink swapped in after Lstat, and O_NONBLOCK keeps
	// a FIFO from blocking; the opened file must be the one Lstat saw.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, unreadable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, unreadable
	}
	// The extra byte detects growth beyond the limit after Stat.
	raw, err := io.ReadAll(io.LimitReader(f, pluginConfigMaxBytes+1))
	if err != nil {
		return nil, unreadable
	}
	if len(raw) > pluginConfigMaxBytes {
		return nil, "--config exceeds 64 KiB"
	}
	const invalid = "--config must hold one JSON object with no repeated key"
	if !utf8.Valid(raw) || pinned.RejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var config map[string]any
	if err := dec.Decode(&config); err != nil || config == nil {
		return nil, invalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalid
	}
	if len(config) > pluginConfigMaxProperties {
		return nil, "--config holds more than 64 properties"
	}
	return config, ""
}
