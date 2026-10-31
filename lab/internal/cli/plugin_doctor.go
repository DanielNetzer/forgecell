package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Bounds of plugin doctor's display of plugin-supplied text.
const (
	pluginDoctorFieldRunes = 256
	pluginDoctorSelectors  = 16
)

// pluginDoctorOptions adjusts the adapter options of each start; tests replace
// it to fire the shutdown grace at once.
var pluginDoctorOptions = func(o mcpadapter.Options) mcpadapter.Options { return o }

// pluginDoctorRow is one declared plugin as plugin doctor found it. It holds no
// path and no environment value: MissingEnv names unset needs.env entries only.
type pluginDoctorRow struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	Contract    string   `json:"contract"`
	Status      string   `json:"status"`
	DisplayName string   `json:"displayName,omitempty"`
	KeyPattern  string   `json:"keyPattern,omitempty"`
	Selectors   []string `json:"selectors,omitempty"`
	Identity    string   `json:"identity,omitempty"`
	CIKind      string   `json:"ciKind,omitempty"`
	MissingEnv  []string `json:"missingEnv"`
}

// runPluginDoctor runs `plugin doctor [NAME] [--lab DIR] [--lab-name NAME]
// [--json]`. For each plugin the active approved Formula declares, or only
// NAME, it starts the plugin through the adapter under the Lab's run root and
// runs its kind's describe tool. A row's status is ok or the fixed mcp reason
// of the failure. It exits 0 when every row is ok or there are none, 2 when a
// row is not ok and 1 on bad usage or when there is no Lab or approved Formula.
func runPluginDoctor(ctx context.Context, args []string, out, stderr io.Writer) int {
	rest, labName, explicitLab, err := labArgs(append([]string{"doctor"}, args...))
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	flags := flag.NewFlagSet("plugin doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	lab := flags.String("lab", "", "Lab directory")
	jsonMode := flags.Bool("json", false, "Structured output")
	// The optional NAME may come before, between or after the flags; a second
	// operand is bad usage.
	only, remaining := "", rest[1:]
	for {
		if err := flags.Parse(remaining); err != nil {
			fmt.Fprintln(stderr, "forgecell:", pluginUsage)
			return 1
		}
		if flags.NArg() == 0 {
			break
		}
		if only != "" || flags.Arg(0) == "" {
			fmt.Fprintln(stderr, "forgecell:", pluginUsage)
			return 1
		}
		only, remaining = flags.Arg(0), flags.Args()[1:]
	}
	if explicitLab && *lab == "" {
		fmt.Fprintln(stderr, "forgecell:", pluginUsage)
		return 1
	}
	if !explicitLab {
		resolved, err := resolveLab(ctx, labName)
		if err != nil || resolved.Dir == "" {
			fmt.Fprintln(stderr, "forgecell: plugin doctor cannot resolve a Lab for this checkout; pass --lab DIR")
			return 1
		}
		*lab = resolved.Dir
	}
	dir, err := filepath.Abs(*lab)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: plugin doctor cannot resolve the Lab directory; pass --lab DIR")
		return 1
	}
	inspected, err := formula.Inspect(dir, "")
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: plugin doctor refused: the active Formula could not be loaded")
		return 1
	}
	loaded, err := formula.Load(dir, "")
	if inspected.Formula.ID == "" || err != nil || !loaded.Approved {
		fmt.Fprintln(stderr, "forgecell: plugin doctor needs a Lab with an active approved Formula; review and approve one with forgecell init")
		return 1
	}
	f := loaded.Formula
	pins := f.Plugins
	if only != "" {
		pins = nil
		for _, p := range f.Plugins {
			if p.Name == only {
				pins = append(pins, p)
			}
		}
		if len(pins) == 0 {
			// The name is the caller's input, so it is not echoed.
			fmt.Fprintln(stderr, "forgecell: plugin doctor refused: the active Formula declares no plugin by that name; forgecell plugin list shows the declared ones")
			return 1
		}
	}
	rows := make([]pluginDoctorRow, 0, len(pins))
	for _, p := range pins {
		rows = append(rows, pluginDoctorCheck(ctx, dir, f, p))
	}
	if *jsonMode {
		if encode(out, stderr, struct {
			Plugins []pluginDoctorRow `json:"plugins"`
		}{rows}) != 0 {
			return 1
		}
	} else if writePluginDoctor(out, rows) != nil {
		return 1
	}
	for _, r := range rows {
		if r.Status != pluginListOK {
			return 2
		}
	}
	return 0
}

// pluginDoctorCheck loads one pin, names its unset needs.env entries, starts
// it and runs its describe tool. Every failure keeps only its fixed reason.
func pluginDoctorCheck(ctx context.Context, labDir string, f formula.Formula, p formula.PluginRef) pluginDoctorRow {
	row := pluginDoctorRow{Name: p.Name, Role: pluginListUnused, Status: pluginListOK, MissingEnv: []string{}}
	roles := f.PluginRoles()[p.Name]
	if len(roles) > 0 {
		row.Role = strings.Join(roles, ",")
	}
	m, _, err := plugin.Load(labDir, p.Manifest, p.SHA256)
	if err != nil {
		row.Status = string(mcp.ReasonOf(err))
		return row
	}
	row.Kind, row.Contract = m.Kind, m.Contract
	for _, name := range m.Needs.Env {
		if value, ok := pluginLookupEnv(name); !ok || value == "" {
			row.MissingEnv = append(row.MissingEnv, name)
		}
	}
	root, err := mcpadapter.PrepareRunRoot(labDir)
	if err != nil {
		row.Status = string(mcp.ReasonOf(err))
		return row
	}
	opts := pluginDoctorOptions(mcpadapter.Options{LabDir: labDir, Ref: mcpadapter.Ref{Manifest: p.Manifest, SHA256: p.SHA256}, RunRoot: root, LookupEnv: pluginLookupEnv})
	text := func(s string) string { return flatten(pluginDoctorCut(s)) }
	switch m.Kind {
	case "tracker":
		var config any
		if tp, ok := f.TrackerPlugin(); ok && tp.Name == p.Name && f.Intake.Config != nil {
			config = f.Intake.Config
		}
		var t *mcpadapter.Tracker
		if t, err = mcpadapter.NewTracker(opts, config); err == nil {
			d, describeErr := t.Describe(ctx)
			if err = describeErr; err == nil {
				row.DisplayName, row.KeyPattern = text(d.DisplayName), text(d.KeyPattern)
				for i, s := range d.Selectors {
					if i == pluginDoctorSelectors {
						row.Selectors = append(row.Selectors, "…")
						break
					}
					row.Selectors = append(row.Selectors, text(s))
				}
			}
		}
	case "forge":
		project := f.Intake.Repo
		if fp, forgeProject, ok := f.ForgePlugin(); ok && fp.Name == p.Name {
			project = forgeProject
		}
		if project == "" {
			project = "forgecell-doctor"
		}
		var fg *mcpadapter.Forge
		if fg, err = mcpadapter.NewForge(opts, project); err == nil {
			d, describeErr := fg.Describe(ctx)
			if err = describeErr; err == nil {
				row.DisplayName, row.Identity, row.CIKind = text(d.DisplayName), text(d.Identity), text(d.CIKind)
			}
		}
	case "reviewer":
		var config any
		for _, r := range f.Reviewers() {
			if r.Plugin == p.Name && r.Config != nil {
				config = r.Config
				break
			}
		}
		d, describeErr := mcpadapter.DescribeReviewer(ctx, opts, config)
		if err = describeErr; err == nil {
			row.DisplayName = text(d.DisplayName)
		}
	default:
		err = &mcp.Error{Reason: mcp.ContractMismatch}
	}
	if err != nil {
		row.Status = string(mcp.ReasonOf(err))
	}
	return row
}

// pluginDoctorCut bounds plugin-supplied text before display.
func pluginDoctorCut(s string) string {
	if r := []rune(s); len(r) > pluginDoctorFieldRunes {
		return string(r[:pluginDoctorFieldRunes]) + "…"
	}
	return s
}

func writePluginDoctor(out io.Writer, rows []pluginDoctorRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "The active Formula pins no plugins.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tCONTRACT\tROLE\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, pluginListCell(r.Kind), pluginListCell(r.Contract), r.Role, r.Status)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, r := range rows {
		var parts []string
		if r.DisplayName != "" {
			parts = append(parts, r.DisplayName)
		}
		if r.KeyPattern != "" {
			parts = append(parts, "key pattern "+r.KeyPattern)
		}
		if len(r.Selectors) > 0 {
			parts = append(parts, "selectors "+strings.Join(r.Selectors, ", "))
		}
		if r.Identity != "" {
			parts = append(parts, "identity "+r.Identity)
		}
		if r.CIKind != "" {
			parts = append(parts, "CI "+r.CIKind)
		}
		if len(r.MissingEnv) > 0 {
			parts = append(parts, "unset env "+strings.Join(r.MissingEnv, ", "))
		}
		if len(parts) > 0 {
			if _, err := fmt.Fprintf(out, "%s: %s\n", r.Name, strings.Join(parts, " · ")); err != nil {
				return err
			}
		}
	}
	return nil
}
