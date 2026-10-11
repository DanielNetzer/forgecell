package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/DanielNetzer/forgecell/lab/internal/install"
)

// runDev handles the developer-only `dev install`, which builds a local checkout into a
// stamped dev release so ordinary rollback and managed-link guards apply to it.
func runDev(ctx context.Context, args []string, version string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if len(args) == 0 || args[0] != "install" {
		return fail(fmt.Errorf("dev requires the install subcommand: dev install [--from CHECKOUT] [--activate] [--allow-dirty]"))
	}
	flags := flag.NewFlagSet("dev install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from := flags.String("from", ".", "Forgecell checkout to build")
	activate := flags.Bool("activate", false, "Make the build the current release")
	allowDirty := flags.Bool("allow-dirty", false, "Build a checkout with uncommitted changes")
	if err := flags.Parse(args[1:]); err != nil {
		return fail(err)
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("dev install takes no positional arguments"))
	}
	release, err := install.DevInstall(ctx, install.DevOptions{Home: os.Getenv("FORGECELL_HOME"), Bin: os.Getenv("FORGECELL_BIN"), Checkout: *from, RunningVersion: version, Activate: *activate, AllowDirty: *allowDirty})
	if err != nil {
		return fail(err)
	}
	if code := encode(out, stderr, release); code != 0 {
		return code
	}
	if release.Activated {
		fmt.Fprintf(stderr, "Forgecell dev build %s installed and activated: %s\n", release.Version, release.Path)
	} else {
		fmt.Fprintf(stderr, "Forgecell dev build %s staged only; current is unchanged (add --activate to switch): %s\n", release.Version, release.Path)
	}
	return 0
}
