package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const deniedSuccess = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]},"result":"{}","permission_denials":[{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"go test ./internal/cli"}},{"tool_name":"Write","tool_use_id":"t2","tool_input":{"file_path":"outside.txt"}}]}`

func TestCodingResultKeepsOutcomeAndRecordsDenials(t *testing.T) {
	text, denials, err := ReadCodingResult("claude-code", deniedSuccess)
	if err != nil {
		t.Fatal(err)
	}
	if out := CodingOutcomeFromResult(text); out.Outcome != "completed" {
		t.Fatalf("outcome %+v", out)
	}
	want := []string{"Bash: go test ./internal/cli", "Write: outside.txt"}
	if strings.Join(denials, "|") != strings.Join(want, "|") {
		t.Fatalf("denials %q, want %q", denials, want)
	}
}

func TestCodingResultStillRejectsFailures(t *testing.T) {
	for _, raw := range []string{`not json`, `null`, `{"is_error":true,"result":"failed","permission_denials":[]}`, `{"result":"","permission_denials":[{"tool_name":"Bash"}]}`, `{"result":"done","permission_denials":"bad"}`, `{"subtype":"error_max_turns","is_error":false,"result":"{}"}`, `{"subtype":"error_during_execution","result":"{}","permission_denials":[]}`} {
		if _, _, err := ReadCodingResult("claude-code", raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestStrictResultStillRejectsDenialsOutsideCoding(t *testing.T) {
	if _, err := ReadResult("claude-code", deniedSuccess, true); err == nil {
		t.Fatal("meta and analysis results must keep rejecting denials")
	}
}

func TestCodingDenialsAreBoundedAndSanitized(t *testing.T) {
	items := []string{}
	for i := 0; i < maxPermissionDenials+5; i++ {
		items = append(items, fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":"echo %d\u0007%s"}}`, i, strings.Repeat("x", 300)))
	}
	raw := `{"is_error":false,"result":"{}","permission_denials":[` + strings.Join(items, ",") + `]}`
	_, denials, err := ReadCodingResult("claude-code", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(denials) != maxPermissionDenials+1 || !strings.HasPrefix(denials[maxPermissionDenials], "5 more denials omitted") {
		t.Fatalf("got %d denials, last %q", len(denials), denials[len(denials)-1])
	}
	for _, d := range denials[:maxPermissionDenials] {
		if len(d) > 204 || strings.ContainsRune(d, 0x07) {
			t.Fatalf("unbounded or unsanitized denial %q", d)
		}
	}
}

func TestExecuteRecordsDenialsOnSuccessfulCoding(t *testing.T) {
	dir := t.TempDir()
	executable := fake(t, "cat > /dev/null\nprintf '%s' '"+deniedSuccess+"'\n")
	r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: dir, Request: map[string]any{"kind": "molecule"}, Timeout: 30 * time.Second})
	if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" {
		t.Fatalf("%+v", r)
	}
	if len(r.Process.PermissionDenials) != 2 {
		t.Fatalf("denials not recorded: %+v", r.Process.PermissionDenials)
	}
}

func TestCodingOutcomeFromProcessKeepsPreciseReason(t *testing.T) {
	out := CodingOutcomeFromProcess(process.Result{Error: "claude-code reported a failure"})
	if out.Outcome != "unknown" || !strings.Contains(out.Reason, "claude-code reported a failure") || strings.Contains(out.Reason, "EOF") {
		t.Fatalf("%+v", out)
	}
	if out := CodingOutcomeFromProcess(process.Result{}); !strings.Contains(out.Reason, "EOF") {
		t.Fatalf("empty result without error should keep decoder reason: %+v", out)
	}
}

func TestDenialSummariesRedactCredentials(t *testing.T) {
	cases := map[string]string{
		`curl -H "Authorization: Bearer abc.def.ghi" https://x`:       "abc.def.ghi",
		`GITHUB_TOKEN=ghp_` + strings.Repeat("a", 36) + ` gh pr list`: strings.Repeat("a", 36),
		`deploy --token s3cr3tvalue --env prod`:                       "s3cr3tvalue",
		`psql "password=hunter2 host=db"`:                             "hunter2",
		`aws s3 ls # AKIAABCDEFGHIJKLMNOP`:                            "AKIAABCDEFGHIJKLMNOP",
	}
	for command, secret := range cases {
		input, _ := json.Marshal(map[string]string{"command": command})
		got := boundedDenial("Bash", input)
		if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%q -> %q still exposes %q", command, got, secret)
		}
	}
	for _, command := range []string{"go test ./internal/cli -run TestAuthTokenRefreshKeepsSessionStaysVerifiable", "go -C lab test ./...", "git diff --stat"} {
		input, _ := json.Marshal(map[string]string{"command": command})
		if got := boundedDenial("Bash", input); got != "Bash: "+command {
			t.Fatalf("benign command altered: %q -> %q", command, got)
		}
	}
}

func TestDenialSummariesStripInvisibleSeparators(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"command": "a\u2028b\u0085c\ufeffd"})
	if got := boundedDenial("Bash", input); got != "Bash: a b c d" {
		t.Fatalf("got %q", got)
	}
}
