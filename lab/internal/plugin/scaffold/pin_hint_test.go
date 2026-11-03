package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

// The interpreter's owner-or-ancestor refusal and the hint that follows it.
const (
	interpAncestorReason = "the interpreter or a directory above it is owned by another user, or a directory above it is writable by others without the sticky bit"
	interpAncestorHint   = "; package-manager prefixes such as Homebrew's are group-writable, so use the system interpreter, an interpreter installed under your home directory (pyenv, nvm or fnm), or remove group write from the directories above the interpreter"
)

// groupWritablePrefix makes a directory under base that its group can write,
// without the sticky bit, as Homebrew's prefix is.
func groupWritablePrefix(t *testing.T, base string) string {
	t.Helper()
	prefix := filepath.Join(base, "prefix")
	mkdir(t, prefix)
	chmod(t, prefix, 0o775)
	return prefix
}

func TestPinInterpretedHintsAtGroupWritablePrefixes(t *testing.T) {
	base, dir, script, _ := interpFixture(t, "python")
	interp := writeServer(t, groupWritablePrefix(t, base), "python3")

	// The engine's rule still refuses the file, so the hint loosens nothing.
	if err := pinned.CheckPlacement(interp); err == nil {
		t.Fatal("pinned.CheckPlacement accepted a file under a group-writable directory without the sticky bit")
	}

	before := tree(t, base)
	_, err := PinInterpreted(dir, script, interp)
	if err == nil || err.Error() != interpAncestorReason+interpAncestorHint {
		t.Fatalf("got %v, want the ancestor reason and the hint", err)
	}
	if strings.Contains(err.Error(), base) || strings.Contains(interpAncestorHint, "/") {
		t.Fatalf("the refusal names a local path: %v", err)
	}
	if after := tree(t, base); !slices.Equal(before, after) {
		t.Fatalf("the refusal changed the files: %v, was %v", after, before)
	}
}

func TestPinRefusalsWithoutTheHint(t *testing.T) {
	for _, c := range []struct {
		name  string
		lang  string
		setup func(t *testing.T, a *pinArgs)
		want  string
	}{
		{"group-writable interpreter", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.interp, 0o720) },
			"the interpreter is group- or world-writable; make it writable by its owner only"},
		{"world-writable interpreter", "python", func(t *testing.T, a *pinArgs) { chmod(t, a.interp, 0o702) },
			"the interpreter is group- or world-writable; make it writable by its owner only"},
		{"hard-linked interpreter", "python", func(t *testing.T, a *pinArgs) {
			if err := os.Link(a.interp, filepath.Join(a.base, "interp-alias")); err != nil {
				t.Fatal(err)
			}
		}, "the interpreter has more than one hard link; copy it to a private location and pin that copy"},
		{"missing interpreter", "python", func(t *testing.T, a *pinArgs) { a.interp = filepath.Join(a.base, "absent") },
			"the interpreter is missing or unreadable"},
		{"script under a group-writable prefix", "typescript", func(t *testing.T, a *pinArgs) {
			a.script = writeServer(t, groupWritablePrefix(t, a.base), "script")
		}, "the server or a directory above it is owned by another user, or a directory above it is writable by others without the sticky bit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a pinArgs
			a.base, a.dir, a.script, a.interp = interpFixture(t, c.lang)
			c.setup(t, &a)
			before := tree(t, a.base)
			_, err := PinInterpreted(a.dir, a.script, a.interp)
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v, want exactly %q", err, c.want)
			}
			if after := tree(t, a.base); !slices.Equal(before, after) {
				t.Fatalf("the refusal changed the files: %v, was %v", after, before)
			}
		})
	}

	t.Run("server under a group-writable prefix", func(t *testing.T) {
		base, dir, _ := pinFixture(t)
		server := writeServer(t, groupWritablePrefix(t, base), "server")
		want := "the server or a directory above it is owned by another user, or a directory above it is writable by others without the sticky bit"
		if _, err := Pin(dir, server); err == nil || err.Error() != want {
			t.Fatalf("got %v, want exactly %q", err, want)
		}
	})
}

// TestPinInterpretedAcceptsAStickyPrefix shows the rule is unchanged the other
// way: the same prefix with the sticky bit passes, with no hint involved.
func TestPinInterpretedAcceptsAStickyPrefix(t *testing.T) {
	base, dir, script, _ := interpFixture(t, "python")
	prefix := groupWritablePrefix(t, base)
	chmod(t, prefix, 0o775|os.ModeSticky)
	interp := writeServer(t, prefix, "python3")
	if err := pinned.CheckPlacement(interp); err != nil {
		t.Fatalf("pinned.CheckPlacement refused a file under a sticky prefix: %v", err)
	}
	if _, err := PinInterpreted(dir, script, interp); err != nil {
		t.Fatalf("an interpreter under a sticky prefix was refused: %v", err)
	}
}
