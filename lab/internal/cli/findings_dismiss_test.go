package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindingsDismissAndUndismiss(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0, fixLow0, fixHigh1)
	code, before, errOut := runCLI("findings", findingsCLIID, "--lab", fx.lab)
	if code != 0 || strings.Contains(before, "dismissed") {
		t.Fatalf("exit %d\n%s%s", code, before, errOut)
	}

	code, out, errOut := runCLI("findings", findingsCLIID, "--lab", fx.lab, "--dismiss", fixHigh0.fingerprint(), "--reason", "Checked by the caller")
	if want := "Finding " + fixHigh0.fingerprint() + " dismissed; it is left out of the open findings and of fix-round proposals until --undismiss.\n"; code != 0 || out != want || errOut != "" {
		t.Fatalf("exit %d\n%q%q", code, out, errOut)
	}
	code, out, errOut = runCLI("findings", findingsCLIID, "--lab", fx.lab)
	if code != 0 || strings.Contains(out, "Unchecked error") || !strings.Contains(out, "Open: 2 (high 1, medium 0, low 1, info 0); resolved: 0\ndismissed: 1\n") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}

	// A dismissed finding is never proposed, even when included.
	code, out, errOut = fx.propose("--include", fixHigh0.fingerprint())
	if code != 0 || len(fx.amended) != 1 || !strings.HasPrefix(out, "finding "+fixHigh0.fingerprint()+" is dismissed; it is not proposed\n") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{fixHigh1.fingerprint()}; !reflect.DeepEqual(fingerprints(fx.amended[0]), want) {
		t.Fatalf("picked %q", fingerprints(fx.amended[0]))
	}

	code, out, errOut = runCLI("findings", findingsCLIID, "--lab", fx.lab, "--undismiss", fixHigh0.fingerprint())
	if want := "Finding " + fixHigh0.fingerprint() + " is no longer dismissed.\n"; code != 0 || out != want || errOut != "" {
		t.Fatalf("exit %d\n%q%q", code, out, errOut)
	}
	// Without dismissals the text output is exactly what it was.
	if code, out, errOut = runCLI("findings", findingsCLIID, "--lab", fx.lab); code != 0 || out != before {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestFindingsDismissRefusals(t *testing.T) {
	fx := newFixCLILab(t, fixHigh0)
	record := filepath.Join(fx.lab, "deliveries", findingsCLIID+".findings.json")
	original, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	fp := fixHigh0.fingerprint()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--dismiss", fp}, "--dismiss requires --reason"},
		{[]string{"--reason", "why"}, "--reason requires --dismiss"},
		{[]string{"--undismiss", fp, "--reason", "why"}, "--reason requires --dismiss"},
		{[]string{"--dismiss", fp, "--reason", "why", "--collect"}, "used alone"},
		{[]string{"--dismiss", fp, "--reason", "why", "--json"}, "used alone"},
		{[]string{"--undismiss", fp, "--propose"}, "used alone"},
		{[]string{"--dismiss", fp, "--reason", "why", "--undismiss", fp}, "used alone"},
		{[]string{"--dismiss", "nope", "--reason", "why"}, "take a finding fingerprint"},
		{[]string{"--undismiss", strings.ToUpper(fp)}, "take a finding fingerprint"},
		// An empty value is a usage error, never a silent findings listing.
		{[]string{"--undismiss", ""}, "--dismiss and --undismiss take a finding fingerprint"},
		{[]string{"--undismiss="}, "--dismiss and --undismiss take a finding fingerprint"},
		{[]string{"--dismiss", ""}, "--dismiss requires --reason"},
		{[]string{"--dismiss", strings.Repeat("e", 64), "--reason", "why"}, "not in any recorded findings round"},
		{[]string{"--dismiss", fp, "--reason", "bell\x07"}, "without control or format characters"},
		{[]string{"--dismiss", fp, "--reason", strings.Repeat("x", 501)}, "at most 500"},
		{[]string{"--undismiss", fp}, "is not dismissed"},
	} {
		code, out, errOut := runCLI(append([]string{"findings", findingsCLIID, "--lab", fx.lab}, c.args...)...)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "forgecell:") || !strings.Contains(errOut, c.want) || strings.Contains(errOut, fx.lab) {
			t.Fatalf("%q: exit %d\n%q%q", c.args, code, out, errOut)
		}
	}
	if after, _ := os.ReadFile(record); string(after) != string(original) {
		t.Fatal("a refused dismissal changed the findings record")
	}
	// A duplicate dismissal is refused.
	if code, _, errOut := runCLI("findings", findingsCLIID, "--lab", fx.lab, "--dismiss", fp, "--reason", "First"); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if code, _, errOut := runCLI("findings", findingsCLIID, "--lab", fx.lab, "--dismiss", fp, "--reason", "Again"); code != 1 || !strings.Contains(errOut, "already dismissed") {
		t.Fatalf("duplicate: exit %d %s", code, errOut)
	}
}
