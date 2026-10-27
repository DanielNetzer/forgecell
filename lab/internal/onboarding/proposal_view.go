package onboarding

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// A read-only view of a saved proposal for a reviewer: the pending proposal, the
// approved Formula it changes, its changed keys (old -> new) and a bounded line
// diff. Nothing here writes, locks or activates; every bound fails safe by
// reporting the view as truncated.

// Proposal kinds, inferred from the changed keys.
const (
	KindRebind       = "rebind"
	KindBaseline     = "baseline"
	KindEnableReview = "enable-review"
	KindModelPin     = "model-pin"
	KindBootstrap    = "bootstrap"
	KindChange       = "change"
)

const (
	// maxDiffLeaves bounds the keys one Formula document is flattened into.
	maxDiffLeaves = 4000
	// maxDiffDepth bounds the nesting the flattening follows.
	maxDiffDepth = 12
	// maxKeyChanges bounds the changed keys a view carries.
	maxKeyChanges = 100
	// maxDiffValueBytes bounds one old or new value.
	maxDiffValueBytes = 400
	// maxDiffLines bounds the lines each side of a line diff compares.
	maxDiffLines = 2000
	// maxDiffOutput bounds the lines a line diff carries.
	maxDiffOutput = 1500
	// diffContext is the unchanged lines kept around each change.
	diffContext = 3
)

// PendingProposal is the proposal init would preserve: an interrupted activation
// first, else the first pending one; nil when there is none.
func PendingProposal(dir string) (*Proposal, error) { return pendingProposal(dir) }

// ProposalBefore is the exact approved Formula the proposal changes, when it can
// be established: the YAML the proposal saved, else the active Formula it names,
// either only when its SHA-256 is the one the proposal recorded.
func ProposalBefore(dir string, p Proposal) (string, bool) {
	if p.ActiveHashBefore == "" {
		return "", false
	}
	candidates := []string{}
	if e := p.CapabilityEvidence; e != nil {
		candidates = append(candidates, e.BeforeYAML)
		if e.Meta != nil {
			candidates = append(candidates, e.Meta.BeforeYAML)
		}
	}
	for _, c := range candidates {
		if c != "" && hash([]byte(c)) == p.ActiveHashBefore {
			return c, true
		}
	}
	if p.ActiveIDBefore != "" {
		if active, err := formula.Inspect(dir, p.ActiveIDBefore); err == nil && active.Formula.SHA256 == p.ActiveHashBefore {
			return active.Formula.YAML, true
		}
	}
	return "", false
}

// KeyChange is one changed key of a Formula: Change is added, removed or
// changed, and Old and New are the JSON of the values ("" when absent).
type KeyChange struct {
	Key       string `json:"key"`
	Change    string `json:"change"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ProposalDiff lists the keys that differ between two Formula documents. Lists
// of mappings with an id are keyed by it (atoms[review], requiredChecks[go-test]),
// so an insertion reads as one added entry. truncated reports a bound reached.
func ProposalDiff(before, after string) (changes []KeyChange, truncated bool, err error) {
	var a, b any
	if err = yaml.Unmarshal([]byte(before), &a); err != nil {
		return nil, false, err
	}
	if err = yaml.Unmarshal([]byte(after), &b); err != nil {
		return nil, false, err
	}
	left, right := &flat{values: map[string]string{}}, &flat{values: map[string]string{}}
	left.walk("", a, 0)
	right.walk("", b, 0)
	truncated = left.truncated || right.truncated
	record := func(c KeyChange) {
		if len(changes) == maxKeyChanges {
			truncated = true
			return
		}
		c.Old, c.Truncated = bounded(c.Old, c.Truncated)
		c.New, c.Truncated = bounded(c.New, c.Truncated)
		truncated = truncated || c.Truncated
		changes = append(changes, c)
	}
	for _, key := range left.order {
		old := left.values[key]
		next, ok := right.values[key]
		switch {
		case !ok:
			record(KeyChange{Key: key, Change: "removed", Old: old})
		case next != old:
			record(KeyChange{Key: key, Change: "changed", Old: old, New: next})
		}
	}
	for _, key := range right.order {
		if _, ok := left.values[key]; !ok {
			record(KeyChange{Key: key, Change: "added", New: right.values[key]})
		}
	}
	return changes, truncated, nil
}

func bounded(s string, truncated bool) (string, bool) {
	if len(s) <= maxDiffValueBytes {
		return s, truncated
	}
	cut := maxDiffValueBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…", true
}

// flat is a Formula document flattened to key paths and JSON leaf values.
type flat struct {
	values    map[string]string
	order     []string
	truncated bool
}

func (f *flat) leaf(key string, v any) {
	if len(f.order) == maxDiffLeaves {
		f.truncated = true
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(fmt.Sprintf("%q", fmt.Sprint(v)))
	}
	if _, seen := f.values[key]; !seen {
		f.order = append(f.order, key)
	}
	f.values[key] = string(data)
}

func scalarList(list []any) bool {
	for _, item := range list {
		switch item.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func (f *flat) walk(prefix string, v any, depth int) {
	if depth > maxDiffDepth {
		f.truncated = true
		return
	}
	join := func(key string) string {
		if prefix == "" {
			return key
		}
		return prefix + "." + key
	}
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			f.leaf(prefix, value)
		}
		for _, k := range keys {
			f.walk(join(k), value[k], depth+1)
		}
	case []any:
		if len(value) == 0 || scalarList(value) {
			f.leaf(prefix, value)
			return
		}
		for i, item := range value {
			name := fmt.Sprintf("%s[%d]", prefix, i)
			if m, ok := item.(map[string]any); ok {
				if id, ok := m["id"].(string); ok && id != "" && !strings.ContainsAny(id, "[].") {
					name = prefix + "[" + id + "]"
				}
			}
			f.walk(name, item, depth+1)
		}
	default:
		f.leaf(prefix, value)
	}
}

// ProposalKind names the kind of change from its keys: a baseline refresh changes
// only requiredChecks, enabling review only adds atoms[review], a model pin only
// model keys and a rebind only binding and command keys. hasBefore is false for a
// proposal with no approved Formula before it.
func ProposalKind(changes []KeyChange, hasBefore bool) string {
	if !hasBefore {
		return KindBootstrap
	}
	if len(changes) == 0 {
		return KindChange
	}
	all := func(ok func(KeyChange) bool) bool {
		for _, c := range changes {
			if !ok(c) {
				return false
			}
		}
		return true
	}
	switch {
	case all(func(c KeyChange) bool {
		return c.Key == "requiredChecks" || strings.HasPrefix(c.Key, "requiredChecks[")
	}):
		return KindBaseline
	case all(func(c KeyChange) bool { return c.Change == "added" && strings.HasPrefix(c.Key, "atoms[review].") }):
		return KindEnableReview
	case all(func(c KeyChange) bool {
		return c.Key == "harness.model" || strings.HasPrefix(c.Key, "harnesses.") && strings.HasSuffix(c.Key, ".model") && strings.Count(c.Key, ".") == 2
	}):
		return KindModelPin
	case all(func(c KeyChange) bool {
		return c.Change == "changed" && (strings.HasSuffix(c.Key, ".binding") || strings.HasSuffix(c.Key, ".command"))
	}):
		return KindRebind
	}
	return KindChange
}

// DiffLine is one line of a line diff: Op is " " (unchanged), "-" (removed),
// "+" (added) or "…" (unchanged lines left out).
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// LineDiff is a bounded line diff of two texts with diffContext unchanged lines
// around each change. A side longer than maxDiffLines after the common prefix and
// suffix is shown as removed and added whole, and truncated is set; so it is when
// the output reaches maxDiffOutput lines.
func LineDiff(before, after string) (lines []DiffLine, truncated bool) {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	midA, midB := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	var ops []DiffLine
	for _, line := range a[:prefix] {
		ops = append(ops, DiffLine{" ", line})
	}
	if len(midA) > maxDiffLines || len(midB) > maxDiffLines {
		truncated = true
		for _, line := range midA {
			ops = append(ops, DiffLine{"-", line})
		}
		for _, line := range midB {
			ops = append(ops, DiffLine{"+", line})
		}
	} else {
		ops = append(ops, lcsDiff(midA, midB)...)
	}
	for _, line := range a[len(a)-suffix:] {
		ops = append(ops, DiffLine{" ", line})
	}
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.Op == " " {
			continue
		}
		for j := max(0, i-diffContext); j <= min(len(ops)-1, i+diffContext); j++ {
			keep[j] = true
		}
	}
	for i, op := range ops {
		if len(lines) >= maxDiffOutput {
			return lines, true
		}
		if keep[i] {
			op.Text, _ = bounded(op.Text, false)
			lines = append(lines, op)
		} else if len(lines) == 0 || lines[len(lines)-1].Op != "…" {
			lines = append(lines, DiffLine{"…", ""})
		}
	}
	return lines, truncated
}

// lcsDiff is the line diff of a and b by longest common subsequence; each side
// has at most maxDiffLines lines.
func lcsDiff(a, b []string) []DiffLine {
	n, m := len(a), len(b)
	table := make([]int32, (n+1)*(m+1))
	at := func(i, j int) *int32 { return &table[i*(m+1)+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				*at(i, j) = *at(i+1, j+1) + 1
			} else {
				*at(i, j) = max(*at(i+1, j), *at(i, j+1))
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{" ", a[i]})
			i, j = i+1, j+1
		case *at(i+1, j) >= *at(i, j+1):
			out = append(out, DiffLine{"-", a[i]})
			i++
		default:
			out = append(out, DiffLine{"+", b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{"-", a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{"+", b[j]})
	}
	return out
}
