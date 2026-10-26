package verification

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func acceptObservation(stdout string) Observation {
	return Observation{ID: "regression", Category: "regression", Result: process.Result{Reconciled: true, Code: 1, Stdout: stdout}}
}

func TestAcceptProtectedFailuresHelperReturnsTheParsedFailingSet(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"run","Package":"example.test/pkg","Test":"TestB"}`,
		`{"Action":"fail","Package":"example.test/pkg","Test":"TestB"}`,
		`{"Action":"run","Package":"example.test/pkg","Test":"TestA"}`,
		`{"Action":"fail","Package":"example.test/pkg","Test":"TestA"}`,
		`{"Action":"pass","Package":"example.test/pkg","Test":"TestC"}`,
		`{"Action":"fail","Package":"example.test/pkg"}`,
	}, "\n")
	got, err := ProtectedFailuresForDeltas(acceptObservation(stream))
	if err != nil {
		t.Fatal(err)
	}
	want := []TestID{{Package: "example.test/pkg", Test: "TestA"}, {Package: "example.test/pkg", Test: "TestB"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAcceptProtectedFailuresHelperRefusesUncleanFailures(t *testing.T) {
	clean := `{"Action":"fail","Package":"example.test/pkg","Test":"TestPinned"}` + "\n" + `{"Action":"fail","Package":"example.test/pkg"}` + "\n"
	output := func(pkg, test, text string) string {
		if test == "" {
			return `{"Action":"output","Package":"` + pkg + `","Output":"` + text + `\n"}` + "\n"
		}
		return `{"Action":"output","Package":"` + pkg + `","Test":"` + test + `","Output":"` + text + `\n"}` + "\n"
	}
	mutate := func(f func(*Observation)) Observation {
		o := acceptObservation(clean)
		f(&o)
		return o
	}
	for name, c := range map[string]struct {
		obs  Observation
		want string
	}{
		"timed out":       {mutate(func(o *Observation) { o.Result.TimedOut = true }), "incomplete"},
		"parent timeout":  {mutate(func(o *Observation) { o.Result.ParentDeadline = true }), "incomplete"},
		"interrupted":     {mutate(func(o *Observation) { o.Result.Interrupted = true }), "incomplete"},
		"overflow":        {mutate(func(o *Observation) { o.Result.Overflow = true }), "incomplete"},
		"unreconciled":    {mutate(func(o *Observation) { o.Result.Reconciled = false }), "incomplete"},
		"killed":          {mutate(func(o *Observation) { o.Result.Code = -1 }), "failing status"},
		"passed":          {mutate(func(o *Observation) { o.Result.OK, o.Result.Code = true, 0 }), "no failure to accept"},
		"truncated":       {mutate(func(o *Observation) { o.Result.Truncated = true }), "truncated"},
		"empty stream":    {acceptObservation(""), "no go test -json events"},
		"not json":        {acceptObservation("FAIL example.test/pkg\n"), "not go test -json"},
		"cut mid-line":    {acceptObservation(clean + `{"Action":"fail","Pack`), "not go test -json"},
		"build failure":   {acceptObservation(`{"Action":"build-fail","ImportPath":"example.test/pkg"}` + "\n"), "build failure"},
		"panic":           {acceptObservation(output("example.test/pkg", "TestPinned", "panic: boom") + clean), "stopped early"},
		"timeout marker":  {acceptObservation(output("example.test/pkg", "", "panic: test timed out after 10m0s") + clean), "stopped early"},
		"abnormal exit":   {acceptObservation(output("example.test/pkg", "", "signal: killed") + clean), "stopped early"},
		"unfinished":      {acceptObservation(`{"Action":"run","Package":"example.test/pkg","Test":"TestHang"}` + "\n" + clean), "never finished"},
		"package only":    {acceptObservation(`{"Action":"fail","Package":"example.test/pkg"}` + "\n"), "without a failing test"},
		"no failing test": {acceptObservation(`{"Action":"pass","Package":"example.test/pkg","Test":"TestPinned"}` + "\n"), "without any failing test"},
	} {
		got, err := ProtectedFailuresForDeltas(c.obs)
		if err == nil || !strings.Contains(err.Error(), c.want) || got != nil {
			t.Fatalf("%s: got %+v, %v; want a refusal naming %q", name, got, err, c.want)
		}
	}
}

func TestAcceptProtectedFailuresKeepsTheCandidateViewError(t *testing.T) {
	failed := Result{Error: "verification command go-test-lab modified source or wrote outside artifact allowances"}
	keepCandidateError(&failed)
	if failed.CandidateError != failed.Error {
		t.Fatalf("candidate error %q", failed.CandidateError)
	}
	silent := Result{}
	keepCandidateError(&silent)
	if silent.CandidateError == "" {
		t.Fatal("a failed candidate view without an error was recorded as a pass")
	}
	passed := Result{RequiredChecksPassed: true}
	keepCandidateError(&passed)
	if passed.CandidateError != "" {
		t.Fatalf("a passing candidate view recorded %q", passed.CandidateError)
	}
}
