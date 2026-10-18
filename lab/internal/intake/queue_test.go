package intake

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func queueGH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}
func TestIssueQueueRemote(t *testing.T) {
	issue := `{"number":1,"title":"hello","state":"open","html_url":"https://github.com/o/r/issues/1"}`
	for _, tc := range []struct {
		raw   string
		bad   bool
		count int
		more  bool
	}{
		{"[" + issue + "]", false, 1, true}, {`[{"pull_request":{}}]`, false, 0, true}, {`[]`, false, 0, false},
		{`null`, true, 0, false}, {`{}`, true, 0, false}, {`[`, true, 0, false}, {"[" + issue + "," + issue + "]", true, 0, false},
		{strings.Replace("["+issue+"]", "o/r", "x/r", 1), true, 0, false}, {`[{"pull_request":"bad"}]`, true, 0, false},
		{strings.Repeat(" ", 1_000_001), true, 0, false},
	} {
		queueGH(t, "[ \"$#\" = 6 ] && [ \"$1 $2 $3 $4 $5 $6\" = 'api --hostname github.com --method GET repos/o/r/issues?state=open&per_page=1&page=2' ] || exit 9\ncat <<'DATA'\n"+tc.raw+"\nDATA\n")
		p, err := ReadIssueQueue(context.Background(), "o/r", 2, 1, "")
		if (err != nil) != tc.bad {
			t.Fatalf("%v: %v", tc.raw, err)
		}
		if !tc.bad && (len(p.Issues) != tc.count || p.MorePossible != tc.more || p.RemoteItems != tc.count && !tc.more) {
			t.Fatalf("%+v", p)
		}
	}
}
func TestIssueQueueSelectionAndFailures(t *testing.T) {
	queueGH(t, "[ \"$1 $2 $3 $4 $5 $6\" = 'api --hostname github.com --method GET repos/o/r/issues/1' ] || exit 9\nprintf '%s' '{\"number\":1,\"title\":\"closed\",\"state\":\"closed\",\"html_url\":\"https://github.com/O/R/issues/1\"}'\n")
	p, err := ReadIssueQueue(context.Background(), "o/r", 1, 30, "#1")
	if err != nil || !p.Selected || p.MorePossible || len(p.Issues) != 1 || p.Issues[0].State != "CLOSED" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, ref := range []string{"x/r#1", "0", "bad"} {
		if _, err := ReadIssueQueue(context.Background(), "o/r", 1, 30, ref); err == nil {
			t.Fatal(ref)
		}
	}
	for _, pair := range [][2]int{{0, 30}, {1, 0}, {1, 101}} {
		if _, err := ReadIssueQueue(context.Background(), "o/r", pair[0], pair[1], ""); err == nil {
			t.Fatal(pair)
		}
	}
	queueGH(t, "exit 1\n")
	if _, err := ReadIssueQueue(context.Background(), "o/r", 1, 30, ""); err == nil {
		t.Fatal("failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadIssueQueue(ctx, "o/r", 1, 30, ""); err == nil {
		t.Fatal("cancel hidden")
	}
	queueGH(t, "printf '%s' '{\"pull_request\":{}}'\n")
	if _, err := ReadIssueQueue(context.Background(), "o/r", 1, 30, "1"); err == nil {
		t.Fatal("selected PR")
	}
}

func TestIssueQueueCancellationStopsPipeHoldingDescendant(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix process groups")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("QUEUE_CHILD_PID", pidFile)
	// The descendant outlives every failure deadline below, so it can only stop
	// because cancellation killed it. The pid is renamed into place so a reader
	// never sees a partly written file.
	queueGH(t, "sleep 120 &\necho $! > \"$QUEUE_CHILD_PID.tmp\"\nmv \"$QUEUE_CHILD_PID.tmp\" \"$QUEUE_CHILD_PID\"\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ReadIssueQueue(ctx, "o/r", 1, 30, ""); done <- err }()
	// Readiness is the pid-file marker; the deadline is reached only on failure.
	var pid int
	for deadline := time.Now().Add(30 * time.Second); ; {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if pid > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatal("fake gh returned before starting its descendant", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fake gh did not start descendant")
		}
		time.Sleep(10 * time.Millisecond)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "GitHub data unavailable") {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation did not return")
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		raw, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(raw))
		var exitErr *exec.ExitError
		if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && state == "") {
			t.Fatal("cannot inspect owned descendant", err)
		}
		if state == "" || strings.HasPrefix(state, "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant still executing", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
