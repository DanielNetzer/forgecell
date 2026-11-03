package cli

import (
	"fmt"
	"io"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/scaffold"
)

// runPluginPin runs `plugin pin <dir> --server <path> [--interpreter <path>]`:
// it writes <dir>/plugin.json from <dir>/plugin.json.tmpl, pinned to the
// server's (and the interpreter's) resolved path and digest, then prints the
// manifest's own SHA-256 and the Formula's plugins: line. --interpreter is
// required for a python or typescript template and refused for a go one. It
// prints no absolute path. It exits 0 on success and 1 otherwise.
func runPluginPin(args []string, out, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "forgecell:", pluginPinUsage)
		return 1
	}
	dir, server, interpreter := "", "", ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--server":
			if server != "" || i+1 >= len(args) || args[i+1] == "" {
				return usage()
			}
			server = args[i+1]
			i++
		case arg == "--interpreter":
			if interpreter != "" || i+1 >= len(args) || args[i+1] == "" {
				return usage()
			}
			interpreter = args[i+1]
			i++
		case dir == "" && arg != "" && arg[0] != '-':
			dir = arg
		default:
			return usage()
		}
	}
	if dir == "" || server == "" {
		return usage()
	}
	var p scaffold.Pinned
	var err error
	if interpreter == "" {
		p, err = scaffold.Pin(dir, server)
	} else {
		p, err = scaffold.PinInterpreted(dir, server, interpreter)
	}
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	fmt.Fprintf(out, "Pinned %s for the plugin %s.\nManifest SHA-256: %s\n", scaffold.ManifestFile, p.Name, p.ManifestSHA256)
	fmt.Fprintf(out, "Put the plugin in your Lab as plugins/%s, then add this line under the Formula's plugins:\n", p.Name)
	fmt.Fprintf(out, "%s\n", pluginPinFormulaEntry(p.Name, p.ManifestSHA256))
	return 0
}

// pluginPinFormulaEntry is the Formula's plugins: list item for a plugin pinned
// as plugins/<name>/plugin.json with the manifest digest.
func pluginPinFormulaEntry(name, digest string) string {
	return fmt.Sprintf("  - {name: %s, manifest: plugins/%s/%s, sha256: \"%s\"}", name, name, scaffold.ManifestFile, digest)
}
