package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxADFDepth bounds how deep the flattening descends; deeper content is
	// left out and the text is marked as cut.
	maxADFDepth = 32
	// maxADFNodes bounds how many nodes one description visits.
	maxADFNodes = 50_000
	// maxListStart bounds an ordered list's first number.
	maxListStart = 1_000_000
)

// adfNode is one Atlassian Document Format node. Only the fields the
// flattening reads are decoded.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
	Attrs   struct {
		Text      string `json:"text"`
		ShortName string `json:"shortName"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		Order     *int64 `json:"order"`
		State     string `json:"state"`
	} `json:"attrs"`
}

// descriptionText flattens an issue description to plain text of at most
// limit runes. A null or absent description is empty, a plain string is kept
// as it is and anything else must be an ADF node. A cut is always marked.
func descriptionText(raw json.RawMessage, limit int) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", malformed("a malformed description")
		}
		return cut(s, limit), nil
	}
	var doc adfNode
	if raw[0] != '{' || json.Unmarshal(raw, &doc) != nil {
		return "", malformed("a malformed description")
	}
	return flattenADF(&doc, limit), nil
}

// flattenADF renders an ADF document as plain text of at most limit runes:
// paragraphs and headings become lines, lists become indented "- " or "1. "
// items, code blocks keep their lines, quotes get "> ", table rows join their
// cells with " | ", task lists become "- [ ] " or "- [x] " items, decision
// lists "- " items, and text marks are dropped. Unknown inline nodes are
// skipped; an unknown block node is skipped too and the text ends in the
// truncated mark, so the skip is visible. Content past the depth, node or size
// bound is left out and the text ends in the truncated mark.
func flattenADF(doc *adfNode, limit int) string {
	r := &adfRenderer{limit: limit - utf8.RuneCountInString(truncatedMark)}
	r.block(doc, "", 0)
	text := strings.TrimRight(r.b.String(), "\n")
	if r.cut || r.skipped {
		text += truncatedMark
	}
	return text
}

type adfRenderer struct {
	b     strings.Builder
	runes int
	limit int
	nodes int
	cut   bool
	// skipped records an unknown block node that was left out; unlike cut,
	// rendering goes on after it.
	skipped bool
}

// visit counts a node against the node and depth bounds; false means the node
// is left out and the text is marked as cut.
func (r *adfRenderer) visit(depth int) bool {
	r.nodes++
	if r.cut || r.nodes > maxADFNodes || depth > maxADFDepth {
		r.cut = true
		return false
	}
	return true
}

// write appends s up to the size bound.
func (r *adfRenderer) write(s string) {
	if r.cut {
		return
	}
	n := utf8.RuneCountInString(s)
	if r.runes+n <= r.limit {
		r.b.WriteString(s)
		r.runes += n
		return
	}
	runes := []rune(s)
	r.b.WriteString(string(runes[:r.limit-r.runes]))
	r.runes = r.limit
	r.cut = true
}

// line writes text as lines, each starting with prefix.
func (r *adfRenderer) line(prefix, text string) {
	for _, part := range strings.Split(text, "\n") {
		r.write(strings.TrimRight(prefix+part, " \t") + "\n")
	}
}

// blank separates two top-level blocks with an empty line.
func (r *adfRenderer) blank() {
	if r.b.Len() > 0 && !strings.HasSuffix(r.b.String(), "\n\n") {
		r.write("\n")
	}
}

func (r *adfRenderer) blocks(nodes []adfNode, prefix string, depth int) {
	for i := range nodes {
		r.block(&nodes[i], prefix, depth)
	}
}

func (r *adfRenderer) block(n *adfNode, prefix string, depth int) {
	if !r.visit(depth) {
		return
	}
	switch n.Type {
	case "doc":
		for i := range n.Content {
			if i > 0 {
				r.blank()
			}
			r.block(&n.Content[i], prefix, depth+1)
		}
	case "paragraph", "heading":
		r.line(prefix, r.inline(n.Content, depth+1))
	case "codeBlock":
		r.line(prefix, r.inline(n.Content, depth+1))
	case "bulletList", "orderedList":
		start := int64(1)
		if n.Type == "orderedList" && n.Attrs.Order != nil && *n.Attrs.Order >= 0 && *n.Attrs.Order <= maxListStart {
			start = *n.Attrs.Order
		}
		for i := range n.Content {
			marker := "- "
			if n.Type == "orderedList" {
				marker = strconv.FormatInt(start+int64(i), 10) + ". "
			}
			r.listItem(&n.Content[i], prefix, marker, depth+1)
		}
	case "blockquote":
		r.blocks(n.Content, prefix+"> ", depth+1)
	case "panel", "expand", "nestedExpand", "layoutSection", "layoutColumn":
		if t := strings.TrimSpace(n.Attrs.Title); t != "" {
			r.line(prefix, t)
		}
		r.blocks(n.Content, prefix, depth+1)
	case "rule":
		r.line(prefix, "----")
	case "table":
		for i := range n.Content {
			r.tableRow(&n.Content[i], prefix, depth+1)
		}
	case "taskList", "decisionList":
		r.actionList(n, prefix, depth+1)
	default:
		r.skipped = true
	}
}

// actionList writes a task list's items as "- [ ] " or "- [x] " lines, by
// attrs.state, and a decision list's items as "- " lines. A task list nested
// in a task list is indented under the item before it.
func (r *adfRenderer) actionList(n *adfNode, prefix string, depth int) {
	item := "decisionItem"
	if n.Type == "taskList" {
		item = "taskItem"
	}
	for i := range n.Content {
		c := &n.Content[i]
		if !r.visit(depth) {
			return
		}
		switch {
		case c.Type == item:
			marker := "- "
			if item == "taskItem" {
				marker = "- [ ] "
				if c.Attrs.State == "DONE" {
					marker = "- [x] "
				}
			}
			head, tail, more := strings.Cut(r.inline(c.Content, depth+1), "\n")
			r.line(prefix+marker, head)
			if more {
				r.line(prefix+strings.Repeat(" ", utf8.RuneCountInString(marker)), tail)
			}
		case n.Type == "taskList" && c.Type == "taskList":
			r.actionList(c, prefix+strings.Repeat(" ", utf8.RuneCountInString("- [ ] ")), depth+1)
		default:
			r.skipped = true
		}
	}
}

// listItem writes one list item: its first paragraph after the marker, and
// every other block indented under it.
func (r *adfRenderer) listItem(n *adfNode, prefix, marker string, depth int) {
	if !r.visit(depth) {
		return
	}
	if n.Type != "listItem" {
		r.skipped = true
		return
	}
	rest := prefix + strings.Repeat(" ", utf8.RuneCountInString(marker))
	first := true
	for i := range n.Content {
		c := &n.Content[i]
		if first && c.Type == "paragraph" {
			first = false
			if !r.visit(depth + 1) {
				return
			}
			text := r.inline(c.Content, depth+2)
			head, tail, more := strings.Cut(text, "\n")
			r.line(prefix+marker, head)
			if more {
				r.line(rest, tail)
			}
			continue
		}
		if first {
			first = false
			r.line(prefix+marker, "")
		}
		r.block(c, rest, depth+1)
	}
	if first {
		r.line(prefix+marker, "")
	}
}

// tableRow writes one table row as its cells' text joined by " | ".
func (r *adfRenderer) tableRow(n *adfNode, prefix string, depth int) {
	if !r.visit(depth) {
		return
	}
	if n.Type != "tableRow" {
		r.skipped = true
		return
	}
	var cells []string
	for i := range n.Content {
		c := &n.Content[i]
		if !r.visit(depth + 1) {
			continue
		}
		if c.Type != "tableCell" && c.Type != "tableHeader" {
			r.skipped = true
			continue
		}
		var parts []string
		for j := range c.Content {
			if !r.visit(depth + 2) {
				break
			}
			if t := strings.TrimSpace(r.inline(c.Content[j].Content, depth+3)); t != "" {
				parts = append(parts, strings.ReplaceAll(t, "\n", " "))
			}
		}
		cells = append(cells, strings.Join(parts, " "))
	}
	r.line(prefix, strings.Join(cells, " | "))
}

// inline returns the text of inline nodes, bounded to the space left. Marks
// are dropped, a hard break is a new line, a mention, emoji or status keeps its
// text and an inline card its URL. Other nodes are skipped.
func (r *adfRenderer) inline(nodes []adfNode, depth int) string {
	var b strings.Builder
	room := r.limit - r.runes + 1
	for i := range nodes {
		n := &nodes[i]
		if b.Len() > room*utf8.UTFMax || !r.visit(depth) {
			break
		}
		switch n.Type {
		case "text":
			b.WriteString(n.Text)
		case "hardBreak":
			b.WriteString("\n")
		case "mention", "status":
			b.WriteString(n.Attrs.Text)
		case "emoji":
			if n.Attrs.Text != "" {
				b.WriteString(n.Attrs.Text)
			} else {
				b.WriteString(n.Attrs.ShortName)
			}
		case "inlineCard":
			b.WriteString(n.Attrs.URL)
		}
	}
	return b.String()
}
