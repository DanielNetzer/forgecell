package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

// These helpers expose the mcp-provider/v1 rules to sibling manifests (the
// plugin kinds) and to strict decoders of plugin output, so one rule set holds
// for every manifest. They wrap the private rules and never change them.

// RequireExactKeys refuses any object key in raw, at any depth, that is not
// spelled exactly like a JSON field of t (a struct, or a pointer or slice of
// one). It catches case-variant and Kelvin-sign keys that encoding/json would
// otherwise match without regard to case.
func RequireExactKeys(raw []byte, t reflect.Type) error {
	if t == nil {
		return errors.New("no type to check keys against")
	}
	var doc any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return checkKeys(doc, t)
}

// ValidatePin applies the manifest rule for a pinned file: a clean absolute path
// without placeholders and a lowercase SHA-256.
func ValidatePin(label string, f pinned.File) error { return validPin(label, f) }

// Validate applies the manifest rules for a command's executable and optional
// interpreter.
func (c Command) Validate(label string) error { return c.validate(label) }

// ValidName reports whether s is a valid extension or plugin name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ValidToolName reports whether s is a valid MCP tool name for a manifest.
func ValidToolName(s string) bool { return toolPattern.MatchString(s) }

// ValidateArgs applies the manifest rules for an argv: no NUL byte, placeholders
// only as a whole argument or a clean path prefix, and no argument that names a
// pinned path.
func ValidateArgs(args []string, pins []pinned.File) error {
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("an argument contains a NUL byte")
		}
		if err := checkPlaceholders(arg); err != nil {
			return err
		}
		for _, p := range pins {
			if pinned.PathInArg(arg, p.Path) {
				return fmt.Errorf("argument %q contains the pinned path %s", arg, p.Path)
			}
		}
	}
	return nil
}
