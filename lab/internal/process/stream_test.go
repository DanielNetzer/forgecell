package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestStreamHelperProcess(t *testing.T) {
	mode := os.Getenv("FORGECELL_TEST_STREAM")
	if mode == "" {
		return
	}
	count, _ := strconv.Atoi(os.Getenv("FORGECELL_TEST_COUNT"))
	switch mode {
	case "lines":
		w := bufio.NewWriter(os.Stdout)
		for i := 0; i < count; i++ {
			fmt.Fprintf(w, "line-%05d\n", i)
		}
		w.Flush()
	case "noeol":
		os.Stdout.WriteString("first\nsecond")
	case "longline":
		os.Stdout.WriteString(strings.Repeat("x", count) + "\n")
	case "forever":
		line := strings.Repeat("y", 1023) + "\n"
		for {
			if _, err := os.Stdout.WriteString(line); err != nil {
				os.Exit(3)
			}
		}
	case "stderr":
		os.Stderr.WriteString(strings.Repeat("e", count))
		os.Stdout.WriteString("ok\n")
	}
	os.Exit(0)
}

func streamOptions(mode string, count int) Options {
	return Options{Argv: []string{os.Args[0], "-test.run=^TestStreamHelperProcess$"}, Env: append(os.Environ(), "FORGECELL_TEST_STREAM="+mode, "FORGECELL_TEST_COUNT="+strconv.Itoa(count)), Timeout: successBackstop}
}

func collect(into *[]string) *LineSink {
	return &LineSink{Line: func(line []byte) error {
		*into = append(*into, string(line))
		return nil
	}}
}

func TestLineSinkReceivesTheCompleteStreamPastTheRetainedCap(t *testing.T) {
	o := streamOptions("lines", 5000)
	o.MaxOutputBytes = 1000
	var got []string
	o.StdoutLines = collect(&got)
	r := Run(context.Background(), o)
	if !r.OK || r.Overflow || !r.Truncated {
		t.Fatalf("a streamed run past the retained cap must succeed and report truncation: %+v", r)
	}
	if len(r.Stdout) != 1000 || r.Activity == nil || r.Activity.StdoutBytes != 55000 {
		t.Fatalf("the retained capture must stay capped while activity counts every byte: %d %+v", len(r.Stdout), r.Activity)
	}
	if len(got) != 5000 || got[0] != "line-00000" || got[2500] != "line-02500" || got[4999] != "line-04999" {
		t.Fatalf("the sink did not receive the complete ordered stream: %d lines", len(got))
	}
}

func TestLineSinkDoesNotMarkAnUntruncatedRunTruncated(t *testing.T) {
	o := streamOptions("lines", 10)
	var got []string
	o.StdoutLines = collect(&got)
	r := Run(context.Background(), o)
	if !r.OK || r.Truncated || len(got) != 10 {
		t.Fatalf("%+v %v", r, got)
	}
}

func TestLineSinkDeliversAFinalLineWithoutNewline(t *testing.T) {
	o := streamOptions("noeol", 0)
	var got []string
	o.StdoutLines = collect(&got)
	r := Run(context.Background(), o)
	if !r.OK || len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("%+v %q", r, got)
	}
}

func TestLineSinkBoundsLineLength(t *testing.T) {
	o := streamOptions("longline", 5000)
	var got []string
	o.StdoutLines = collect(&got)
	o.StdoutLines.MaxLineBytes = 1000
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || !strings.Contains(r.Error, "line") {
		t.Fatalf("an over-long line must fail closed: %+v", r)
	}
	o.StdoutLines.MaxLineBytes = 10000
	got = nil
	if r = Run(context.Background(), o); !r.OK || len(got) != 1 {
		t.Fatalf("a line within the bound must pass: %+v", r)
	}
}

func TestLineSinkHardBoundStopsTheProcess(t *testing.T) {
	o := streamOptions("forever", 0)
	var got []string
	o.StdoutLines = collect(&got)
	o.StdoutLines.MaxBytes = 100000
	// The fixture never exits on its own, so an overflow that is not a timeout
	// proves the byte bound, not the backstop, stopped it; no wall-clock
	// measurement is needed.
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || r.TimedOut || r.Interrupted || !strings.Contains(r.Error, "stream") {
		t.Fatalf("an unbounded stream must be stopped and fail closed: %+v", r)
	}
	if r.Activity == nil || r.Activity.StdoutBytes < 100000 {
		t.Fatalf("activity %+v", r.Activity)
	}
}

func TestLineSinkErrorFailsClosedAndStopsTheProcess(t *testing.T) {
	o := streamOptions("lines", 100000)
	seen := 0
	o.StdoutLines = &LineSink{Line: func([]byte) error {
		seen++
		if seen == 3 {
			return errors.New("sink refused the stream")
		}
		return nil
	}}
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || !strings.Contains(r.Error, "sink refused the stream") {
		t.Fatalf("a sink error must fail the run closed: %+v", r)
	}
	if seen != 3 {
		t.Fatalf("the sink was called %d times after refusing", seen)
	}
}

func TestLineSinkLimitsAreBoundedByTheHardLimits(t *testing.T) {
	if StreamHardLimitBytes != 64<<20 || StreamMaxLineBytes != 1<<20 {
		t.Fatalf("hard limits changed: %d %d", StreamHardLimitBytes, StreamMaxLineBytes)
	}
	for name, c := range map[string]struct {
		sink     LineSink
		bytes    int64
		lineSize int
	}{
		"defaults":      {LineSink{}, StreamHardLimitBytes, StreamMaxLineBytes},
		"smaller":       {LineSink{MaxBytes: 10, MaxLineBytes: 5}, 10, 5},
		"above the cap": {LineSink{MaxBytes: 1 << 40, MaxLineBytes: 1 << 30}, StreamHardLimitBytes, StreamMaxLineBytes},
		"negative":      {LineSink{MaxBytes: -1, MaxLineBytes: -1}, StreamHardLimitBytes, StreamMaxLineBytes},
	} {
		if bytes, line := c.sink.limits(); bytes != c.bytes || line != c.lineSize {
			t.Fatalf("%s: limits = %d, %d", name, bytes, line)
		}
	}
}

func TestStderrStaysCappedWhenStdoutIsStreamed(t *testing.T) {
	o := streamOptions("stderr", 5000)
	o.MaxOutputBytes = 1000
	var got []string
	o.StdoutLines = collect(&got)
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow {
		t.Fatalf("a streamed stdout must not relax the stderr cap: %+v", r)
	}
}

func TestWithoutASinkTheCapAndOverflowAreUnchanged(t *testing.T) {
	o := streamOptions("lines", 5000)
	o.MaxOutputBytes = 1000
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || r.Truncated || len(r.Stdout) > 1000 {
		t.Fatalf("callers that do not opt in must keep the cap and overflow behavior: %+v", r)
	}
}
