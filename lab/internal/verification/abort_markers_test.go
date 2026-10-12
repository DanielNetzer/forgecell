package verification

import (
	"strings"
	"testing"
)

func TestHardening129AbnormalExitMarkersStopDeltaAcceptance(t *testing.T) {
	const pkg = `"Package":"example.test/p"`
	failedA := []string{`{"Action":"run",` + pkg + `,"Test":"TestA"}`, `{"Action":"fail",` + pkg + `,"Test":"TestA"}`}
	for name, marker := range map[string]string{
		"killed":            "signal: killed\n",
		"segfault":          "signal: segmentation fault\n",
		"os.Exit(2)":        "exit status 2\n",
		"os.Exit(3)":        "exit status 3\n",
		"os.Exit(0)":        "exit status 0\n",
		"killed in a test":  "signal: killed",
		"padded exit":       "  exit status 137  \n",
		"after other lines": "FAIL\nexit status 7\n",
	} {
		lines := append([]string{}, failedA...)
		lines = append(lines, `{"Action":"output",`+pkg+`,"Output":`+quoteJSON(marker)+`}`, `{"Action":"fail",`+pkg+`}`)
		if _, err := parseFailedTests(events(lines...)); err == nil || !strings.Contains(err.Error(), "stopped early") {
			t.Fatalf("%s: a package that ended abnormally was accepted: %v", name, err)
		}
	}
}

func TestHardening129OrdinaryFailureStillAcceptsTheDeclaredDelta(t *testing.T) {
	const pkg = `"Package":"example.test/p"`
	ordinary := events(
		`{"Action":"run",`+pkg+`,"Test":"TestA"}`,
		`{"Action":"output",`+pkg+`,"Test":"TestA","Output":"--- FAIL: TestA (0.00s)\n"}`,
		`{"Action":"fail",`+pkg+`,"Test":"TestA"}`,
		`{"Action":"output",`+pkg+`,"Output":"FAIL\n"}`,
		`{"Action":"output",`+pkg+`,"Output":"exit status 1\n"}`,
		`{"Action":"output",`+pkg+`,"Output":"FAIL\texample.test/p\t0.01s\n"}`,
		`{"Action":"fail",`+pkg+`}`,
	)
	got, err := parseFailedTests(ordinary)
	if err != nil || len(got) != 1 || got[0].Test != "TestA" {
		t.Fatalf("exit status 1 accompanies every ordinary failure and must not be an abort marker: %v %v", got, err)
	}
	unrelated := events(
		`{"Action":"run",`+pkg+`,"Test":"TestA"}`,
		`{"Action":"output",`+pkg+`,"Test":"TestA","Output":"    a_test.go:9: saw signal: killed in a log line\n"}`,
		`{"Action":"fail",`+pkg+`,"Test":"TestA"}`,
		`{"Action":"fail",`+pkg+`}`,
	)
	if got, err = parseFailedTests(unrelated); err != nil || len(got) != 1 {
		t.Fatalf("only a whole line is a marker, not text a test logs: %v %v", got, err)
	}
}

func TestHardening129AbnormalExitMarkerMatchesOnlyWholeLines(t *testing.T) {
	for output, want := range map[string]bool{
		"signal: killed\n": true, "exit status 2\n": true, "exit status 1\n": false, "exit status\n": false,
		"exit status two\n": false, "got exit status 2\n": false, "ok\n": false, "": false, "FAIL\nsignal: aborted\n": true,
	} {
		if _, got := abnormalExitMarker(output); got != want {
			t.Fatalf("abnormalExitMarker(%q) = %t, want %t", output, got, want)
		}
	}
}

func quoteJSON(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`).Replace(s) + `"`
}
