// Package testenv lets a test declare the environment it needs, so a test run inside a
// sandbox that denies it skips with a reason instead of failing.
package testenv

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// SandboxDeniesVariable lists, comma-separated, the capabilities the Codex coding sandbox
// denies (#403). The Lab sets it only inside a Codex coding run; CI and the Lab's checks
// never set it, so the tests that need those capabilities still run there.
const SandboxDeniesVariable = "FORGECELL_SANDBOX_DENIES"

// SandboxDenies reports whether FORGECELL_SANDBOX_DENIES lists capability, with the skip
// reason that names it.
func SandboxDenies(capability string) (reason string, denied bool) {
	if !slices.Contains(strings.Split(os.Getenv(SandboxDeniesVariable), ","), capability) || capability == "" {
		return "", false
	}
	return "the sandbox denies " + capability + " (" + SandboxDeniesVariable + " lists it); the Lab runs this test outside the sandbox", true
}

// SkipIfSandboxDenies skips t when FORGECELL_SANDBOX_DENIES lists capability.
func SkipIfSandboxDenies(t testing.TB, capability string) {
	t.Helper()
	if reason, denied := SandboxDenies(capability); denied {
		t.Skip(reason)
	}
}
