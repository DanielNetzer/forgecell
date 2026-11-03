package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/scaffold"
)

// pluginShownPath is a relative path printed verbatim in a suggested command:
// nothing a shell would read specially.
var pluginShownPath = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// runPluginNew runs `plugin new --kind K --lang L [--dir D] <name>`: it writes a
// plugin skeleton into D (./<name> by default) and prints the next commands
// with relative paths only: for python and typescript they name python3 or node
// and the --interpreter flag of plugin pin. It exits 0 on success and 1
// otherwise.
func runPluginNew(args []string, out, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "forgecell:", pluginNewUsage)
		return 1
	}
	flags := map[string]string{}
	name := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--kind", "--lang", "--dir":
			if _, repeated := flags[arg]; repeated || i+1 >= len(args) || args[i+1] == "" {
				return usage()
			}
			flags[arg] = args[i+1]
			i++
		default:
			if name != "" || arg == "" || arg[0] == '-' {
				return usage()
			}
			name = arg
		}
	}
	if name == "" || flags["--kind"] == "" || flags["--lang"] == "" {
		return usage()
	}
	dir := flags["--dir"]
	if dir == "" {
		dir = "./" + name
	}
	if err := scaffold.Write(dir, scaffold.Options{Kind: flags["--kind"], Lang: flags["--lang"], Name: name}); err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	shown := pluginRelativeDir(dir)
	switch flags["--lang"] {
	case "python":
		fmt.Fprintf(out, "Wrote the %s plugin skeleton %s.\nNext, pin the manifest to server.py and python3 (3.9 or later), and test it:\n", flags["--kind"], name)
		fmt.Fprintf(out, "  forgecell plugin pin %s --server %s/server.py --interpreter <python3>\n", shown, shown)
		fmt.Fprintf(out, "  forgecell plugin test %s/%s\n", shown, scaffold.ManifestFile)
		fmt.Fprintf(out, "<python3> is python3's absolute path with every symlink resolved, as printed by:\n")
		fmt.Fprintf(out, "  python3 -c 'import os, sys; print(os.path.realpath(sys.executable))'\n")
	case "typescript":
		fmt.Fprintf(out, "Wrote the %s plugin skeleton %s.\nNext, build server.js with node (22.18 or later), pin the manifest to it and node, and test it:\n", flags["--kind"], name)
		fmt.Fprintf(out, "  node %s/build.mjs\n", shown)
		fmt.Fprintf(out, "  forgecell plugin pin %s --server %s/server.js --interpreter <node>\n", shown, shown)
		fmt.Fprintf(out, "  forgecell plugin test %s/%s\n", shown, scaffold.ManifestFile)
		fmt.Fprintf(out, "<node> is node's absolute path with every symlink resolved, as printed by:\n")
		fmt.Fprintf(out, "  node -p 'require(\"fs\").realpathSync(process.execPath)'\n")
	default:
		fmt.Fprintf(out, "Wrote the %s plugin skeleton %s.\nNext, build the server, pin the manifest to it and test it:\n", flags["--kind"], name)
		fmt.Fprintf(out, "  go -C %s build -o %s .\n", shown, name)
		fmt.Fprintf(out, "  forgecell plugin pin %s --server %s/%s\n", shown, shown, name)
		fmt.Fprintf(out, "  forgecell plugin test %s/%s\n", shown, scaffold.ManifestFile)
	}
	return 0
}

// pluginRelativeDir shows dir relative to the working directory, or the
// placeholder <dir> when it has no plain relative form, so no absolute path is
// ever printed.
func pluginRelativeDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "<dir>"
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "<dir>"
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || filepath.IsAbs(rel) {
		return "<dir>"
	}
	rel = filepath.ToSlash(rel)
	if rel != "." && !strings.HasPrefix(rel, "../") && rel != ".." {
		rel = "./" + rel
	}
	if !pluginShownPath.MatchString(rel) {
		return "<dir>"
	}
	return rel
}
