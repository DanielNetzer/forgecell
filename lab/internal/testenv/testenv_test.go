package testenv

import (
	"os"
	"strings"
	"testing"
)

func TestSandboxDeniesReadsTheVariable(t *testing.T) {
	for _, c := range []struct {
		value  string
		set    bool
		denied bool
	}{
		{"", false, false},
		{"", true, false},
		{"ps", true, true},
		{"localhost-bind,ps", true, true},
		{"localhost-bind,spawn", true, false},
		{"psx", true, false},
	} {
		// t.Setenv restores the original value when the test ends, also after Unsetenv.
		t.Setenv(SandboxDeniesVariable, c.value)
		if !c.set {
			os.Unsetenv(SandboxDeniesVariable)
		}
		reason, denied := SandboxDenies("ps")
		if denied != c.denied {
			t.Fatalf("%q: denied = %v, want %v", c.value, denied, c.denied)
		}
		if denied && (!strings.Contains(reason, "ps") || !strings.Contains(reason, SandboxDeniesVariable)) {
			t.Fatalf("%q: reason does not name ps and the variable: %q", c.value, reason)
		}
		if !denied && reason != "" {
			t.Fatalf("%q: reason without a denial: %q", c.value, reason)
		}
	}
	t.Setenv(SandboxDeniesVariable, "ps")
	if _, denied := SandboxDenies(""); denied {
		t.Fatal("an empty capability was denied")
	}
}

func TestSkipIfSandboxDeniesSkipsOnlyListedCapability(t *testing.T) {
	for _, c := range []struct {
		value   string
		skipped bool
	}{{"", false}, {"localhost-bind", false}, {"ps", true}, {"spawn,ps", true}} {
		t.Setenv(SandboxDeniesVariable, c.value)
		var ran bool
		t.Run("ps", func(t *testing.T) {
			SkipIfSandboxDenies(t, "ps")
			ran = true
			if c.skipped {
				t.Error("test ran although ps is denied")
			}
		})
		if ran == c.skipped {
			t.Fatalf("%q: ran = %v, want skipped = %v", c.value, ran, c.skipped)
		}
	}
}
