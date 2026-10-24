package demo

import (
	"fmt"
	"os"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// Hostile stub modes the failure-path tests bind instead of StubArg. The test
// binary serves them, and StubArg itself, before any test runs: it is the stub
// harness executable of every demo run in this package.
const (
	testMalformedStub    = "__demo-test-malformed"
	testOutsideScopeStub = "__demo-test-outside-scope"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case StubArg:
			os.Exit(ServeStub(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case testMalformedStub:
			// Not a coding outcome: a missing reason and paths, then trailing data.
			fmt.Print(`{"schemaVersion":"v1","outcome":"completed"} {"approved":true}`)
			os.Exit(0)
		case testOutsideScopeStub:
			dir, err := os.Getwd()
			if err == nil {
				err = applyCannedChange(dir)
			}
			if err == nil {
				err = writeNewFile(dir, "notes.txt", []byte("written outside the approved scope\n"), 0o644)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Print(stubOutcome)
			os.Exit(0)
		}
	}
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// Demo Labs keep their check TMPDIRs under a private root that is removed
	// afterwards, never in the user's real temp root.
	cleanup, err := resources.IsolateLabTempRoot("demo-labtmp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()
	return m.Run()
}
