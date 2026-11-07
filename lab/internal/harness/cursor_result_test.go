package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// cursorOutput wraps result in Cursor's --print --output-format json final object.
func cursorOutput(t *testing.T, result string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readCursorOutcome(t *testing.T, result string) (string, CodingOutcome) {
	t.Helper()
	text, err := ReadResult("cursor", cursorOutput(t, result), true)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	return text, CodingOutcomeFromResult(text)
}

const cursorOutcomeObject = `{"schemaVersion":"v1","outcome":"completed","reason":"Updated README.md only: …","paths":[]}`

func TestReadResultCursorNarrationBeforeOutcome(t *testing.T) {
	narration := "Implementing the README-only documentation from the approved plan. Checking the repository state and current README.Expanding the README to match the acceptance criteria, then running the approved checks."
	text, out := readCursorOutcome(t, narration+cursorOutcomeObject)
	if text != cursorOutcomeObject {
		t.Fatalf("text = %q", text)
	}
	if out.Outcome != "completed" || out.Reason != "Updated README.md only: …" || out.Paths == nil || len(out.Paths) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	// Trailing whitespace after the object is allowed.
	if _, out := readCursorOutcome(t, narration+cursorOutcomeObject+"\n \t"); out.Outcome != "completed" {
		t.Fatalf("trailing whitespace outcome = %+v", out)
	}
}

func TestReadResultCursorObjectOnlyUnchanged(t *testing.T) {
	for _, result := range []string{cursorOutcomeObject, "  \n" + cursorOutcomeObject + "\n"} {
		text, out := readCursorOutcome(t, result)
		if text != result {
			t.Fatalf("text = %q, want %q", text, result)
		}
		if out.Outcome != "completed" || out.Reason != "Updated README.md only: …" {
			t.Fatalf("outcome = %+v", out)
		}
	}
}

func TestReadResultCursorNoTrailingObjectStaysUnknown(t *testing.T) {
	cases := map[string]string{
		"narration only":     "Working on it. Done.",
		"object then text":   "Done." + cursorOutcomeObject + " All checks passed.",
		"truncated object":   `Done.{"schemaVersion":"v1","outcome":"completed","reason":"Upd`,
		"fenced object":      "Done.\n```json\n" + cursorOutcomeObject + "\n```",
		"object starts text": cursorOutcomeObject + " trailing narration",
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			text, out := readCursorOutcome(t, result)
			if text != result {
				t.Fatalf("text = %q, want unchanged %q", text, result)
			}
			if out.Outcome != "unknown" || !strings.HasPrefix(out.Reason, "Invalid or missing coding outcome: ") {
				t.Fatalf("outcome = %+v", out)
			}
		})
	}
}

func TestReadResultCursorBracesInStrings(t *testing.T) {
	object := `{"schemaVersion":"v1","outcome":"completed","reason":"Handled {x} and a stray } plus {","paths":[]}`
	cases := map[string]string{
		"braces in narration": "Rendering {x} in the template, then {\"partial\": checks.",
		"braces in object":    "Narration first.",
		"braces in both":      "Saw `{\"k\":1}` in a log and a lone }.",
	}
	for name, narration := range cases {
		t.Run(name, func(t *testing.T) {
			text, out := readCursorOutcome(t, narration+object)
			if text != object {
				t.Fatalf("text = %q", text)
			}
			if out.Outcome != "completed" || out.Reason != "Handled {x} and a stray } plus {" {
				t.Fatalf("outcome = %+v", out)
			}
		})
	}
}

func TestReadResultCursorInvalidOutcomeStillValidated(t *testing.T) {
	// An unrelated trailing object is extracted but still fails outcome validation.
	_, out := readCursorOutcome(t, `Summary: {"files":2}`)
	if out.Outcome != "unknown" || !strings.HasPrefix(out.Reason, "Invalid or missing coding outcome: ") {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestReadResultCursorOnlyAffectsCursor(t *testing.T) {
	result := "Narration." + cursorOutcomeObject
	text, err := ReadResult("claude-code", cursorOutput(t, result), true)
	if err != nil || text != result {
		t.Fatalf("claude-code text = %q, err = %v", text, err)
	}
	if text, err := ReadResult("codex", result, true); err != nil || text != result {
		t.Fatalf("codex text = %q, err = %v", text, err)
	}
}
