package scaffold

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// interpLangs are the skeleton languages whose server runs through a pinned
// interpreter.
var interpLangs = []string{"python", "typescript"}

// interpFor finds lang's interpreter with exec.LookPath and resolves every
// symlink, as plugin pin requires. It skips the test, with the reason, unless
// the interpreter runs with the launcher's minimal environment, is new enough
// (python3 3.9, node 22.18 with stripTypeScriptTypes) and passes the engine's
// placement rules.
func interpFor(t *testing.T, lang string) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	name, check := "python3", []string{"-c", "import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)"}
	if lang == "typescript" {
		name, check = "node", []string{"-e", `const [a, b] = process.versions.node.split(".").map(Number);` +
			`process.exit((a > 22 || (a === 22 && b >= 18)) && typeof require("node:module").stripTypeScriptTypes === "function" ? 0 : 1)`}
	}
	found, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on PATH", name)
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil || !filepath.IsAbs(resolved) {
		t.Skipf("%s on PATH cannot be resolved to an absolute path", name)
	}
	cmd := exec.Command(resolved, check...)
	cmd.Dir = tempDir(t)
	cmd.Env = interpEnv(cmd.Dir)
	if err := cmd.Run(); err != nil {
		t.Skipf("%s does not run here or is too old (python3 3.9 or node 22.18 needed): %v", name, err)
	}
	if err := pinned.CheckPlacement(resolved); err != nil {
		t.Skipf("%s is not placed where the engine's rules trust it", name)
	}
	return resolved
}

// interpEnv is the launcher's minimal environment, with home as HOME.
func interpEnv(home string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LANG=C"}
}

// interpScript writes the skeleton for lang and kind into base and returns its
// directory and the script plugin pin takes: server.py, or the server.js that
// `node build.mjs` builds.
func interpScript(t *testing.T, base, lang, kind, name, interp string) (dir, script string) {
	t.Helper()
	dir = filepath.Join(base, name)
	if err := Write(dir, Options{Kind: kind, Lang: lang, Name: name}); err != nil {
		t.Fatal(err)
	}
	if lang == "python" {
		return dir, filepath.Join(dir, "server.py")
	}
	cmd := exec.Command(interp, "build.mjs")
	cmd.Dir = dir
	cmd.Env = interpEnv(tempDir(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node build.mjs failed: %v\n%s", err, out)
	}
	script = filepath.Join(dir, "server.js")
	if info, err := os.Lstat(script); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("server.js was not built as a private regular file: %v", err)
	}
	return dir, script
}

func TestInterpretedSkeletonsPassConformance(t *testing.T) {
	for _, lang := range interpLangs {
		t.Run(lang, func(t *testing.T) {
			interp := interpFor(t, lang)
			for _, kind := range contract.Kinds {
				t.Run(kind, func(t *testing.T) {
					base := tempDir(t)
					name := lang[:2] + "-" + kind
					dir, script := interpScript(t, base, lang, kind, name, interp)
					p, err := PinInterpreted(dir, script, interp)
					if err != nil {
						t.Fatal(err)
					}
					m, digest, err := plugin.LoadFile(filepath.Join(dir, ManifestFile))
					if err != nil || p.Name != name || p.ManifestSHA256 != digest ||
						m.Server.Interpreter == nil || m.Server.Interpreter.Path != interp || m.Server.Executable.Path != script {
						t.Fatalf("pinned %+v, manifest server %+v, %v", p, m.Server, err)
					}
					run := filepath.Join(base, "run")
					mkdir(t, run)
					report := conformance.Run(context.Background(), filepath.Join(dir, ManifestFile), conformance.Options{
						Root:      run,
						LookupEnv: func(string) (string, bool) { return "", false },
						Grace:     firedGrace,
					})
					if !report.Passed || report.ManifestSHA256 != p.ManifestSHA256 || report.Contract != contract.ID(kind) {
						raw, _ := json.MarshalIndent(report, "", "  ")
						t.Fatalf("conformance did not pass:\n%s", raw)
					}
					if left, _ := os.ReadDir(run); len(left) != 0 {
						t.Fatalf("the run left %d entries behind", len(left))
					}
				})
			}
		})
	}
}

func TestInterpretedSkeletonFiles(t *testing.T) {
	for _, lang := range interpLangs {
		for _, kind := range contract.Kinds {
			t.Run(lang+"/"+kind, func(t *testing.T) {
				c, _ := contract.Lookup(kind)
				base := tempDir(t)
				name := "my-" + kind
				dir := filepath.Join(base, "skeleton")
				if err := Write(dir, Options{Kind: kind, Lang: lang, Name: name}); err != nil {
					t.Fatal(err)
				}
				script := "server.py"
				want := []string{".", "README.md", "fixture.json", TemplateFile, script, "tools.json"}
				if lang == "typescript" {
					script = "server.ts"
					want = []string{".", "README.md", "build.mjs", "fixture.json", TemplateFile, script, "tools.json"}
				}
				if got := tree(t, dir); !slices.Equal(got, want) {
					t.Fatalf("files %v, want %v", got, want)
				}
				for _, f := range want[1:] {
					if info, err := os.Lstat(filepath.Join(dir, f)); err != nil || info.Mode().Perm() != fileMode {
						t.Fatalf("%s mode %v, %v", f, info.Mode(), err)
					}
				}
				read := func(f string) string {
					raw, err := os.ReadFile(filepath.Join(dir, f))
					if err != nil {
						t.Fatal(err)
					}
					return string(raw)
				}

				// tools.json and fixture.json are the bytes the Go skeleton holds.
				tools, _ := toolsJSON(c)
				fixture, _ := fixtureJSON(c, name)
				if read("tools.json") != string(tools) || read("fixture.json") != string(fixture) {
					t.Fatal("tools.json or fixture.json differs from the Go skeleton's")
				}

				// plugin.json.tmpl is the Go skeleton's template plus one
				// server.interpreter line, and decodes once all four
				// placeholders are filled.
				tmpl := read(TemplateFile)
				goTmpl, _ := manifestTemplate(c, name)
				line := `    "interpreter": {"path": "{{INTERPRETER_PATH}}", "sha256": "{{INTERPRETER_SHA256}}"},` + "\n"
				if strings.Count(tmpl, line) != 1 || strings.Replace(tmpl, line, "", 1) != string(goTmpl) {
					t.Fatalf("the template is not the Go template plus the interpreter line:\n%s", tmpl)
				}
				filled := strings.NewReplacer(serverPath, "/opt/example/server.js", serverSHA256, strings.Repeat("a", 64),
					interpreterPath, "/usr/bin/example", interpreterSHA256, strings.Repeat("b", 64)).Replace(tmpl)
				m, err := plugin.Decode([]byte(filled))
				if err != nil || m.Server.Interpreter == nil || m.Server.Interpreter.Path != "/usr/bin/example" || m.Name != name {
					t.Fatalf("the filled template: %+v, %v", m.Server, err)
				}

				// The script embeds the table, reads no file and imports only
				// the standard library.
				src := read(script)
				if strings.Contains(src, "{{") || !strings.Contains(src, `"`+c.ID+`"`) || !strings.Contains(src, `"`+c.Describe().Name+`"`) {
					t.Fatal("the script keeps a placeholder or lacks the contract id or the describe tool")
				}
				for _, l := range strings.Split(src, "\n") {
					trimmed := strings.TrimSpace(l)
					switch lang {
					case "python":
						if (strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ")) && trimmed != "import json" && trimmed != "import sys" {
							t.Fatalf("server.py imports more than json and sys: %q", l)
						}
						if strings.Contains(l, "open(") {
							t.Fatalf("server.py opens a file: %q", l)
						}
					case "typescript":
						for _, banned := range []string{"import ", "export ", "enum ", "namespace ", "require(", "readFile", "private ", "public ", "protected "} {
							if strings.HasPrefix(trimmed, banned) || (banned == "require(" || banned == "readFile") && strings.Contains(l, banned) {
								t.Fatalf("server.ts uses %q: %q", banned, l)
							}
						}
					}
				}
				for _, doc := range []string{src, read("README.md")} {
					for _, want := range []string{"top-level required member", "unknown top-level member", "JSON type", "bool is not an integer", "bounds", "enums", "patterns", "nested"} {
						if !strings.Contains(doc, want) {
							t.Fatalf("a skeleton document does not mention %q", want)
						}
					}
				}
			})
		}
	}
}

// TestGoSkeletonIsByteIdentical pins every file of the Go skeleton, for fixed
// inputs, to its SHA-256 as slice 6a wrote it.
func TestGoSkeletonIsByteIdentical(t *testing.T) {
	want := map[string]string{
		"tracker/README.md":         "24f398a470d47b741cdfc4123b4595737feca2ded276864c1a02d7a2a3c79def",
		"tracker/fixture.json":      "58a87405a1aabccff41579eb664dcd5aeb1a7ffecfdd60b2260bcd768080aefa",
		"tracker/go.mod":            "d40bb225ee099817b4d986475995ee6681eddef32a46c55b12cc606465348649",
		"tracker/main.go":           "bdb9044eae692bfcd932bdec6dc4863f3051cf16369264e84f7a37a1a4217597",
		"tracker/plugin.json.tmpl":  "3472c4411f8ca9d36e06fadae0adc91a8d20c9b644df9997c22425c877f15cb6",
		"tracker/tools.json":        "dc5ef221c3aa39cba5ce08400b0a24d1fb8b9947c2dea83d550de2ed71b72b87",
		"forge/README.md":           "7705eed9442a1e5fedc8166a434c0576538315fde05913800c495d8026f792a5",
		"forge/fixture.json":        "63eab287edf4b427af2fbb9dd75e65d738836826e7b5557234573aaa100353b2",
		"forge/go.mod":              "ecc815e9012ac642bd5cd56fe57b2fd58550ff0abf7e73f30f15fea877836401",
		"forge/main.go":             "041090aec0de96403bb48897e6aa7385aef42c117e918575dfe5c0736107924b",
		"forge/plugin.json.tmpl":    "5efc663e2c445e93942b4db8d2a801dd412971da621dc6ee1c9b354c4ca799ca",
		"forge/tools.json":          "1112ceb61726228c3ffa2011f400df901f1397c131a9bb0856007053eac8dca6",
		"reviewer/README.md":        "c5c5db093b8c00dd930175a153bd718cd699b7f2404907af4d1e23cfa287b8f9",
		"reviewer/fixture.json":     "7240275607175e07e3194b8d504c311b12ed366458de0ce3dee438b9d6be5f0a",
		"reviewer/go.mod":           "fc1ccc378d12de9b88ab6231ca57bed7bd87a736fc7aa65729434095209e80aa",
		"reviewer/main.go":          "d400690099e095e77cb88ee97b34879d06581f3ea374c1b78bd2767460cfcf52",
		"reviewer/plugin.json.tmpl": "6731514edecc9daa18bf96a78db37f40c01467c2465592c9d372169120744f6b",
		"reviewer/tools.json":       "ed13a5946ded0e0a054a438a1c9ca48bf8b1554a135ad821734552bd69798288",
	}
	for _, kind := range contract.Kinds {
		dir := filepath.Join(tempDir(t), "golden")
		if err := Write(dir, Options{Kind: kind, Lang: "go", Name: "golden-" + kind}); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"README.md", "fixture.json", "go.mod", "main.go", TemplateFile, "tools.json"} {
			raw, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if got := sha(raw); got != want[kind+"/"+f] {
				t.Errorf("%s/%s: SHA-256 %s, want %q", kind, f, got, want[kind+"/"+f])
			}
		}
	}
}

func TestWriteNamesEveryLanguage(t *testing.T) {
	if err := Write(filepath.Join(tempDir(t), "x"), Options{Kind: "tracker", Lang: "rust", Name: "good"}); err == nil || err.Error() != "the language must be go, python or typescript" {
		t.Fatalf("got %v", err)
	}
}

// interpFixture writes a skeleton and stand-ins for its script and its
// interpreter. Pinning only hashes them, so neither has to run.
func interpFixture(t *testing.T, lang string) (base, dir, script, interp string) {
	t.Helper()
	base = tempDir(t)
	dir = filepath.Join(base, "skel")
	if err := Write(dir, Options{Kind: "tracker", Lang: lang, Name: "interp-me"}); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "server.py")
	if lang != "python" {
		script = filepath.Join(base, "script")
		if err := os.WriteFile(script, []byte("#!/bin/false\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	interp = filepath.Join(base, "interp")
	if err := os.WriteFile(interp, []byte("#!/bin/false\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return base, dir, script, interp
}

func TestPinInterpretedWritesALoadableManifest(t *testing.T) {
	base, dir, script, interp := interpFixture(t, "python")
	// server.py has no exec bit, which a script run by the interpreter needs
	// none of.
	if info, err := os.Lstat(script); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("server.py mode %v, %v", info.Mode(), err)
	}
	p, err := PinInterpreted(dir, script, interp)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ManifestFile)
	raw, _ := os.ReadFile(path)
	m, digest, err := plugin.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scriptBytes, _ := os.ReadFile(script)
	interpBytes, _ := os.ReadFile(interp)
	if p.Name != "interp-me" || p.ManifestSHA256 != digest || digest != sha(raw) ||
		m.Server.Executable.Path != script || m.Server.Executable.SHA256 != sha(scriptBytes) ||
		m.Server.Interpreter == nil || m.Server.Interpreter.Path != interp || m.Server.Interpreter.SHA256 != sha(interpBytes) {
		t.Fatalf("pinned %+v, manifest server %+v", p, m.Server)
	}
	if strings.Contains(string(raw), "{{") {
		t.Fatal("a placeholder is left in plugin.json")
	}

	// Pin without the interpreter refuses the template with the interpreter
	// reason, for a script without its exec bit (the template is read before
	// the server is hashed) and with it, and leaves plugin.json as it was.
	before := tree(t, base)
	for _, mode := range []os.FileMode{0o600, 0o700} {
		chmod(t, script, mode)
		if _, err := Pin(dir, script); err == nil || !strings.Contains(err.Error(), "pins an interpreter; give its path with --interpreter") || strings.Contains(err.Error(), base) {
			t.Fatalf("Pin without the interpreter, script mode %v: %v", mode, err)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(raw) || !slices.Equal(tree(t, base), before) {
		t.Fatal("a refused pin changed the files")
	}
}

// pinArgs is a refusal case's fixture and the arguments it pins with.
type pinArgs struct{ base, dir, script, interp string }

func TestPinInterpretedRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		lang  string
		setup func(t *testing.T, a *pinArgs) // edits the files or the arguments
		want  string
	}{
		{"interpreter given for go", "go", func(*testing.T, *pinArgs) {}, "pins no interpreter"},
		{"group-writable interpreter", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.interp, 0o720) }, "interpreter is group- or world-writable"},
		{"world-writable interpreter", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.interp, 0o702) }, "interpreter is group- or world-writable"},
		{"directory interpreter", "python", func(t *testing.T, a *pinArgs) {
			a.interp = filepath.Join(a.base, "sub")
			mkdir(t, a.interp)
		}, "interpreter must be a regular file"},
		{"symlinked interpreter", "python", func(t *testing.T, a *pinArgs) {
			link := filepath.Join(a.base, "interp-link")
			if err := os.Symlink(a.interp, link); err != nil {
				t.Fatal(err)
			}
			a.interp = link
		}, "interpreter is a symlink"},
		{"missing interpreter", "python", func(t *testing.T, a *pinArgs) { a.interp = filepath.Join(a.base, "absent") }, "interpreter is missing"},
		{"empty interpreter", "python", func(t *testing.T, a *pinArgs) { a.interp = "" }, "interpreter path is empty"},
		{"interpreter without exec bit", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.interp, 0o600) }, "interpreter is not executable"},
		{"quote in interpreter path", "python", func(t *testing.T, a *pinArgs) { a.interp = writeServer(t, a.base, `in"terp`) }, `interpreter path must not contain "`},
		{"hard-linked interpreter", "python", func(t *testing.T, a *pinArgs) {
			if err := os.Link(a.interp, filepath.Join(a.base, "interp-alias")); err != nil {
				t.Fatal(err)
			}
		}, "interpreter has more than one hard link"},
		{"group-writable script", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.script, 0o620) }, "server is group- or world-writable"},
		{"interpreter placeholder missing", "typescript", func(t *testing.T, a *pinArgs) {
			editTemplate(t, a.dir, func(s string) string { return strings.Replace(s, interpreterSHA256, strings.Repeat("b", 64), 1) })
		}, "exactly once"},
		{"placeholder left over", "typescript", func(t *testing.T, a *pinArgs) {
			editTemplate(t, a.dir, func(s string) string { return strings.Replace(s, `"roles": []`, `"roles": ["{{ROLE}}"]`, 1) })
		}, "placeholder left over"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a pinArgs
			a.base, a.dir, a.script, a.interp = interpFixture(t, c.lang)
			c.setup(t, &a)
			before := tree(t, a.base)
			_, err := PinInterpreted(a.dir, a.script, a.interp)
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), a.base) {
				t.Fatalf("got %v, want %q and no local path", err, c.want)
			}
			if after := tree(t, a.base); !slices.Equal(before, after) {
				t.Fatalf("the refusal changed the files: %v, was %v", after, before)
			}
		})
	}
}

// TestInterpretedWireRefusesHostileInput pipes hostile stdin through the
// tracker skeleton's script, run as the launcher runs it (a private copy with
// no extension), and checks the reply to each line in order.
func TestInterpretedWireRefusesHostileInput(t *testing.T) {
	call := func(id, tool, args string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}` + "\n"
	}
	list := func(id, limit string) string {
		return call(id, "tracker_list", `{"config":{},"selector":"oldest","limit":`+limit+`}`)
	}
	type want = interpWant
	lines := []interpLine{
		{"over 1 MiB", `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 1<<20+16) + `"}}` + "\n", &want{id: "null", code: -32600, text: "size bound"}},
		{"served after the oversize message", `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n", &want{id: "2"}},
		{"CRLF line", `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\r\n", &want{id: "3"}},
		{"batch array", `[{"jsonrpc":"2.0","id":4,"method":"ping"}]` + "\n", &want{id: "null", code: -32600}},
		{"oversize id", `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 300) + `","method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"fractional id", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"bool id", `{"jsonrpc":"2.0","id":true,"method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"duplicate keys", `{"jsonrpc":"2.0","id":5,"id":6,"method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"duplicate argument keys", call("7", "tracker_describe", `{"config":{},"config":{}}`), &want{id: "null", code: -32600}},
		{"NaN", `{"jsonrpc":"2.0","id":8,"method":"ping","x":NaN}` + "\n", &want{id: "null", code: -32700}},
		{"invalid UTF-8", "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"ping\",\"x\":\"\xff\"}\n", &want{id: "null", code: -32700}},
		{"null arguments", call("10", "tracker_describe", "null"), &want{id: "10", isError: true}},
		{"array arguments", call("11", "tracker_describe", "[1]"), &want{id: "11", code: -32602}},
		{"params not an object", `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":[1]}` + "\n", &want{id: "12", code: -32602}},
		{"unknown tool", call("13", "tracker_nope", "{}"), &want{id: "13", code: -32602}},
		{"inherited name as tool", call("14", "constructor", "{}"), &want{id: "14", code: -32602}},
		{"unknown method", `{"jsonrpc":"2.0","id":15,"method":"bogus"}` + "\n", &want{id: "15", code: -32601}},
		{"wrong top-level type", call("16", "tracker_describe", `{"config":"x"}`), &want{id: "16", isError: true}},
		{"unknown member", call("17", "tracker_describe", `{"config":{},"extra":1}`), &want{id: "17", isError: true}},
		{"inherited name as member", call("18", "tracker_describe", `{"config":{},"toString":1}`), &want{id: "18", isError: true}},
		{"bool for an integer", list("19", "true"), &want{id: "19", isError: true, text: "do not match"}},
		{"fraction for an integer", list("20", "1.5"), &want{id: "20", isError: true, text: "do not match"}},
		{"1.0 for an integer", list("21", "1.0"), &want{id: "21", isError: true, text: "do not match"}},
		{"integer accepted", list("22", "5"), &want{id: "22", isError: true, text: "not implemented yet: tracker_list"}},
		{"missing required member", call("23", "tracker_list", `{"config":{}}`), &want{id: "23", isError: true, text: "do not match"}},
		{"unknown notification", `{"jsonrpc":"2.0","method":"notifications/bogus","params":{}}` + "\n", nil},
		{"initialize", `{"jsonrpc":"2.0","id":24,"method":"initialize","params":{}}` + "\n", &want{id: "24", text: `"` + contract.ID("tracker") + `"`}},
		{"tools/list", `{"jsonrpc":"2.0","id":25,"method":"tools/list"}` + "\n", &want{id: "25", text: `"tracker_list"`}},
		{"describe", call("26", "tracker_describe", `{"config":{}}`), &want{id: "26", text: `"keyPattern"`}},
		{"lone surrogate id", `{"jsonrpc":"2.0","id":"\ud800","method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"served after the surrogate id", `{"jsonrpc":"2.0","id":28,"method":"ping"}` + "\n", &want{id: "28"}},
		{"byte order mark", "\xef\xbb\xbf" + `{"jsonrpc":"2.0","id":29,"method":"ping"}` + "\n", &want{id: "null", code: -32700}},
		{"no-break space", " " + `{"jsonrpc":"2.0","id":30,"method":"ping"}` + "\n", &want{id: "null", code: -32700}},
		{"partial line at EOF", `{"jsonrpc":"2.0","id":27,"method":"ping"}`, nil},
	}
	// Python alone refuses numbers it would parse slowly or could not write.
	pyOnly := []interpLine{
		{"integer literal over 20 characters", list("31", "1"+strings.Repeat("0", 25)), &want{id: "null", code: -32700}},
		{"integer id over 20 characters", `{"jsonrpc":"2.0","id":123456789012345678901,"method":"ping"}` + "\n", &want{id: "null", code: -32700}},
		{"non-finite float", list("32", "1e999"), &want{id: "null", code: -32700}},
		{"served after the numbers", `{"jsonrpc":"2.0","id":33,"method":"ping"}` + "\n", &want{id: "33"}},
	}
	for _, lang := range interpLangs {
		t.Run(lang, func(t *testing.T) {
			all := lines
			if lang == "python" {
				all = append(slices.Clone(pyOnly), lines...)
			}
			var in strings.Builder
			for _, l := range all {
				in.WriteString(l.input)
			}
			checkReplies(t, wireRun(t, lang, in.String()), all)
		})
	}
}

// TestIDsAreMeasuredByTheirRawText: all three skeletons bound an id by its JSON
// text as written, so 60 "A"s each written as a backslash-u0041 are too long.
func TestIDsAreMeasuredByTheirRawText(t *testing.T) {
	plain := `"` + strings.Repeat("A", 60) + `"`
	lines := []interpLine{
		{"escaped id", `{"jsonrpc":"2.0","id":"` + strings.Repeat(`\`+"u0041", 60) + `","method":"ping"}` + "\n", &interpWant{id: "null", code: -32600}},
		{"plain id", `{"jsonrpc":"2.0","id":` + plain + `,"method":"ping"}` + "\n", &interpWant{id: plain}},
	}
	in := lines[0].input + lines[1].input
	for _, lang := range append([]string{"go"}, interpLangs...) {
		t.Run(lang, func(t *testing.T) { checkReplies(t, wireRun(t, lang, in), lines) })
	}
}

// TestInterpretedServersKeepUpWithManyRequests pipes 2000 pings in one write and
// wants every reply and no stderr (such as Node's MaxListenersExceededWarning).
func TestInterpretedServersKeepUpWithManyRequests(t *testing.T) {
	for _, lang := range interpLangs {
		t.Run(lang, func(t *testing.T) {
			replies := wireRun(t, lang, strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n", 2000))
			if len(replies) != 2000 || replies[1999] != `{"jsonrpc":"2.0","id":1,"result":{}}` {
				t.Fatalf("%d replies, the last %q", len(replies), replies[len(replies)-1])
			}
		})
	}
}

// wireRun runs the tracker skeleton for lang as the launcher runs it (for
// python and typescript, a private copy of the script with no extension),
// writes input to its stdin in one write and returns its reply lines. It fails
// the test when the server fails or writes to stderr.
func wireRun(t *testing.T, lang, input string) []string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	base := tempDir(t)
	mkdir(t, filepath.Join(base, "bin"))
	server := filepath.Join(base, "bin", "server")
	argv := []string{server}
	if lang == "go" {
		dir := filepath.Join(base, "wire")
		if err := Write(dir, Options{Kind: "tracker", Lang: "go", Name: "wire"}); err != nil {
			t.Fatal(err)
		}
		buildSkeleton(t, dir, server, base)
	} else {
		interp := interpFor(t, lang)
		_, script := interpScript(t, base, lang, "tracker", "wire", interp)
		raw, err := os.ReadFile(script)
		if err == nil {
			err = os.WriteFile(server, raw, 0o400)
		}
		if err != nil {
			t.Fatal(err)
		}
		argv = []string{interp, server}
	}
	home := filepath.Join(base, "home")
	mkdir(t, home)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = home, interpEnv(home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		stdin.Write([]byte(input))
		stdin.Close()
	}()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("the server failed (%v) or wrote to stderr: %q", err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// interpWant is the reply a wire line wants.
type interpWant struct {
	id      string // the reply's id, as raw JSON
	code    int    // a JSON-RPC error code, or 0 for a result
	isError bool   // a tool result with isError and reason failed
	text    string // a substring of the reply
}

// interpLine is one line of wire input and the reply it wants (nil: none).
type interpLine struct {
	name, input string
	want        *interpWant
}

// checkReplies matches each reply, in order, to the line that wants one.
func checkReplies(t *testing.T, replies []string, lines []interpLine) {
	t.Helper()
	i := 0
	for _, l := range lines {
		name, exp := l.name, l.want
		if exp == nil {
			continue
		} else if i >= len(replies) {
			t.Fatalf("%s: no reply; replies: %q", name, replies)
		}
		raw := replies[i]
		i++
		var got struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *struct {
				Code int `json:"code"`
			} `json:"error"`
			Result *struct {
				IsError           bool `json:"isError"`
				StructuredContent struct {
					Reason string `json:"reason"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil || got.JSONRPC != "2.0" || string(got.ID) != exp.id {
			t.Fatalf("%s: reply %s, %v", name, raw, err)
		}
		switch {
		case exp.code != 0:
			if got.Error == nil || got.Error.Code != exp.code || got.Result != nil {
				t.Fatalf("%s: want error %d, got %s", name, exp.code, raw)
			}
		case exp.isError:
			if got.Result == nil || !got.Result.IsError || got.Result.StructuredContent.Reason != "failed" {
				t.Fatalf("%s: want an isError result with reason failed, got %s", name, raw)
			}
		default:
			if got.Error != nil || got.Result == nil || got.Result.IsError {
				t.Fatalf("%s: want a result, got %s", name, raw)
			}
		}
		if !strings.Contains(raw, exp.text) {
			t.Fatalf("%s: reply lacks %q: %s", name, exp.text, raw)
		}
	}
	if i != len(replies) {
		t.Fatalf("%d extra replies: %q", len(replies)-i, replies[i:])
	}
}
