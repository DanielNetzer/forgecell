package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// Schema is the strict JSON Schema subset the contracts use: type, properties,
// required, additionalProperties (false, true or a schema for map values),
// maxProperties, propertyNames, items, maxItems, enum, minLength, maxLength
// (runes), minimum, maximum, pattern and format. format is "identifier" (no
// control, format or line separator characters), "uri" (absolute http or https without
// credentials) or "date-time" (RFC 3339).
type Schema struct {
	Type                 string             `json:"type"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties,omitempty"`
	MaxProperties        *int               `json:"maxProperties,omitempty"`
	PropertyNames        *Schema            `json:"propertyNames,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	Minimum              *int64             `json:"minimum,omitempty"`
	Maximum              *int64             `json:"maximum,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
	Format               string             `json:"format,omitempty"`

	closed  bool
	open    bool
	values  *Schema
	pattern *regexp.Regexp
}

// maxAdvertisedSchemaBytes bounds a plugin's advertised inputSchema.
const maxAdvertisedSchemaBytes = 64 << 10

// prepare checks that the schema is strict and bounded, and compiles it.
func (s *Schema) prepare(path string) error {
	bad := func(why string) error { return fmt.Errorf("schema %s: %s", path, why) }
	if s == nil {
		return bad("is missing")
	}
	if s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return bad("pattern does not compile")
		}
		s.pattern = re
	}
	if s.Format != "" && !slices.Contains([]string{"identifier", "uri", "date-time"}, s.Format) {
		return bad("unknown format " + s.Format)
	}
	switch s.Type {
	case "object":
		switch string(s.AdditionalProperties) {
		case "false":
			s.closed = true
		case "true":
			if s.Properties != nil || s.MaxProperties == nil {
				return bad("an open object has no properties and a maxProperties bound")
			}
			s.open = true
		case "":
			return bad("additionalProperties must be explicit")
		default:
			s.values = &Schema{}
			if err := json.Unmarshal(s.AdditionalProperties, s.values); err != nil {
				return bad("additionalProperties is malformed")
			}
			if s.Properties != nil || s.MaxProperties == nil || s.PropertyNames == nil {
				return bad("a map needs maxProperties and propertyNames and no properties")
			}
			if err := s.values.prepare(path + "{}"); err != nil {
				return err
			}
			if err := s.PropertyNames.prepare(path + "{key}"); err != nil {
				return err
			}
		}
		for _, name := range s.Required {
			if _, ok := s.Properties[name]; !ok {
				return bad("requires the undeclared property " + name)
			}
		}
		for name, p := range s.Properties {
			if err := p.prepare(path + "." + name); err != nil {
				return err
			}
		}
	case "array":
		if s.Items == nil || s.MaxItems == nil {
			return bad("an array needs items and maxItems")
		}
		return s.Items.prepare(path + "[]")
	case "string":
		if s.MaxLength == nil && s.Enum == nil {
			return bad("a string needs maxLength or enum")
		}
	case "integer":
		if s.Minimum == nil || s.Maximum == nil {
			return bad("an integer needs minimum and maximum")
		}
	case "boolean":
	default:
		return bad("unsupported type " + s.Type)
	}
	return nil
}

// Validate checks a value decoded with json.Decoder.UseNumber.
func (s *Schema) Validate(v any) error { return s.validate("$", v) }

func (s *Schema) validate(path string, v any) error {
	bad := func(why string) error { return fmt.Errorf("%s %s", path, why) }
	if s.Enum != nil && !slices.ContainsFunc(s.Enum, func(e any) bool { return reflect.DeepEqual(e, v) }) {
		return bad("is not an allowed value")
	}
	switch s.Type {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return bad("must be an object")
		}
		if s.MaxProperties != nil && len(obj) > *s.MaxProperties {
			return bad(fmt.Sprintf("has more than %d members", *s.MaxProperties))
		}
		for _, name := range s.Required {
			if _, ok := obj[name]; !ok {
				return bad("is missing " + name)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(obj)) {
			at := path + "." + strconv.Quote(key)
			if p, ok := s.Properties[key]; ok {
				if err := p.validate(at, obj[key]); err != nil {
					return err
				}
				continue
			}
			switch {
			case s.values != nil:
				if err := s.PropertyNames.validate(at+"(key)", key); err != nil {
					return err
				}
				if err := s.values.validate(at, obj[key]); err != nil {
					return err
				}
			case !s.open:
				return bad("has an unknown member " + strconv.Quote(key))
			}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return bad("must be an array")
		}
		if len(list) > *s.MaxItems {
			return bad(fmt.Sprintf("has more than %d items", *s.MaxItems))
		}
		for i, item := range list {
			if err := s.Items.validate(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return bad("must be a string")
		}
		return s.validateString(path, str)
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return bad("must be an integer")
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return bad("must be an integer")
		}
		if i < *s.Minimum || i > *s.Maximum {
			return bad(fmt.Sprintf("must be between %d and %d", *s.Minimum, *s.Maximum))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad("must be a boolean")
		}
	}
	return nil
}

func (s *Schema) validateString(path, str string) error {
	bad := func(why string) error { return fmt.Errorf("%s %s", path, why) }
	if !utf8.ValidString(str) {
		return bad("is not valid UTF-8")
	}
	n := utf8.RuneCountInString(str)
	if s.MinLength != nil && n < *s.MinLength {
		return bad(fmt.Sprintf("is shorter than %d characters", *s.MinLength))
	}
	if s.MaxLength != nil && n > *s.MaxLength {
		return bad(fmt.Sprintf("is longer than %d characters", *s.MaxLength))
	}
	if s.pattern != nil && !s.pattern.MatchString(str) {
		return bad("does not match its pattern")
	}
	switch s.Format {
	case "identifier":
		if !identifier(str) {
			return bad("contains control, format or line separator characters")
		}
	case "uri":
		u, err := url.Parse(str)
		if !identifier(str) || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return bad("must be an absolute http or https URL without credentials")
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, str); err != nil {
			return bad("must be an RFC 3339 date-time")
		}
	}
	return nil
}

// identifier reports whether s has no control, format, line or paragraph
// separator (U+2028, U+2029) or surrounding space characters, so it can be
// compared and shown verbatim.
func identifier(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return len(s) == len(bytes.TrimSpace([]byte(s)))
}

// advertised is the part of a plugin's inputSchema the compatibility check
// reads. type may be a string or a list of strings.
type advertised struct {
	Type       any                        `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

// Compatible checks a plugin's advertised inputSchema against the contract
// input: an object schema that declares every contract-required property with
// the same JSON type, declares any optional contract property it lists with
// the same type, and requires nothing the contract does not require.
func (s *Schema) Compatible(raw json.RawMessage) error {
	if len(raw) > maxAdvertisedSchemaBytes {
		return fmt.Errorf("the inputSchema exceeds %d bytes", maxAdvertisedSchemaBytes)
	}
	var a advertised
	if err := json.Unmarshal(raw, &a); err != nil {
		return errors.New("the inputSchema is malformed")
	}
	if !typeAllows(a.Type, "object") {
		return errors.New("the inputSchema is not an object schema")
	}
	for name, want := range s.Properties {
		got, ok := a.Properties[name]
		if !ok {
			if slices.Contains(s.Required, name) {
				return fmt.Errorf("the inputSchema lacks the required property %q", name)
			}
			continue
		}
		var p struct {
			Type any `json:"type"`
		}
		if err := json.Unmarshal(got, &p); err != nil || !typeAllows(p.Type, want.Type) {
			return fmt.Errorf("the inputSchema property %q is not of type %s", name, want.Type)
		}
	}
	for _, name := range a.Required {
		if !slices.Contains(s.Required, name) {
			return fmt.Errorf("the inputSchema requires %q, which the contract does not", name)
		}
	}
	return nil
}

// typeAllows reports whether an advertised type (a string or a list of
// strings) accepts values of the contract type. A number accepts integers.
func typeAllows(advertisedType any, want string) bool {
	ok := func(t any) bool { return t == want || (want == "integer" && t == "number") }
	switch t := advertisedType.(type) {
	case string:
		return ok(t)
	case []any:
		return slices.ContainsFunc(t, ok)
	}
	return false
}
