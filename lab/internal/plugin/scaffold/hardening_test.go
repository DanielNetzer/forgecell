package scaffold

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// withAfterTempWrite sets the pin seam for one test.
func withAfterTempWrite(t *testing.T, hook func(root *os.Root, tmp string)) {
	t.Helper()
	afterTempWrite = hook
	t.Cleanup(func() { afterTempWrite = nil })
}

func TestPinDigestIsOfTheFinalManifest(t *testing.T) {
	_, dir, server := pinFixture(t)
	p, err := Pin(dir, server)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil || p.ManifestSHA256 != sha(raw) {
		t.Fatalf("pinned %s, plugin.json hashes to %s: %v", p.ManifestSHA256, sha(raw), err)
	}

	// PinInterpreted returns the digest of the bytes it renamed too. Pin only
	// hashes the interpreter, so a stand-in file serves.
	base := tempDir(t)
	pdir := filepath.Join(base, "py")
	if err := Write(pdir, Options{Kind: "tracker", Lang: "python", Name: "py"}); err != nil {
		t.Fatal(err)
	}
	interp := writeServer(t, base, "python3")
	p, err = PinInterpreted(pdir, filepath.Join(pdir, "server.py"), interp)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(pdir, ManifestFile))
	if err != nil || p.ManifestSHA256 != sha(raw) {
		t.Fatalf("pinned %s, plugin.json hashes to %s: %v", p.ManifestSHA256, sha(raw), err)
	}
}

// TestPinRefusesASwappedTempManifest changes the temporary manifest between
// its write and its read-back through the seam, and wants each change refused
// with plugin.json and the directory left as they were.
func TestPinRefusesASwappedTempManifest(t *testing.T) {
	for _, c := range []struct {
		name string
		swap func(t *testing.T, root *os.Root, path string)
		want string
	}{
		{"replaced by an identical file", func(t *testing.T, root *os.Root, path string) {
			raw := readFile(t, path)
			removeFile(t, path)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "changed before it was read back"},
		{"replaced by a symlink", func(t *testing.T, root *os.Root, path string) {
			removeFile(t, path)
			if err := os.Symlink(filepath.Join(filepath.Dir(path), TemplateFile), path); err != nil {
				t.Fatal(err)
			}
		}, "changed before it was read back"},
		{"rewritten in place", func(t *testing.T, root *os.Root, path string) {
			raw := readFile(t, path)
			if err := os.WriteFile(path, bytes.Replace(raw, []byte(`"pin-me"`), []byte(`"pin-it"`), 1), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "changed before it was read back"},
		{"grown past the manifest bound", func(t *testing.T, root *os.Root, path string) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write(bytes.Repeat([]byte(" "), extension.MaxManifestBytes+1)); err != nil {
				t.Fatal(err)
			}
		}, "exceeds the manifest bound"},
		{"made group-writable", func(t *testing.T, root *os.Root, path string) {
			chmod(t, path, 0o620)
		}, "group- or world-writable"},
		{"hard-linked", func(t *testing.T, root *os.Root, path string) {
			if err := os.Link(path, filepath.Join(filepath.Dir(filepath.Dir(path)), "alias")); err != nil {
				t.Fatal(err)
			}
		}, "more than one hard link"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, dir, server := pinFixture(t)
			if _, err := Pin(dir, server); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, ManifestFile)
			before := readFile(t, manifest)
			files := tree(t, dir)
			// A rebuilt server, so the refused pin would have changed plugin.json.
			if err := os.WriteFile(server, []byte("#!/bin/true\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			called := false
			withAfterTempWrite(t, func(root *os.Root, tmp string) {
				called = true
				c.swap(t, root, filepath.Join(dir, tmp))
			})
			_, err := Pin(dir, server)
			if !called {
				t.Fatal("the seam did not run")
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), base) {
				t.Fatalf("the error names a local path: %v", err)
			}
			if after := readFile(t, manifest); !bytes.Equal(after, before) {
				t.Fatal("a refused pin changed plugin.json")
			}
			if after := tree(t, dir); !slices.Equal(after, files) {
				t.Fatalf("the refusal left %v, was %v", after, files)
			}
		})
	}
}

func TestWriteRefusesAGroupOrWorldWritableAdoptedDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o707, 0o720, 0o702} {
		t.Run(mode.String(), func(t *testing.T) {
			base := tempDir(t)
			dir := filepath.Join(base, "open")
			mkdir(t, dir)
			chmod(t, dir, mode)
			before := tree(t, base)
			err := Write(dir, Options{Kind: "tracker", Lang: "go", Name: "open"})
			if err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), base) {
				t.Fatalf("the error names a local path: %v", err)
			}
			if after := tree(t, base); !slices.Equal(after, before) {
				t.Fatalf("the refusal left %v, was %v", after, before)
			}
			if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("the directory is now %v, %v", info.Mode(), err)
			}
		})
	}
	for _, mode := range []os.FileMode{0o700, 0o755} {
		dir := filepath.Join(tempDir(t), "private")
		mkdir(t, dir)
		chmod(t, dir, mode)
		if err := Write(dir, Options{Kind: "tracker", Lang: "go", Name: "private"}); err != nil {
			t.Fatalf("a %v directory was refused: %v", mode, err)
		}
	}
}

func TestGoBuildCommandIgnoresGoWorkAndGoFlags(t *testing.T) {
	got := GoBuildCommand("./demo", "demo")
	if !slices.Equal(got, []string{`GOWORK=off GOFLAGS= \`, "go -C ./demo build -o demo ."}) {
		t.Fatalf("build command %q", got)
	}
	dir := filepath.Join(tempDir(t), "built")
	if err := Write(dir, Options{Kind: "forge", Lang: "go", Name: "built"}); err != nil {
		t.Fatal(err)
	}
	readme := string(readFile(t, filepath.Join(dir, "README.md")))
	for _, want := range []string{GoBuildEnv + " go build -o built .", "GOWORK=off", "go.work", "GOFLAGS"} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README.md lacks %q", want)
		}
	}
}

// TestReadmeFormulaSnippetParses parses every skeleton README's Formula
// snippet, for every kind and language, with the real Formula parser.
func TestReadmeFormulaSnippetParses(t *testing.T) {
	for _, lang := range append([]string{"go"}, interpLangs...) {
		for _, kind := range contract.Kinds {
			t.Run(lang+"/"+kind, func(t *testing.T) {
				name := "my-" + kind
				dir := filepath.Join(tempDir(t), name)
				if err := Write(dir, Options{Kind: kind, Lang: lang, Name: name}); err != nil {
					t.Fatal(err)
				}
				snippets, err := formulaSnippets(string(readFile(t, filepath.Join(dir, "README.md"))))
				if err != nil || len(snippets) != 1 {
					t.Fatalf("%d snippets, %v", len(snippets), err)
				}
				raw, err := snippetFormula(snippets[0])
				if err != nil {
					t.Fatal(err)
				}
				f, err := formula.Parse(raw)
				if err != nil {
					t.Fatalf("the snippet does not parse: %v\n%s", err, snippets[0])
				}
				if len(f.Plugins) != 1 || f.Plugins[0].Name != name {
					t.Fatalf("plugins %+v", f.Plugins)
				}
			})
		}
	}
}

func TestGoSkeletonDocumentsReasonsAndLimits(t *testing.T) {
	for _, kind := range contract.Kinds {
		c, _ := contract.Lookup(kind)
		dir := filepath.Join(tempDir(t), "doc")
		if err := Write(dir, Options{Kind: kind, Lang: "go", Name: "doc"}); err != nil {
			t.Fatal(err)
		}
		mainGo := readFile(t, filepath.Join(dir, "main.go"))
		if formatted, err := format.Source(mainGo); err != nil || !bytes.Equal(formatted, mainGo) {
			t.Fatalf("%s: main.go is not gofmt-clean: %v", kind, err)
		}
		for _, tool := range c.Tools {
			if want := fmt.Sprintf("{%q, %d, %d},", tool.Name, tool.MaxOutputBytes, tool.TimeoutMS); !bytes.Contains(mainGo, []byte(want)) {
				t.Fatalf("%s: main.go lacks the limits %s", kind, want)
			}
		}
		readme := string(readFile(t, filepath.Join(dir, "README.md")))
		for _, want := range []string{"auth-missing", "rate-limited", "network", "`failed`", "fail(reason, message)", "maxOutputBytes", "timeoutMs"} {
			if !strings.Contains(readme, want) {
				t.Fatalf("%s: README.md lacks %q", kind, want)
			}
		}
	}
	guide := readGuide(t)
	for _, want := range []string{"GOWORK=off GOFLAGS= go -C ./demo-tracker build", "go.work", "fail(reason, message)", "`authMissing`"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("the plugin guide lacks %q", want)
		}
	}
}

// skeletonHooks is a file added to a built forge skeleton: handlers that
// exercise each reason, the output bound and the timeout. The handler context
// seam records each call's timeout and lets forge_describe end its own context
// and then never return, as a hung handler past its timeout does.
const skeletonHooks = `package main

import (
	"context"
	"strings"
	"time"
)

var lastTimeout time.Duration

func init() {
	var cancelCurrent context.CancelFunc
	handlerContext = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		lastTimeout = d
		c, cancel := context.WithCancel(ctx)
		cancelCurrent = cancel
		return c, cancel
	}
	handlers["forge_policy"] = func(context.Context, map[string]any) (any, error) { return nil, fail(authMissing, "no token") }
	handlers["forge_find_change"] = func(context.Context, map[string]any) (any, error) { return nil, fail(rateLimited, "slow down") }
	handlers["forge_change"] = func(context.Context, map[string]any) (any, error) { return nil, fail(network, "unreachable") }
	handlers["forge_checks"] = func(context.Context, map[string]any) (any, error) { return nil, fail("bogus", "odd") }
	handlers["forge_comments"] = func(context.Context, map[string]any) (any, error) {
		return map[string]any{"timeoutMs": lastTimeout.Milliseconds()}, nil
	}
	handlers["forge_publish_draft"] = func(context.Context, map[string]any) (any, error) {
		return map[string]any{"pad": strings.Repeat("P", %d)}, nil
	}
	handlers["forge_describe"] = func(context.Context, map[string]any) (any, error) {
		cancelCurrent()
		select {}
	}
}
`

// argsFor returns arguments that pass the skeleton's top-level check: every
// required member with a value of its schema type.
func argsFor(t *testing.T, tool *contract.Tool) string {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{}
	for _, name := range schema.Required {
		typ := schema.Properties[name].Type
		if list, ok := typ.([]any); ok && len(list) > 0 {
			typ = list[0]
		}
		args[name] = map[any]any{"string": "x", "object": map[string]any{}, "integer": 1, "number": 1, "array": []any{}, "boolean": false}[typ]
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSkeletonWireHardening builds a forge skeleton with skeletonHooks and
// checks the raw-line JSON check, each failure reason, the output bound and the
// timeout on the wire.
func TestSkeletonWireHardening(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	c, _ := contract.Lookup("forge")
	toolByName := map[string]*contract.Tool{}
	for i := range c.Tools {
		toolByName[c.Tools[i].Name] = &c.Tools[i]
	}
	draft := toolByName["forge_publish_draft"]
	comments := toolByName["forge_comments"]
	if draft == nil || comments == nil || toolByName["forge_describe"] == nil {
		t.Fatal("the forge contract lacks a tool the test calls")
	}

	base := tempDir(t)
	dir := filepath.Join(base, "hooks")
	if err := Write(dir, Options{Kind: "forge", Lang: "go", Name: "hooks"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.go"), []byte(fmt.Sprintf(skeletonHooks, draft.MaxOutputBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(base, "server")
	buildSkeleton(t, dir, server, base)

	ping := func(id string) string { return `{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}` }
	call := func(id, name string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + argsFor(t, toolByName[name]) + `}}`
	}
	type want struct {
		id     string // the reply's id, as raw JSON
		code   int    // a JSON-RPC error code, or 0 for a result
		reason string // an isError tool result's reason, or "" for a plain result
		text   string // a substring of the reply
	}
	lines := []struct {
		name, input string
		want        want
	}{
		{"leading U+00A0", " " + ping("1"), want{id: "null", code: -32700}},
		{"leading U+0085", "\u0085" + ping("2"), want{id: "null", code: -32700}},
		{"trailing U+00A0", ping("3") + " ", want{id: "null", code: -32700}},
		{"JSON whitespace around", " \t" + ping("4") + " \t", want{id: "4"}},
		{"auth-missing", call("5", "forge_policy"), want{id: "5", reason: "auth-missing", text: "no token"}},
		{"rate-limited", call("6", "forge_find_change"), want{id: "6", reason: "rate-limited"}},
		{"network", call("7", "forge_change"), want{id: "7", reason: "network"}},
		{"an unknown reason", call("8", "forge_checks"), want{id: "8", reason: "failed"}},
		{"the contract timeout", call("9", "forge_comments"), want{id: "9", text: fmt.Sprintf(`"timeoutMs":%d`, comments.TimeoutMS)}},
		{"over the output bound", call("10", "forge_publish_draft"), want{id: "10", reason: "failed", text: "output bound"}},
		{"timed out", call("11", "forge_describe"), want{id: "11", reason: "failed", text: "timed out"}},
		{"while the hung handler runs", call("12", "forge_comments"), want{id: "12", reason: "failed", text: "still running"}},
		{"ping after the hung handler", ping("13"), want{id: "13"}},
	}
	var in strings.Builder
	for _, l := range lines {
		in.WriteString(l.input + "\n")
	}
	cmd := exec.Command(server)
	cmd.Dir = base
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(in.String())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("the server failed: %v\n%s", err, stderr.String())
	}
	replies := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(replies) != len(lines) {
		t.Fatalf("%d replies for %d lines: %q", len(replies), len(lines), replies)
	}
	for i, l := range lines {
		raw := replies[i]
		var reply struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
			Result *struct {
				IsError           bool `json:"isError"`
				StructuredContent struct {
					Reason string `json:"reason"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(raw), &reply); err != nil || string(reply.ID) != l.want.id {
			t.Fatalf("%s: reply %s, %v", l.name, raw, err)
		}
		switch {
		case l.want.code != 0:
			if reply.Error == nil || reply.Error.Code != l.want.code {
				t.Fatalf("%s: want error %d, got %s", l.name, l.want.code, raw)
			}
		case l.want.reason != "":
			if reply.Result == nil || !reply.Result.IsError || reply.Result.StructuredContent.Reason != l.want.reason {
				t.Fatalf("%s: want reason %s, got %s", l.name, l.want.reason, raw)
			}
		default:
			if reply.Error != nil || reply.Result == nil || reply.Result.IsError {
				t.Fatalf("%s: want a result, got %s", l.name, raw)
			}
		}
		if !strings.Contains(raw, l.want.text) {
			t.Fatalf("%s: reply lacks %q: %s", l.name, l.want.text, raw)
		}
		if strings.Contains(raw, "PPPP") {
			t.Fatalf("%s: part of an over-bound result was sent", l.name)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
