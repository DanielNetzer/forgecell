package verification

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonEvents(t *testing.T, events ...map[string]string) string {
	t.Helper()
	var lines []string
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	return strings.Join(lines, "\n") + "\n"
}

func failingTestStream(t *testing.T, test, output string, packageOutput ...string) string {
	t.Helper()
	events := []map[string]string{
		{"Action": "run", "Package": "p", "Test": test},
		{"Action": "output", "Package": "p", "Test": test, "Output": output},
		{"Action": "fail", "Package": "p", "Test": test},
	}
	for _, out := range packageOutput {
		events = append(events, map[string]string{"Action": "output", "Package": "p", "Output": out})
	}
	events = append(events, map[string]string{"Action": "fail", "Package": "p"})
	return jsonEvents(t, events...)
}

func TestAbortMarkerScopeIgnoresTestLogText(t *testing.T) {
	for _, output := range []string{
		"    a_test.go:3: panic: not a real panic\n",
		"    a_test.go:3: fatal error: logged text\n",
		"    a_test.go:3: test timed out after 5s (logged)\n",
		"  panic: indented\n",
	} {
		failed, err := parseFailedTests(failingTestStream(t, "TestA", output))
		if err != nil || len(failed) != 1 || failed[0] != (TestID{Package: "p", Test: "TestA"}) {
			t.Fatalf("logged %q must not abort the run: %v %v", output, failed, err)
		}
	}
}

func TestAbortMarkerScopeStillRefusesRealAborts(t *testing.T) {
	cases := map[string]string{
		"runtime panic in a test":  failingTestStream(t, "TestA", "panic: boom [recovered]\n"),
		"fatal error in a test":    failingTestStream(t, "TestA", "fatal error: all goroutines are asleep - deadlock!\n"),
		"timeout line in a test":   failingTestStream(t, "TestA", "test timed out after 10m0s\n"),
		"package-level panic":      failingTestStream(t, "TestA", "ok\n", "panic: boom\n"),
		"build failure":            failingTestStream(t, "TestA", "ok\n", "FAIL\tp [build failed]\n"),
		"setup failure":            failingTestStream(t, "TestA", "ok\n", "FAIL\tp [setup failed]\n"),
		"killed binary":            failingTestStream(t, "TestA", "ok\n", "signal: killed\n"),
		"nonstandard exit status":  failingTestStream(t, "TestA", "ok\n", "exit status 2\n"),
		"package-level timed out":  failingTestStream(t, "TestA", "ok\n", "test timed out after 1s\n"),
		"package-level fatal":      failingTestStream(t, "TestA", "ok\n", "fatal error: out of memory\n"),
		"panic printed by program": failingTestStream(t, "TestA", "panic: printed at line start\n"),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			if failed, err := parseFailedTests(stream); err == nil {
				t.Fatalf("a real abort must refuse the run, got %v", failed)
			}
		})
	}
}
