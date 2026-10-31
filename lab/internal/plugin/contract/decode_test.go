package contract

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// fixtureCase is one golden case. The output is used byte for byte (so
// duplicate keys survive), or raw replaces it; expand then replaces tokens in
// those bytes to build huge, repeated or invalid-UTF-8 input. Exactly one of
// want (the decoded value), accepted or reason is set.
type fixtureCase struct {
	Name     string               `json:"name"`
	Tool     string               `json:"tool"`
	Output   json.RawMessage      `json:"output"`
	Raw      *string              `json:"raw"`
	Expand   map[string]expansion `json:"expand"`
	Want     json.RawMessage      `json:"want"`
	Accepted bool                 `json:"accepted"`
	Reason   string               `json:"reason"`
}

type expansion struct {
	Repeat    string `json:"repeat"`
	Sequence  string `json:"sequence"`
	Separator string `json:"separator"`
	Hex       string `json:"hex"`
	Count     int    `json:"count"`
}

func (e expansion) build(t *testing.T) []byte {
	t.Helper()
	switch {
	case e.Hex != "":
		b, err := hex.DecodeString(e.Hex)
		if err != nil {
			t.Fatal(err)
		}
		return b
	case e.Sequence != "":
		parts := make([]string, e.Count)
		for i := range parts {
			parts[i] = fmt.Sprintf(e.Sequence, i)
		}
		return []byte(strings.Join(parts, e.Separator))
	default:
		return []byte(strings.Repeat(e.Repeat, e.Count))
	}
}

func (c fixtureCase) input(t *testing.T) []byte {
	t.Helper()
	raw := []byte(c.Output)
	if c.Raw != nil {
		raw = []byte(*c.Raw)
	}
	for token, e := range c.Expand {
		if !bytes.Contains(raw, []byte(token)) {
			t.Fatalf("%s: token %q is not in the output", c.Name, token)
		}
		raw = bytes.ReplaceAll(raw, []byte(token), e.build(t))
	}
	return raw
}

func normalize(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGoldenFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 11 {
		t.Fatalf("found %d fixture files, want 11", len(paths))
	}
	accepted, refused := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cases []fixtureCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, c := range cases {
			t.Run(filepath.Base(path)+"/"+c.Name, func(t *testing.T) {
				set := 0
				for _, b := range []bool{c.Want != nil, c.Accepted, c.Reason != ""} {
					if b {
						set++
					}
				}
				if set != 1 {
					t.Fatalf("set exactly one of want, accepted and reason")
				}
				got, err := Decode(c.Tool, c.input(t))
				if c.Reason != "" {
					if err == nil {
						t.Fatalf("accepted %+v, want %s", got, c.Reason)
					}
					if r := mcp.ReasonOf(err); string(r) != c.Reason {
						t.Fatalf("reason %s (%v), want %s", r, err, c.Reason)
					}
					refused[c.Tool] = true
					return
				}
				if err != nil {
					t.Fatalf("refused: %v (%s)", err, err.(*mcp.Error).Diagnostics)
				}
				accepted[c.Tool] = true
				if c.Want == nil {
					return
				}
				encoded, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if g, w := normalize(t, encoded), normalize(t, c.Want); !reflect.DeepEqual(g, w) {
					t.Fatalf("decoded\n %s\nwant\n %s", encoded, c.Want)
				}
			})
		}
	}
	for _, kind := range Kinds {
		c, _ := Lookup(kind)
		for _, tool := range c.Tools {
			if !accepted[tool.Name] || !refused[tool.Name] {
				t.Errorf("%s needs both an accepted and a refused golden case", tool.Name)
			}
		}
	}
}

func TestDecodeReturnsTypedValues(t *testing.T) {
	got, err := Decode("forge_publish_draft", []byte(`{"ref":"!1","url":"https://example.com/1","draft":true,"headCommit":"0123456789abcdef0123456789abcdef01234567"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := got.(ForgePublished); !ok || !p.Draft || p.Ref != "!1" {
		t.Fatalf("Decode = %#v", got)
	}
	if _, err := Decode("forge_merge", []byte(`{}`)); mcp.ReasonOf(err) != mcp.ContractMismatch {
		t.Fatalf("an unknown tool: %v", err)
	}
}

func TestEveryDisplayFieldIsBoundedLikeItsSchema(t *testing.T) {
	for _, kind := range Kinds {
		c, _ := Lookup(kind)
		for _, tool := range c.Tools {
			checkBounds(t, tool.Name, reflect.TypeOf(outputs[tool.Name]()).Elem(), tool.Output)
		}
	}
}

// checkBounds requires every text tag to equal the schema maxLength of its field,
// so sanitizing never lengthens what the schema allowed.
func checkBounds(t *testing.T, path string, typ reflect.Type, s *Schema) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
		if s.Type == "array" {
			s = s.Items
		}
	}
	switch typ.Kind() {
	case reflect.Map:
		checkBounds(t, path+"{}", typ.Elem(), s.values)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			prop, ok := s.Properties[name]
			if !ok {
				t.Errorf("%s.%s is not in the schema", path, name)
				continue
			}
			if bound, _, ok := textBound(f); ok {
				leaf := prop
				if leaf.Type == "array" {
					leaf = leaf.Items
				}
				if leaf.MaxLength == nil || *leaf.MaxLength != bound {
					t.Errorf("%s.%s text bound %d differs from its schema maxLength", path, name, bound)
				}
				continue
			}
			checkBounds(t, path+"."+name, f.Type, prop)
		}
	}
}
