package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// tempDir returns the test's temporary directory with symlinks resolved, as
// pinned placement checks require.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// tree lists every path under root, relative to it, so a refusal can be shown
// to leave nothing behind.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func firedGrace() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// buildSkeleton builds the skeleton in dir into out with the Go toolchain,
// offline: no proxy, no toolchain download, no workspace, and a GOCACHE from
// the environment or the given cache directory.
func buildSkeleton(t *testing.T, dir, out, cache string) {
	t.Helper()
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goTool); err != nil {
		if goTool, err = exec.LookPath("go"); err != nil {
			t.Skip("no go toolchain")
		}
	}
	cmd := exec.Command(goTool, "build", "-trimpath", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=mod",
		"GOMODCACHE="+filepath.Join(cache, "gomodcache"))
	if os.Getenv("GOCACHE") == "" {
		cmd.Env = append(cmd.Env, "GOCACHE="+filepath.Join(cache, "gocache"))
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the skeleton failed: %v\n%s", err, output)
	}
}

func TestSkeletonPassesConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds each skeleton with go build")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	// One cache serves the three builds.
	cache := tempDir(t)
	for _, kind := range contract.Kinds {
		t.Run(kind, func(t *testing.T) {
			base := tempDir(t)
			name := "example-" + kind
			dir := filepath.Join(base, name)
			if err := Write(dir, Options{Kind: kind, Lang: "go", Name: name}); err != nil {
				t.Fatal(err)
			}
			server := filepath.Join(base, "bin", name)
			if err := os.Mkdir(filepath.Dir(server), 0o700); err != nil {
				t.Fatal(err)
			}
			buildSkeleton(t, dir, server, cache)
			p, err := Pin(dir, server)
			if err != nil {
				t.Fatal(err)
			}
			if p.Name != name || len(p.ManifestSHA256) != 64 {
				t.Fatalf("pinned %+v", p)
			}
			run := filepath.Join(base, "run")
			if err := os.Mkdir(run, 0o700); err != nil {
				t.Fatal(err)
			}
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
}

func TestSkeletonFilesFollowTheContract(t *testing.T) {
	for _, kind := range contract.Kinds {
		t.Run(kind, func(t *testing.T) {
			c, _ := contract.Lookup(kind)
			dir := filepath.Join(tempDir(t), "skeleton")
			if err := Write(dir, Options{Kind: kind, Lang: "go", Name: "my-" + kind}); err != nil {
				t.Fatal(err)
			}
			want := []string{".", "README.md", "fixture.json", "go.mod", "main.go", TemplateFile, "tools.json"}
			if got := tree(t, dir); !slices.Equal(got, want) {
				t.Fatalf("files %v, want %v", got, want)
			}
			info, err := os.Lstat(dir)
			if err != nil || info.Mode().Perm() != dirMode {
				t.Fatalf("directory mode %v, %v", info.Mode(), err)
			}
			for _, name := range want[1:] {
				if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != fileMode {
					t.Fatalf("%s mode %v, %v", name, info.Mode(), err)
				}
			}

			// fixture.json: the describe value decodes under the contract, and
			// every other tool has an empty object.
			var fixtures map[string]json.RawMessage
			readJSON(t, filepath.Join(dir, "fixture.json"), &fixtures)
			if len(fixtures) != len(c.Tools) {
				t.Fatalf("fixture.json has %d entries for %d tools", len(fixtures), len(c.Tools))
			}
			if _, err := c.Describe().Decode(fixtures[c.Describe().Name]); err != nil {
				t.Fatalf("the describe fixture does not decode: %v", err)
			}
			for _, tool := range c.Tools {
				if tool.Name != c.Describe().Name && string(compact(t, fixtures[tool.Name])) != "{}" {
					t.Fatalf("fixture for %s is %s", tool.Name, fixtures[tool.Name])
				}
			}

			// tools.json: every contract tool, in order, with its access and its
			// exact input schema.
			var table struct {
				Tools []toolEntry `json:"tools"`
			}
			readJSON(t, filepath.Join(dir, "tools.json"), &table)
			if len(table.Tools) != len(c.Tools) {
				t.Fatalf("tools.json lists %d tools for %d", len(table.Tools), len(c.Tools))
			}
			for i, entry := range table.Tools {
				tool := c.Tools[i]
				if entry.Name != tool.Name || entry.Access != tool.Access || string(compact(t, entry.InputSchema)) != string(compact(t, tool.InputSchema())) {
					t.Fatalf("tools.json entry %d is %+v", i, entry)
				}
				if err := tool.CheckAdvertised(entry.InputSchema); err != nil {
					t.Fatalf("tools.json schema for %s: %v", tool.Name, err)
				}
			}

			// plugin.json.tmpl: once its placeholders are filled it is a valid
			// manifest that lists every tool with its mapped access.
			raw, err := os.ReadFile(filepath.Join(dir, TemplateFile))
			if err != nil {
				t.Fatal(err)
			}
			filled := strings.NewReplacer(serverPath, "/opt/example/server", serverSHA256, strings.Repeat("a", 64)).Replace(string(raw))
			m, err := plugin.Decode([]byte(filled))
			if err != nil {
				t.Fatalf("the filled template does not decode: %v\n%s", err, filled)
			}
			if m.Kind != kind || m.Name != "my-"+kind || m.Contract != c.ID || len(m.Tools) != len(c.Tools) || m.Needs.MinPollSeconds != 60 ||
				len(m.Needs.Env) != 0 || len(m.Needs.Network) != 0 || len(m.Dependencies) != 0 || !strings.Contains(m.Limitations, "Skeleton") {
				t.Fatalf("manifest %+v", m)
			}
			for i, tool := range m.Tools {
				if tool.Name != c.Tools[i].Name || tool.Access != plugin.AccessFor(c.Tools[i].Access) {
					t.Fatalf("manifest tool %d is %+v", i, tool)
				}
			}

			// go.mod and main.go: no requirements and no lab import.
			goMod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			if string(goMod) != "module example.com/my-"+kind+"\n\ngo 1.22\n" {
				t.Fatalf("go.mod is %q", goMod)
			}
			mainGo, _ := os.ReadFile(filepath.Join(dir, "main.go"))
			if strings.Contains(string(mainGo), "forgecell/lab") || strings.Contains(string(mainGo), "{{") ||
				!strings.Contains(string(mainGo), `contractID      = "`+c.ID+`"`) {
				t.Fatal("main.go imports the lab, keeps a placeholder or lacks the contract id")
			}
			// Both say what the argument check covers and what a handler
			// must check itself.
			readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
			for _, doc := range []string{string(mainGo), string(readme)} {
				for _, want := range []string{"top-level required member", "unknown top-level member", "JSON type", "bounds", "enums", "patterns", "nested"} {
					if !strings.Contains(doc, want) {
						t.Fatalf("a skeleton document does not mention %q", want)
					}
				}
			}
		})
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteAdoptsAnEmptyDirectory(t *testing.T) {
	dir := filepath.Join(tempDir(t), "empty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, Options{Kind: "reviewer", Lang: "go", Name: "rv"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRefusals(t *testing.T) {
	good := Options{Kind: "tracker", Lang: "go", Name: "good"}
	for _, c := range []struct {
		name  string
		opts  Options
		setup func(t *testing.T, base string) string
		want  string
	}{
		{"unknown kind", Options{Kind: "wiki", Lang: "go", Name: "good"}, nil, "kind must be"},
		{"unknown lang", Options{Kind: "tracker", Lang: "rust", Name: "good"}, nil, "language must be go"},
		{"python not yet", Options{Kind: "tracker", Lang: "python", Name: "good"}, nil, "not supported yet"},
		{"typescript not yet", Options{Kind: "forge", Lang: "typescript", Name: "good"}, nil, "not supported yet"},
		{"bad name", Options{Kind: "tracker", Lang: "go", Name: "Bad_Name"}, nil, "plugin name must be"},
		{"long name", Options{Kind: "tracker", Lang: "go", Name: strings.Repeat("a", 33)}, nil, "plugin name must be"},
		{"non-empty dir", good, func(t *testing.T, base string) string {
			dir := filepath.Join(base, "full")
			mkdir(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "not empty"},
		{"symlinked dir", good, func(t *testing.T, base string) string {
			target := filepath.Join(base, "target")
			mkdir(t, target)
			link := filepath.Join(base, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, "symlink"},
		{"a file", good, func(t *testing.T, base string) string {
			file := filepath.Join(base, "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return file
		}, "directory you own"},
		{"missing parent", good, func(t *testing.T, base string) string {
			return filepath.Join(base, "missing", "dir")
		}, "parent is missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := tempDir(t)
			dir := filepath.Join(base, "new")
			if c.setup != nil {
				dir = c.setup(t, base)
			}
			before := tree(t, base)
			err := Write(dir, c.opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), base) {
				t.Fatalf("the error names a local path: %v", err)
			}
			if after := tree(t, base); !slices.Equal(before, after) {
				t.Fatalf("the refusal left files behind: %v, was %v", after, before)
			}
		})
	}
}

func TestWriteRemovesOnlyWhatItCreated(t *testing.T) {
	base := tempDir(t)
	// A repeated file name fails the second exclusive create after the first
	// file and the directory were made: both are removed again.
	dir := filepath.Join(base, "fresh")
	err := writeFiles(dir, []skeletonFile{{"a.txt", []byte("1")}, {"a.txt", []byte("2")}})
	if err == nil {
		t.Fatal("a repeated file name was written twice")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the created directory was left behind: %v", err)
	}
	// An adopted empty directory is kept, and only the created file goes.
	adopted := filepath.Join(base, "adopted")
	mkdir(t, adopted)
	if err := writeFiles(adopted, []skeletonFile{{"b.txt", nil}, {"b.txt", nil}}); err == nil {
		t.Fatal("a repeated file name was written twice")
	}
	if got := tree(t, adopted); !slices.Equal(got, []string{"."}) {
		t.Fatalf("the adopted directory holds %v", got)
	}
}

// TestSkeletonWireRefusesHostileInput builds the tracker skeleton once and
// pipes hostile stdin through it, checking the reply to each line in order. It
// runs under -short too; only a missing go toolchain skips it.
func TestSkeletonWireRefusesHostileInput(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	base := tempDir(t)
	dir := filepath.Join(base, "wire")
	if err := Write(dir, Options{Kind: "tracker", Lang: "go", Name: "wire"}); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(base, "server")
	buildSkeleton(t, dir, server, base)

	call := func(id, args string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"tracker_describe","arguments":` + args + `}}`
	}
	type want struct {
		id      string // the reply's id, as raw JSON
		code    int    // a JSON-RPC error code, or 0 for a result
		isError bool   // a tool result with isError and reason failed
		text    string // a substring of the reply
	}
	lines := []struct {
		name  string
		input string
		want  *want // nil: no reply
	}{
		{"over 1 MiB", `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 1<<20+16) + `"}}` + "\n", &want{id: "null", code: -32600, text: "size bound"}},
		{"served after the oversize message", `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n", &want{id: "2"}},
		{"CRLF line", `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\r\n", &want{id: "3"}},
		{"batch array", `[{"jsonrpc":"2.0","id":4,"method":"ping"}]` + "\n", &want{id: "null", code: -32600}},
		{"oversize id", `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 300) + `","method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"duplicate keys", `{"jsonrpc":"2.0","id":5,"id":6,"method":"ping"}` + "\n", &want{id: "null", code: -32600}},
		{"duplicate argument keys", call("7", `{"config":{},"config":{}}`) + "\n", &want{id: "null", code: -32600}},
		{"null arguments", call("8", "null") + "\n", &want{id: "8", isError: true}},
		{"array arguments", call("9", "[1]") + "\n", &want{id: "9", code: -32602}},
		{"wrong top-level type", call("10", `{"config":"x"}`) + "\n", &want{id: "10", isError: true}},
		{"unknown member", call("11", `{"config":{},"extra":1}`) + "\n", &want{id: "11", isError: true}},
		{"unknown notification", `{"jsonrpc":"2.0","method":"notifications/bogus","params":{}}` + "\n", nil},
		{"invalid UTF-8", "{\"jsonrpc\":\"2.0\",\"id\":12,\"method\":\"ping\",\"x\":\"\xff\"}\n", &want{id: "null", code: -32700}},
		{"describe", call("13", `{"config":{}}`) + "\n", &want{id: "13", text: `"keyPattern"`}},
		{"partial line at EOF", `{"jsonrpc":"2.0","id":14,"method":"ping"}`, nil},
	}
	var in strings.Builder
	for _, l := range lines {
		in.WriteString(l.input)
	}
	cmd := exec.Command(server)
	cmd.Dir = base
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(in.String())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the server failed: %v\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("the server wrote to stderr: %q", stderr.String())
	}
	replies := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	i := 0
	for _, l := range lines {
		if l.want == nil {
			continue
		}
		if i >= len(replies) {
			t.Fatalf("%s: no reply; replies: %q", l.name, replies)
		}
		raw := replies[i]
		i++
		var reply struct {
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
		if err := json.Unmarshal([]byte(raw), &reply); err != nil || reply.JSONRPC != "2.0" || string(reply.ID) != l.want.id {
			t.Fatalf("%s: reply %s, %v", l.name, raw, err)
		}
		switch {
		case l.want.code != 0:
			if reply.Error == nil || reply.Error.Code != l.want.code || reply.Result != nil {
				t.Fatalf("%s: want error %d, got %s", l.name, l.want.code, raw)
			}
		case l.want.isError:
			if reply.Result == nil || !reply.Result.IsError || reply.Result.StructuredContent.Reason != "failed" {
				t.Fatalf("%s: want an isError result with reason failed, got %s", l.name, raw)
			}
		default:
			if reply.Error != nil || reply.Result == nil || reply.Result.IsError {
				t.Fatalf("%s: want a result, got %s", l.name, raw)
			}
		}
		if !strings.Contains(raw, l.want.text) {
			t.Fatalf("%s: reply lacks %q: %s", l.name, l.want.text, raw)
		}
	}
	if i != len(replies) {
		t.Fatalf("%d extra replies: %q", len(replies)-i, replies[i:])
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
