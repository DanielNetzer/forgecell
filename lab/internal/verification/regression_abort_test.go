package verification

import (
	"strings"
	"testing"
)

func events(lines ...string) string { return strings.Join(lines, "\n") }

// A panic stops the package: tests after it never start, so a declared failing
// test must not be accepted as the package's only failure.
func TestDeltaParsingRejectsAbortedPackagesAndUnfinishedTests(t *testing.T) {
	const pkg = `"Package":"example.test/p"`
	for name, out := range map[string]string{
		"panic":      events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"output",`+pkg+`,"Test":"TestA","Output":"panic: boom\n"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`}`),
		"timeout":    events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"output",`+pkg+`,"Output":"panic: test timed out after 1s\n"}`, `{"Action":"fail",`+pkg+`}`),
		"unfinished": events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"run",`+pkg+`,"Test":"TestB"}`, `{"Action":"fail",`+pkg+`}`),
	} {
		if _, err := parseFailedTests(out); err == nil {
			t.Fatalf("%s: aborted package accepted", name)
		}
	}
	// A passing package may legitimately print panic text (for example tests of recover).
	ok := events(`{"Action":"run",`+pkg+`,"Test":"TestRecover"}`, `{"Action":"output",`+pkg+`,"Test":"TestRecover","Output":"panic: recovered\n"}`, `{"Action":"pass",`+pkg+`,"Test":"TestRecover"}`,
		`{"Action":"run","Package":"example.test/q","Test":"TestPinned"}`, `{"Action":"fail","Package":"example.test/q","Test":"TestPinned"}`, `{"Action":"fail","Package":"example.test/q"}`, `{"Action":"pass",`+pkg+`}`)
	got, err := parseFailedTests(ok)
	if err != nil || len(got) != 1 || got[0].Test != "TestPinned" {
		t.Fatalf("clean declared failure rejected: %v %v", got, err)
	}
}
