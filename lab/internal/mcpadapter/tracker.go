package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// Bounds of tracker input.
const (
	// MaxConfigBytes bounds the Formula's intake.config as JSON.
	MaxConfigBytes = 4 << 10
	// maxKeyPatternRunes bounds a declared keyPattern.
	maxKeyPatternRunes = 256
	// maxKeyRunes bounds an Issue key.
	maxKeyRunes = 128
	// maxSelectorRunes bounds a selector, as the contract does.
	maxSelectorRunes = 200
)

// Tracker serves tracker.Keyed from a tracker plugin. Every call carries
// {config}, the Formula's intake.config. Every key, sent or received, must
// match the plugin's declared keyPattern, which each operation reads first
// with tracker_describe.
type Tracker struct {
	opts   Options
	config json.RawMessage
}

var _ tracker.Keyed = (*Tracker)(nil)

// NewTracker returns a Tracker passing config, JSON-compatible data that must
// encode to an object of at most MaxConfigBytes; nil is the empty object.
func NewTracker(opts Options, config any) (*Tracker, error) {
	if config == nil {
		config = map[string]any{}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, refuse("", stepOptions, mcp.SchemaInvalid, "the tracker config is not JSON-compatible")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil || compact.Len() > MaxConfigBytes || compact.Bytes()[0] != '{' {
		return nil, refuse("", stepOptions, mcp.SchemaInvalid, "the tracker config must be a JSON object of at most 4 KiB")
	}
	return &Tracker{opts: opts, config: json.RawMessage(compact.Bytes())}, nil
}

// Describe returns the plugin's description, whose keyPattern must compile
// under the key rules.
func (t *Tracker) Describe(ctx context.Context) (contract.TrackerDescription, error) {
	var out contract.TrackerDescription
	err := run(ctx, t.opts, "tracker", func(s *session) error {
		d, _, err := t.describe(ctx, s)
		out = d
		return err
	})
	return out, err
}

// List reads one page of the Issues selector admits. The selector must be one
// the plugin declares, or extend a declared prefix ending in ':'. A page with
// more items than limit, or an item key that does not match keyPattern, is
// refused.
func (t *Tracker) List(ctx context.Context, selector, cursor string, limit int) (contract.TrackerPage, error) {
	const tool = "tracker_list"
	if limit < 1 || limit > tracker.MaxListLimit {
		return contract.TrackerPage{}, refuse("", tool, mcp.SchemaInvalid, "the list limit is outside 1..100")
	}
	if !cleanText(selector, maxSelectorRunes) {
		return contract.TrackerPage{}, refuse("", tool, mcp.SchemaInvalid, "the selector is empty, too long or holds control or format characters")
	}
	if cursor != "" && !cleanText(cursor, 1024) {
		return contract.TrackerPage{}, refuse("", tool, mcp.SchemaInvalid, "the cursor is too long or holds control or format characters")
	}
	var out contract.TrackerPage
	err := run(ctx, t.opts, "tracker", func(s *session) error {
		d, pattern, err := t.describe(ctx, s)
		if err != nil {
			return err
		}
		if !admits(d.Selectors, selector) {
			return refuse(s.manifest.Name, tool, mcp.SchemaInvalid, "the plugin does not declare this selector")
		}
		args := map[string]any{"config": t.config, "selector": selector, "limit": limit}
		if cursor != "" {
			args["cursor"] = cursor
		}
		page, err := callAs[contract.TrackerPage](ctx, s, tool, args)
		if err != nil {
			return err
		}
		if err := checkPage(page, pattern, cursor, limit); err != nil {
			return s.refuseAnswer(tool, mcp.SchemaInvalid, err.Error())
		}
		out = page
		return nil
	})
	return out, err
}

// checkPage refuses a page a caller could not trust or page through to its
// end: more items than limit, a key that does not match keyPattern, a total
// below the items it holds, or a next cursor that is the request's own or
// follows a page with no items, either of which could page forever.
func checkPage(page contract.TrackerPage, pattern *regexp.Regexp, cursor string, limit int) error {
	if len(page.Items) > limit {
		return errors.New("the page holds more items than the limit")
	}
	for _, item := range page.Items {
		if matchKey(pattern, item.Key) != nil {
			return errors.New("a listed key does not match keyPattern")
		}
	}
	if page.Total != nil && *page.Total < int64(len(page.Items)) {
		return errors.New("the page's total is below the items it holds")
	}
	if page.NextCursor != nil {
		if *page.NextCursor == cursor {
			return errors.New("the next cursor is the request's own cursor")
		}
		if len(page.Items) == 0 {
			return errors.New("a page with no items has a next cursor")
		}
	}
	return nil
}

// Get reads one Issue; the answer must carry the key asked for.
func (t *Tracker) Get(ctx context.Context, key string) (contract.TrackerIssue, error) {
	const tool = "tracker_get"
	if err := checkKeyText(key); err != nil {
		return contract.TrackerIssue{}, refuse("", tool, mcp.SchemaInvalid, err.Error())
	}
	var out contract.TrackerIssue
	err := run(ctx, t.opts, "tracker", func(s *session) error {
		_, pattern, err := t.describe(ctx, s)
		if err != nil {
			return err
		}
		if err := matchKey(pattern, key); err != nil {
			return refuse(s.manifest.Name, tool, mcp.SchemaInvalid, err.Error())
		}
		issue, err := callAs[contract.TrackerIssue](ctx, s, tool, map[string]any{"config": t.config, "key": key})
		if err != nil {
			return err
		}
		if issue.Key != key {
			return s.refuseAnswer(tool, mcp.SchemaInvalid, "the plugin answered with a different Issue")
		}
		out = issue
		return nil
	})
	return out, err
}

// States reads the state of at most tracker.MaxStateKeys distinct Issues. The
// answer may leave out an Issue that was not found but must not name one that
// was not asked for.
func (t *Tracker) States(ctx context.Context, keys []string) (contract.TrackerStates, error) {
	const tool = "tracker_states"
	if len(keys) > tracker.MaxStateKeys {
		return contract.TrackerStates{}, refuse("", tool, mcp.SchemaInvalid, "more than 50 keys were asked for")
	}
	asked := map[string]bool{}
	for _, key := range keys {
		if err := checkKeyText(key); err != nil {
			return contract.TrackerStates{}, refuse("", tool, mcp.SchemaInvalid, err.Error())
		}
		if asked[key] {
			return contract.TrackerStates{}, refuse("", tool, mcp.SchemaInvalid, "a key is listed twice")
		}
		asked[key] = true
	}
	if len(keys) == 0 {
		return contract.TrackerStates{States: map[string]contract.TrackerState{}}, nil
	}
	var out contract.TrackerStates
	err := run(ctx, t.opts, "tracker", func(s *session) error {
		_, pattern, err := t.describe(ctx, s)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := matchKey(pattern, key); err != nil {
				return refuse(s.manifest.Name, tool, mcp.SchemaInvalid, err.Error())
			}
		}
		states, err := callAs[contract.TrackerStates](ctx, s, tool, map[string]any{"config": t.config, "keys": keys})
		if err != nil {
			return err
		}
		for key := range states.States {
			if !asked[key] {
				return s.refuseAnswer(tool, mcp.SchemaInvalid, "the plugin answered for a key that was not asked for")
			}
		}
		if states.States == nil {
			states.States = map[string]contract.TrackerState{}
		}
		out = states
		return nil
	})
	return out, err
}

// describe calls tracker_describe and compiles its keyPattern.
func (t *Tracker) describe(ctx context.Context, s *session) (contract.TrackerDescription, *regexp.Regexp, error) {
	const tool = "tracker_describe"
	d, err := callAs[contract.TrackerDescription](ctx, s, tool, map[string]any{"config": t.config})
	if err != nil {
		return contract.TrackerDescription{}, nil, err
	}
	pattern, err := compileKeyPattern(d.KeyPattern)
	if err != nil {
		return contract.TrackerDescription{}, nil, s.refuseAnswer(tool, mcp.SchemaInvalid, err.Error())
	}
	return d, pattern, nil
}

// admits reports whether selector is declared, or extends a declared prefix
// that ends in ':' with a non-empty value; a bare prefix is not a selector.
func admits(declared []string, selector string) bool {
	for _, d := range declared {
		if strings.HasSuffix(d, ":") {
			if len(selector) > len(d) && strings.HasPrefix(selector, d) {
				return true
			}
			continue
		}
		if selector == d {
			return true
		}
	}
	return false
}

// compileKeyPattern compiles a declared keyPattern of at most 256 runes as a
// Go (RE2, linear-time) expression anchored at both ends, whatever anchors it
// carries itself. The pattern must compile on its own first, so one that only
// compiles once wrapped, such as ^x)|(y$, cannot escape the anchors.
func compileKeyPattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || !utf8.ValidString(pattern) || utf8.RuneCountInString(pattern) > maxKeyPatternRunes {
		return nil, errors.New("keyPattern is empty, not UTF-8 or longer than 256 characters")
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, errors.New("keyPattern is not a valid regular expression")
	}
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return nil, errors.New("keyPattern is not a valid regular expression")
	}
	return re, nil
}

// matchKey requires key to pass checkKeyText and match the anchored pattern.
func matchKey(pattern *regexp.Regexp, key string) error {
	if err := checkKeyText(key); err != nil {
		return err
	}
	if !pattern.MatchString(key) {
		return errors.New("the key does not match keyPattern")
	}
	return nil
}

// checkKeyText refuses a key that is empty, not UTF-8, longer than 128 runes
// or holds a control, format (Cf, which covers the bidi controls), line or
// paragraph separator character.
func checkKeyText(key string) error {
	if !cleanText(key, maxKeyRunes) {
		return errors.New("the key is empty, too long or holds control, format or separator characters")
	}
	return nil
}

// cleanText reports whether s is non-empty UTF-8 of at most maxRunes runes with
// no control, Cf, bidi, U+2028 or U+2029 character.
func cleanText(s string, maxRunes int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxRunes {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Bidi_Control, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}
