package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// maxForgeLedgerBytes bounds the ledger read that finds a Molecule's forge.
const maxForgeLedgerBytes = fixround.MaxForgeLedgerBytes

// moleculeForgeFormula returns the Formula snapshot a Molecule ran with when it
// declares a plugin forge (true), or false for GitHub; see
// fixround.SnapshotFormula. Forge selection never fails open to GitHub.
func moleculeForgeFormula(lab, id string) (formula.Formula, bool, error) {
	return fixround.SnapshotFormula(lab, id)
}

// newPluginForge selects the Formula's forge plugin for the Lab, with a call
// log the delivery records keep.
func newPluginForge(lab string, f formula.Formula) (forge.Changes, delivery.PluginForge, error) {
	return fixround.PluginForge(lab, f, pluginLookupEnv)
}

// deliverPlugin publishes through the Molecule's forge plugin. The receipt is
// written as on GitHub; one result line names the change with the forge's own
// noun.
func deliverPlugin(ctx context.Context, f formula.Formula, o delivery.PublishOptions, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	changes, plugin, err := newPluginForge(o.LabDir, f)
	if err != nil {
		return fail(err)
	}
	o.Changes, o.Plugin = changes, plugin
	result, err := delivery.Publish(ctx, o)
	if err != nil {
		return fail(err)
	}
	if code := encode(out, stderr, result); code != 0 {
		return code
	}
	if line := pluginDeliverLine(result); line != "" {
		fmt.Fprintln(stderr, line)
	}
	return 0
}

// pluginDeliverLine reports a published plugin forge receipt's change with the
// forge's noun: created, or an existing draft adopted, and verified.
func pluginDeliverLine(r delivery.Receipt) string {
	noun := r.ChangeNoun
	if noun == "" {
		noun = delivery.DefaultChangeNoun
	}
	switch delivery.PluginPublishOutcome(r) {
	case "created":
		return "Draft " + noun + " created and verified: " + r.URL
	case "existing-draft":
		return "Existing draft " + noun + " verified: " + r.URL
	}
	return ""
}

// pluginCheckFlags are the checks flags of a plugin forge delivery, read before
// the flag set so the static usage text stays unchanged.
type pluginCheckFlags struct {
	project, ref string
	set          bool
}

// checksValueFlags are the checks flags that take a value, so their value is
// never read as a flag.
var checksValueFlags = map[string]bool{"lab": true, "molecule": true, "repo": true, "pr": true, "commit": true, "required": true}

// splitPluginCheckFlags removes --project and --ref (and their -name and
// =value forms) from the checks arguments and returns the rest. Scanning stops
// at a "--" terminator or the first positional argument, as the flag package's
// does.
func splitPluginCheckFlags(args []string) ([]string, pluginCheckFlags, error) {
	var flags pluginCheckFlags
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if name != "project" && name != "ref" {
			rest = append(rest, a)
			if checksValueFlags[name] && !hasValue && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, flags, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if name == "project" {
			flags.project = value
		} else {
			flags.ref = value
		}
		flags.set = true
	}
	return rest, flags, nil
}

// checksPlugin collects a plugin forge delivery's CI evidence by project, change
// ref and commit, and reports it with the forge's noun.
func checksPlugin(ctx context.Context, f formula.Formula, link delivery.Options, flags pluginCheckFlags, commit string, required []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	changes, plugin, err := newPluginForge(link.LabDir, f)
	if err != nil {
		return fail(err)
	}
	observation, err := delivery.CollectCIPlugin(ctx, link, delivery.PluginCITarget{Project: flags.project, Ref: flags.ref, Commit: commit, Required: required, Changes: changes, Calls: plugin.Calls})
	if err != nil {
		return fail(err)
	}
	if code := encode(out, stderr, observation); code != 0 {
		return code
	}
	noun, err := delivery.ReadPluginChangeNoun(link)
	if err != nil {
		noun = delivery.DefaultChangeNoun
	}
	fmt.Fprintln(stderr, pluginChecksLine(noun, observation))
	if observation.Snapshot.Status != "passed" {
		return 2
	}
	return 0
}

// pluginChecksLine reports a plugin CI observation with the forge's noun.
func pluginChecksLine(noun string, a delivery.CIObservation) string {
	return fmt.Sprintf("Checks on %s %s at %s: %s", noun, a.Identity.Ref, a.Snapshot.Commit, a.Snapshot.Status)
}

// checksForgeRoute decides how checks runs: through the plugin forge (true)
// or GitHub (false). Mixing GitHub and plugin flags, plugin flags without a
// plugin forge, and GitHub flags with one are refused with a plain reason.
func checksForgeRoute(lab, molecule string, flags pluginCheckFlags, repo string, pr int) (formula.Formula, bool, error) {
	if flags.set && (repo != "" || pr != 0) {
		return formula.Formula{}, false, fmt.Errorf("use --project and --ref for a plugin forge, or --repo and --pr for GitHub, not both")
	}
	f, plugin, err := moleculeForgeFormula(lab, molecule)
	if err != nil {
		return formula.Formula{}, false, err
	}
	switch {
	case flags.set && !plugin:
		return formula.Formula{}, false, fmt.Errorf("--project and --ref are for a Molecule whose Formula delivers through a forge plugin; this one delivers through GitHub, so use --repo and --pr")
	case !flags.set && plugin:
		return formula.Formula{}, false, fmt.Errorf("this Molecule's Formula delivers through the forge plugin %s; use --project and --ref in place of --repo and --pr", f.Forge.Plugin)
	}
	return f, plugin, nil
}
