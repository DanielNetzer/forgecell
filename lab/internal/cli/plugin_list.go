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
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Statuses of a plugin list row.
const (
	pluginListOK              = "ok"
	pluginListNotInstalled    = "not-installed"
	pluginListManifestChanged = "manifest-changed"
	pluginListKindMismatch    = "kind-mismatch"
	pluginListInvalid         = "invalid"
	pluginListFailed          = "failed"
	pluginListUnused          = "unused"
)

// pluginListRow is one pinned plugin. It holds no path: the manifest location
// stays in the Formula.
type pluginListRow struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Contract string `json:"contract"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

// runPluginList runs `plugin list [--lab DIR] [--lab-name NAME] [--json]`. It
// loads each manifest the active approved Formula pins, with its pinned digest,
// and starts no plugin process. It exits 0 when every row is ok or there are
// none, 2 when a row is not ok and 1 when there is no Lab or approved Formula.
func runPluginList(ctx context.Context, args []string, out, stderr io.Writer) int {
	rest, name, explicitLab, err := labArgs(append([]string{"list"}, args...))
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	flags := flag.NewFlagSet("plugin list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	lab := flags.String("lab", "", "Lab directory")
	jsonMode := flags.Bool("json", false, "Structured output")
	if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 || (explicitLab && *lab == "") {
		fmt.Fprintln(stderr, "forgecell:", pluginUsage)
		return 1
	}
	if !explicitLab {
		resolved, err := resolveLab(ctx, name)
		if err != nil || resolved.Dir == "" {
			fmt.Fprintln(stderr, "forgecell: plugin list cannot resolve a Lab for this checkout; pass --lab DIR")
			return 1
		}
		*lab = resolved.Dir
	}
	dir, err := filepath.Abs(*lab)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: plugin list cannot resolve the Lab directory; pass --lab DIR")
		return 1
	}
	// Inspect tells a Lab with no active Formula, or one whose active Formula is
	// unreadable or invalid, from one that is only not approved.
	inspected, err := formula.Inspect(dir, "")
	if err != nil {
		fmt.Fprintln(stderr, "forgecell: plugin list refused: the active Formula could not be loaded")
		return 1
	}
	loaded, err := formula.Load(dir, "")
	if inspected.Formula.ID == "" || err != nil || !loaded.Approved {
		fmt.Fprintln(stderr, "forgecell: plugin list needs a Lab with an active approved Formula; review and approve one with forgecell init")
		return 1
	}
	rows := pluginListRows(dir, loaded.Formula)
	if *jsonMode {
		if encode(out, stderr, struct {
			Plugins []pluginListRow `json:"plugins"`
		}{rows}) != 0 {
			return 1
		}
	} else if writePluginList(out, rows) != nil {
		return 1
	}
	for _, r := range rows {
		if r.Status != pluginListOK {
			return 2
		}
	}
	return 0
}

// pluginListRows loads each pinned manifest and checks its kind against every
// role the Formula gives it. A load failure keeps only its fixed reason.
func pluginListRows(labDir string, f formula.Formula) []pluginListRow {
	roles := f.PluginRoles()
	rows := make([]pluginListRow, 0, len(f.Plugins))
	for _, p := range f.Plugins {
		row := pluginListRow{Name: p.Name, Role: pluginListUnused, Status: pluginListOK}
		if len(roles[p.Name]) > 0 {
			row.Role = strings.Join(roles[p.Name], ",")
		}
		m, _, err := plugin.Load(labDir, p.Manifest, p.SHA256)
		if err != nil {
			row.Status = pluginListLoadStatus(err)
			rows = append(rows, row)
			continue
		}
		row.Kind, row.Contract = m.Kind, m.Contract
		for _, role := range roles[p.Name] {
			if m.Kind != role {
				row.Status = pluginListKindMismatch
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func pluginListLoadStatus(err error) string {
	switch mcp.ReasonOf(err) {
	case mcp.NotInstalled:
		return pluginListNotInstalled
	case mcp.ManifestChanged:
		return pluginListManifestChanged
	case mcp.SchemaInvalid:
		return pluginListInvalid
	}
	return pluginListFailed
}

func writePluginList(out io.Writer, rows []pluginListRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "The active Formula pins no plugins.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tCONTRACT\tROLE\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, pluginListCell(r.Kind), pluginListCell(r.Contract), r.Role, r.Status)
	}
	return w.Flush()
}

func pluginListCell(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
