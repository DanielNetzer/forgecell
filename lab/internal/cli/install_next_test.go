package cli

import (
	"os"
	"strings"
	"testing"
)

func TestInstallNextStepsPointsAtUI(t *testing.T) {
	text := installNextSteps()
	for _, want := range []string{"cd into your repository", "forgecell ui", "Set up your Lab", "forgecell doctor"} {
		if !strings.Contains(text, want) {
			t.Fatalf("installer next step lacks %q:\n%s", want, text)
		}
	}
	for _, stale := range []string{"forgecell init", "approve Formula", "forgecell run", "→"} {
		if strings.Contains(text, stale) {
			t.Fatalf("installer next step still holds stale %q:\n%s", stale, text)
		}
	}
	if strings.Index(text, "forgecell ui") > strings.Index(text, "forgecell doctor") {
		t.Fatalf("forgecell ui must be the next step, doctor only a hint:\n%s", text)
	}
	if strings.Contains(text, "/Users/") || strings.Contains(text, "/home/") || strings.Contains(text, "$") {
		t.Fatalf("installer next step leaks a local path or variable:\n%s", text)
	}
	for _, value := range []string{os.Getenv("HOME"), os.Getenv("FORGECELL_HOME"), os.Getenv("FORGECELL_BIN")} {
		if value != "" && strings.Contains(text, value) {
			t.Fatalf("installer next step leaks an environment value:\n%s", text)
		}
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("installer next step must end its line: %q", text)
	}
}
