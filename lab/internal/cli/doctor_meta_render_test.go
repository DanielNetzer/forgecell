package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

// The human doctor view must report the meta harness like the JSON view does.
func TestHumanDoctorShowsMetaHarness(t *testing.T) {
	var out bytes.Buffer
	meta := onboarding.MetaReport{Binding: "claude-code", Capability: harness.Capability{State: "conditional", Reason: "learning can be attempted"}, NextAction: "forgecell init --meta-harness claude-code"}
	renderDoctor(&out, doctorView{Meta: &meta, Lab: "/lab", Note: "note"})
	for _, want := range []string{"Meta harness: claude-code — conditional: learning can be attempted", "  Next: forgecell init --meta-harness claude-code"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderDoctor(&out, doctorView{Meta: &onboarding.MetaReport{}, Lab: "/lab", Note: "note"})
	if strings.Contains(out.String(), "Meta harness:") {
		t.Fatalf("unbound meta harness rendered:\n%s", out.String())
	}
}
