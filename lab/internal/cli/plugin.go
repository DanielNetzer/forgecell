package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
)

const pluginUsage = "usage: forgecell plugin test <manifest> [--json] | forgecell plugin list [--lab DIR] [--lab-name NAME] [--json] | forgecell plugin doctor [NAME] [--lab DIR] [--lab-name NAME] [--json]"

// pluginTestOptions configures plugin test runs; tests replace it to keep runs
// under their temporary directory and to fire the shutdown grace at once.
var pluginTestOptions = func() conformance.Options {
	return conformance.Options{Root: os.TempDir(), LookupEnv: os.LookupEnv}
}

// runPlugin runs `plugin test <manifest> [--json]`: the contract-mode
// conformance kit, offline. It exits 0 when every check passes, 2 when one
// fails and 1 on bad usage. It needs no Lab. `plugin list` is runPluginList and
// `plugin doctor` is runPluginDoctor.
func runPlugin(ctx context.Context, args []string, out, stderr io.Writer) int {
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
	manifest, jsonMode := "", false
	for _, arg := range args[1:] {
		switch {
		case arg == "--json":
			jsonMode = true
		case manifest == "" && arg != "" && arg[0] != '-':
			manifest = arg
		default:
			fmt.Fprintln(stderr, "forgecell:", pluginUsage)
			return 1
		}
	}
	if manifest == "" {
		fmt.Fprintln(stderr, "forgecell:", pluginUsage)
		return 1
	}
	abs, err := filepath.Abs(manifest)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: the manifest path cannot be resolved")
		return 1
	}
	opts := pluginTestOptions()
	if root, err := filepath.EvalSymlinks(opts.Root); err == nil {
		opts.Root = root
	}
	report := conformance.Run(ctx, abs, opts)
	if jsonMode {
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
