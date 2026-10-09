package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLedgerInspectionBoundaries(t *testing.T) {
	const valid = `{"schemaVersion":"v0","kind":"molecule","id":"mol-check","formulaId":"f","status":"waiting","extra":"preserve"}`
	for _, tc := range []struct {
		name      string
		raw       string
		size      int
		kind      string
		wantError string
	}{
		{name: "extensions", raw: valid},
		{name: "historical atom", raw: strings.Replace(valid, `"extra":"preserve"`, `"atoms":[{"id":"old","type":"ship","status":"done"}]`, 1)},
		{name: "atom provenance", raw: strings.Replace(valid, `"extra":"preserve"`, `"atoms":[{"id":"custom","provenance":{"declaredSource":"recipe","actions":[{"attemptId":"a1","resolvedSource":"bound-harness:custom","plannedAction":"invoke-coding-harness","observed":{"action":"invoke-coding-harness","result":"process-succeeded","evidence":[{"path":"result.json","sha256":"hash"}],"extension":true}}]}}]`, 1)},
		{name: "below limit", raw: valid, size: 4_999_999},
		{name: "inclusive limit", raw: valid, size: 5_000_000},
		{name: "over limit", raw: valid, size: 5_000_001, wantError: "exceeds"},
		{name: "symlink", raw: valid, kind: "symlink", wantError: "regular file"},
		{name: "directory", kind: "directory", wantError: "regular file"},
		{name: "fifo", kind: "fifo", wantError: "regular file"},
		{name: "missing", kind: "missing", wantError: "no such file"},
		{name: "malformed", raw: `{`, wantError: "JSON object"},
		{name: "null", raw: `null`, wantError: "JSON object"},
		{name: "schema", raw: strings.Replace(valid, "v0", "v1", 1), wantError: "unsupported"},
		{name: "kind", raw: strings.Replace(valid, "molecule", "formula", 1), wantError: "unsupported"},
		{name: "invalid identity", raw: strings.Replace(valid, "mol-check", "../escape", 1), wantError: "identity"},
		{name: "mismatched identity", raw: strings.Replace(valid, "mol-check", "mol-other", 1), wantError: "id mismatch"},
		{name: "missing identity", raw: strings.Replace(valid, `"id":"mol-check",`, "", 1), wantError: "identity"},
		{name: "missing formula", raw: strings.Replace(valid, `"formulaId":"f",`, "", 1), wantError: "Formula"},
		{name: "missing status", raw: strings.Replace(valid, `"status":"waiting",`, "", 1), wantError: "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "ledgers", "mol-check.json")
			if err := os.Mkdir(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			raw := []byte(tc.raw)
			if tc.size > 0 {
				raw = append(raw, bytes.Repeat([]byte(" "), tc.size-len(raw))...)
			}
			preserved := file
			var err error
			switch tc.kind {
			case "symlink":
				preserved = filepath.Join(dir, "outside.json")
				if err = os.WriteFile(preserved, raw, 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(preserved, file)
			case "directory":
				err = os.Mkdir(file, 0700)
			case "fifo":
				err = syscall.Mkfifo(file, 0600)
			case "missing":
			default:
				err = os.WriteFile(file, raw, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := Run(context.Background(), []string{"ledger", "mol-check", "--lab", dir}, strings.NewReader(""), &out, &stderr, "dev")
			if tc.wantError != "" {
				if code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), tc.wantError) {
					t.Fatalf("code=%d stdout bytes=%d stderr=%q", code, out.Len(), stderr.String())
				}
			} else if code != 0 || stderr.Len() != 0 || !bytes.Equal(out.Bytes(), append(append([]byte(nil), raw...), '\n')) {
				t.Fatalf("code=%d stdout bytes=%d stderr=%q; expected original bytes plus newline", code, out.Len(), stderr.String())
			}
			if tc.kind == "" || tc.kind == "symlink" {
				after, err := os.ReadFile(preserved)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, raw) {
					t.Fatal("inspection changed file contents")
				}
			}
		})
	}
}
