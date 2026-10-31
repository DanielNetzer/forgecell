package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// outputs maps each contract tool to a constructor of its output type.
var outputs = map[string]func() any{
	"tracker_describe":    func() any { return new(TrackerDescription) },
	"tracker_list":        func() any { return new(TrackerPage) },
	"tracker_get":         func() any { return new(TrackerIssue) },
	"tracker_states":      func() any { return new(TrackerStates) },
	"forge_describe":      func() any { return new(ForgeDescription) },
	"forge_policy":        func() any { return new(ForgePolicy) },
	"forge_find_change":   func() any { return new(ForgeChanges) },
	"forge_publish_draft": func() any { return new(ForgePublished) },
	"forge_change":        func() any { return new(ForgeChange) },
	"forge_checks":        func() any { return new(ForgeChecks) },
	"forge_comments":      func() any { return new(ForgeComments) },
	"reviewer_describe":   func() any { return new(ReviewerDescription) },
	"reviewer_findings":   func() any { return new(ReviewerFindings) },
}

// checker is an output type with rules beyond its schema.
type checker interface{ check() error }

// FindTool returns the contract tool with the given name, of any kind.
func FindTool(name string) (*Tool, bool) {
	for _, kind := range Kinds {
		if t, ok := contracts[kind].Tool(name); ok {
			return t, true
		}
	}
	return nil, false
}

// Decode decodes one tool's structuredContent strictly and returns its output
// type by value (for example TrackerIssue for tracker_get). Output over the
// tool's byte cap is OutputTooLarge; every other refusal is SchemaInvalid.
func (t *Tool) Decode(raw []byte) (any, error) {
	invalid := func(why string) (any, error) { return nil, mcp.Fail(mcp.SchemaInvalid, t.Name, why) }
	if len(raw) > t.MaxOutputBytes {
		return nil, mcp.Fail(mcp.OutputTooLarge, t.Name, "the output exceeds "+strconv.Itoa(t.MaxOutputBytes)+" bytes")
	}
	doc, err := strictDocument(raw)
	if err != nil {
		return invalid(err.Error())
	}
	if err := t.Output.Validate(doc); err != nil {
		return invalid(err.Error())
	}
	out := outputs[t.Name]()
	if err := extension.RequireExactKeys(raw, reflect.TypeOf(out)); err != nil {
		return invalid(err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return invalid(err.Error())
	}
	sanitize(reflect.ValueOf(out))
	if c, ok := out.(checker); ok {
		if err := c.check(); err != nil {
			return invalid(err.Error())
		}
	}
	return reflect.ValueOf(out).Elem().Interface(), nil
}

// Decode decodes the output of the named contract tool.
func Decode(tool string, raw []byte) (any, error) {
	t, ok := FindTool(tool)
	if !ok {
		return nil, mcp.Fail(mcp.ContractMismatch, "", "the tool is not part of any contract")
	}
	return t.Decode(raw)
}

// strictDocument decodes exactly one JSON value that is valid UTF-8 and repeats
// no key, keeping numbers exact.
func strictDocument(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("the output is not valid UTF-8")
	}
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		var dup *pinned.DuplicateKeyError
		if errors.As(err, &dup) {
			return nil, fmt.Errorf("the output repeats the key %q", dup.Key)
		}
		return nil, errors.New("the output is malformed JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.New("the output is malformed JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the output has trailing data")
	}
	return doc, nil
}

// sanitize cleans every field tagged text:"N" with safetext.Text, or
// text:"N,multiline" with safetext.Multiline, to an N-rune bound, at any depth.
// Untagged strings are identifiers the schema already restricted.
func sanitize(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			sanitize(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if bound, lines, ok := textBound(v.Type().Field(i)); ok {
				clean(v.Field(i), bound, lines)
				continue
			}
			sanitize(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			sanitize(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			item := reflect.New(v.Type().Elem()).Elem()
			item.Set(v.MapIndex(key))
			sanitize(item)
			v.SetMapIndex(key, item)
		}
	}
}

func textBound(f reflect.StructField) (int, bool, bool) {
	tag, ok := f.Tag.Lookup("text")
	if !ok {
		return 0, false, false
	}
	bound, option, _ := strings.Cut(tag, ",")
	n, err := strconv.Atoi(bound)
	if err != nil || n < 1 || (option != "" && option != "multiline") {
		panic("contract: bad text tag on " + f.Name)
	}
	return n, option == "multiline", true
}

func clean(v reflect.Value, bound int, lines bool) {
	switch v.Kind() {
	case reflect.String:
		if lines {
			v.SetString(safetext.Multiline(v.String(), bound))
		} else {
			v.SetString(safetext.Text(v.String(), bound))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			clean(v.Elem(), bound, lines)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			clean(v.Index(i), bound, lines)
		}
	}
}
