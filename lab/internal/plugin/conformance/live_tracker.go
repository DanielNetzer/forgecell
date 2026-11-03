package conformance

import (
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Bounds of the tracker live checks.
const (
	liveListLimit = 20
	liveListPages = 5
	liveGetKeys   = 5
)

// tracker runs live-list, live-get and live-states. Every call passes the
// --config object; the target is never sent on its own.
func (lr *liveRun) tracker() {
	d, keys, err := lr.list()
	if !lr.record(CheckLiveList, err) {
		return
	}
	if len(keys) == 0 {
		for _, id := range []string{CheckLiveGet, CheckLiveStates} {
			lr.results[id] = Result{ID: id, Status: Skip, Reason: reasonNoKeys, Summary: summaries[id]}
		}
		return
	}
	if len(keys) > liveGetKeys {
		keys = keys[:liveGetKeys]
	}
	states, err := lr.get(d, keys)
	if !lr.record(CheckLiveGet, err) {
		return
	}
	lr.record(CheckLiveStates, lr.states(d, keys, states))
}

// list describes the tracker with the live config, then lists its first
// described selector, limit liveListLimit, following nextCursor for at most
// liveListPages pages. Keys must be unique across pages and match keyPattern,
// and no cursor may repeat.
func (lr *liveRun) list() (contract.TrackerDescription, []string, error) {
	const tool = "tracker_list"
	d, err := as[contract.TrackerDescription](lr.call("tracker_describe", map[string]any{"config": lr.config()}))
	if err != nil {
		return d, nil, err
	}
	if len(d.Selectors) == 0 {
		return d, nil, mcp.Fail(mcp.Failed, "tracker_describe", "the description names no selector")
	}
	pattern, err := anchoredKeyPattern(d.KeyPattern)
	if err != nil {
		return d, nil, err
	}
	described := describedStates(d)
	var keys []string
	seenKeys, seenCursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for page := 0; page < liveListPages; page++ {
		args := map[string]any{"config": lr.config(), "selector": d.Selectors[0], "limit": liveListLimit}
		if cursor != "" {
			args["cursor"] = cursor
		}
		p, err := as[contract.TrackerPage](lr.call(tool, args))
		if err != nil {
			return d, nil, err
		}
		if len(p.Items) > liveListLimit {
			return d, nil, mcp.Fail(mcp.Failed, tool, "a page holds more items than the limit")
		}
		for _, item := range p.Items {
			if seenKeys[item.Key] {
				return d, nil, mcp.Fail(mcp.Failed, tool, "a key repeats across pages")
			}
			if err := checkKey(tool, pattern, item.Key); err != nil {
				return d, nil, err
			}
			if !described[item.State] {
				return d, nil, mcp.Fail(mcp.Failed, tool, "a listed state is not a described state")
			}
			seenKeys[item.Key] = true
			keys = append(keys, item.Key)
		}
		if p.NextCursor == nil {
			break
		}
		next := *p.NextCursor
		if seenCursors[next] {
			return d, nil, mcp.Fail(mcp.Failed, tool, "a cursor repeats")
		}
		seenCursors[next] = true
		cursor = next
	}
	return d, keys, nil
}

// anchoredKeyPattern returns keyPattern anchored with ^ and $ as the host
// anchors it for intake.ParseIssueKey (molecule.TrackerKeyPattern). The raw
// pattern must compile on its own, so one that only compiles once wrapped,
// such as ^x)|(y$, is refused rather than turned into an alternation.
func anchoredKeyPattern(pattern string) (string, error) {
	if _, err := regexp.Compile(pattern); err != nil {
		return "", mcp.Fail(mcp.SchemaInvalid, "tracker_describe", "keyPattern is not a valid regular expression")
	}
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		pattern = "^(?:" + pattern + ")$"
	}
	return pattern, nil
}

// checkKey applies the host's key rules through intake.ParseIssueKey: the key
// text rules of intake.ValidateIssueKey (at most 128 runes, no control,
// format, bidi or separator character), then a whole match of the anchored
// keyPattern, never a substring one.
func checkKey(tool, pattern, key string) error {
	if _, err := intake.ParseIssueKey(key, pattern); err != nil {
		return mcp.Fail(mcp.Failed, tool, "a key fails the key rules or does not match keyPattern")
	}
	return nil
}

// describedStates is the set of the description's states.
func describedStates(d contract.TrackerDescription) map[string]bool {
	described := map[string]bool{}
	for _, s := range d.States {
		described[s] = true
	}
	return described
}

// get calls tracker_get for each key. Each must return the same key, passing
// the key rules, and a state the description lists. It returns each key's
// state.
func (lr *liveRun) get(d contract.TrackerDescription, keys []string) (map[string]string, error) {
	const tool = "tracker_get"
	pattern, err := anchoredKeyPattern(d.KeyPattern)
	if err != nil {
		return nil, err
	}
	described := describedStates(d)
	states := map[string]string{}
	for _, key := range keys {
		issue, err := as[contract.TrackerIssue](lr.call(tool, map[string]any{"config": lr.config(), "key": key}))
		if err != nil {
			return nil, err
		}
		if err := checkKey(tool, pattern, issue.Key); err != nil {
			return nil, err
		}
		if issue.Key != key {
			return nil, mcp.Fail(mcp.Failed, tool, "the issue carries another key")
		}
		if !described[issue.State] {
			return nil, mcp.Fail(mcp.Failed, tool, "the issue's state is not a described state")
		}
		states[key] = issue.State
	}
	return states, nil
}

// states calls tracker_states for keys once and requires every returned state
// to be a described one, and exactly those keys, each with the state
// tracker_get returned.
func (lr *liveRun) states(d contract.TrackerDescription, keys []string, got map[string]string) error {
	const tool = "tracker_states"
	out, err := as[contract.TrackerStates](lr.call(tool, map[string]any{"config": lr.config(), "keys": keys}))
	if err != nil {
		return err
	}
	described := describedStates(d)
	for _, s := range out.States {
		if !described[s.State] {
			return mcp.Fail(mcp.Failed, tool, "a returned state is not a described state")
		}
	}
	if len(out.States) != len(keys) {
		return mcp.Fail(mcp.Failed, tool, "the states do not cover exactly the requested keys")
	}
	for _, key := range keys {
		s, ok := out.States[key]
		if !ok {
			return mcp.Fail(mcp.Failed, tool, "a requested key has no state")
		}
		if s.State != got[key] {
			return mcp.Fail(mcp.Failed, tool, "tracker_states disagrees with tracker_get")
		}
	}
	return nil
}
