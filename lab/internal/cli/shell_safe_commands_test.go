package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

func TestShellSafeCommandsQuoteHostilePaths(t *testing.T) {
	for _, path := range []string{"/tmp/plain", "/tmp/has space", "/tmp/$(touch pwned)", "/tmp/it's `x` \"y\"", "/tmp/new\nline"} {
		want := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
		x := doctorCommands{lab: path}
		for name, got := range map[string]string{
			"recheck": x.recheck(""),
			"harness": x.recheck("odd id"),
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s for %q = %q, lacks %q", name, path, got, want)
			}
		}
		if got := x.candidate(onboarding.Candidate{ID: "codex", Readiness: "blocked"}); !strings.Contains(got, want) {
			t.Fatalf("candidate command = %q, lacks %q", got, want)
		}
	}
	if got := harnessArg("has space"); got != "'has space'" {
		t.Fatalf("harnessArg = %q", got)
	}
	if got := harnessArg("codex"); got != "codex" {
		t.Fatalf("harnessArg = %q", got)
	}
}

func TestShellSafeCommandsDoctorNextCommandQuotesTheLab(t *testing.T) {
	_, parent := doctorFixture(t)
	lab := filepath.Join(parent, "lab $(touch pwned)")
	if err := os.MkdirAll(lab, 0700); err != nil {
		t.Fatal(err)
	}
	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	next, _ := report["nextCommand"].(string)
	if want := "forgecell doctor --lab '" + lab + "'"; !strings.HasPrefix(next, want) {
		t.Fatalf("nextCommand = %q, want prefix %q", next, want)
	}
}
