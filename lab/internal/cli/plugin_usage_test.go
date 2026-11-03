package cli

import (
	"strings"
	"testing"
)

// The plugin command forms as the help and usage show them, pinned here so a
// change to any shared form constant shows up as a test failure.
const (
	pluginNewForm    = "plugin new --kind tracker|forge|reviewer --lang go|python|typescript [--dir DIR] <name>"
	pluginPinForm    = "plugin pin <dir> --server <path> [--interpreter <path>]"
	pluginTestForm   = "plugin test <manifest> [--json] [--live --target T [--config FILE] [--change REF --commit SHA] [--allow-write --head BRANCH --base BRANCH]]"
	pluginListForm   = "plugin list [--lab DIR] [--lab-name NAME] [--json]"
	pluginDoctorForm = "plugin doctor [NAME] [--lab DIR] [--lab-name NAME] [--json]"
)

func TestPluginUsageNamesEveryCommand(t *testing.T) {
	want := "usage: forgecell " + strings.Join([]string{
		pluginNewForm, pluginPinForm, pluginTestForm, pluginListForm, pluginDoctorForm,
	}, " | forgecell ")
	if pluginUsage != want {
		t.Fatalf("pluginUsage:\n%s\nwant:\n%s", pluginUsage, want)
	}
	for _, args := range [][]string{{"plugin"}, {"plugin", "frobnicate"}} {
		code, out, errOut := runCLI(args...)
		if code != 1 || out != "" || errOut != "forgecell: "+want+"\n" {
			t.Fatalf("%v: exit %d, %q%q", args, code, out, errOut)
		}
		for _, name := range []string{
			"forgecell plugin new ", "forgecell plugin pin ", "forgecell plugin test ", "--live",
			"forgecell plugin list ", "forgecell plugin doctor ",
		} {
			if !strings.Contains(errOut, name) {
				t.Errorf("%v: the usage does not name %q", args, name)
			}
		}
		if strings.Count(errOut, "\n") != 1 {
			t.Errorf("%v: the usage is not one line: %q", args, errOut)
		}
	}
}

func TestPluginUsageIsBuiltFromThePerCommandUsages(t *testing.T) {
	// The per-command usages stay byte-identical.
	for _, c := range []struct{ got, want string }{
		{pluginNewUsage, "usage: forgecell " + pluginNewForm},
		{pluginPinUsage, "usage: forgecell " + pluginPinForm},
		{pluginTestLiveUsage, "usage: forgecell " + pluginTestForm},
	} {
		if c.got != c.want {
			t.Errorf("usage changed: %q, want %q", c.got, c.want)
		}
	}
	rest := strings.TrimPrefix(pluginUsage, "usage: ")
	for _, u := range []string{pluginNewUsage, pluginPinUsage, pluginTestLiveUsage} {
		form, ok := strings.CutPrefix(u, "usage: ")
		if !ok {
			t.Fatalf("%q lacks the usage: prefix", u)
		}
		if !strings.Contains(rest, form) {
			t.Errorf("pluginUsage lacks %q", form)
		}
	}
	// The forms appear in the order new, pin, test, list, doctor.
	parts := strings.Split(rest, " | ")
	order := []string{pluginNewForm, pluginPinForm, pluginTestForm, pluginListForm, pluginDoctorForm}
	if len(parts) != len(order) {
		t.Fatalf("pluginUsage has %d commands: %q", len(parts), parts)
	}
	for i, form := range order {
		if parts[i] != "forgecell "+form {
			t.Errorf("command %d is %q, want %q", i, parts[i], "forgecell "+form)
		}
	}
}

func TestHelpListsEveryPluginCommand(t *testing.T) {
	code, out, _ := runCLI("--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(out, "\n")
	at := -1
	for i, line := range lines {
		if line == pluginNewForm {
			at = i
			break
		}
	}
	if at < 1 || at+5 >= len(lines) {
		t.Fatalf("help has no plugin new line:\n%s", out)
	}
	// The plugin lines replace the old plugin test line in place: labs comes
	// before them and ui after them, as before.
	want := []string{"labs [--json]", pluginNewForm, pluginPinForm, pluginTestForm, pluginListForm, pluginDoctorForm}
	for i, w := range want {
		if got := lines[at-1+i]; got != w {
			t.Errorf("help line %d is %q, want %q", at+i, got, w)
		}
	}
	if !strings.HasPrefix(lines[at+5], "ui [") {
		t.Errorf("help line after the plugin lines is %q", lines[at+5])
	}
	for _, form := range want[1:] {
		if strings.Count(out, form+"\n") != 1 {
			t.Errorf("help lists %q other than once", form)
		}
	}
	// The plugin test line still names the base form.
	if !strings.Contains(out, "plugin test <manifest> [--json]") {
		t.Error("help does not list plugin test")
	}
}

func TestPluginNewAndPinUsageErrorsPrintTheirOwnUsage(t *testing.T) {
	for _, c := range []struct {
		args  []string
		usage string
	}{
		{[]string{"plugin", "new"}, pluginNewUsage},
		{[]string{"plugin", "new", "--kind", "tracker", "x"}, pluginNewUsage},
		{[]string{"plugin", "pin"}, pluginPinUsage},
		{[]string{"plugin", "pin", "./rv", "--server"}, pluginPinUsage},
	} {
		code, out, errOut := runCLI(c.args...)
		if code != 1 || out != "" || errOut != "forgecell: "+c.usage+"\n" {
			t.Errorf("%v: exit %d, %q%q", c.args, code, out, errOut)
		}
		if strings.Contains(errOut, " | ") {
			t.Errorf("%v: printed the all-commands usage: %q", c.args, errOut)
		}
	}
}
