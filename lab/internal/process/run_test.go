package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("FORGECELL_TEST_PROCESS") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "echo":
		io.Copy(os.Stdout, os.Stdin)
	case "unicode":
		for _, b := range []byte("Lab 🧪 café שלום") {
			os.Stdout.Write([]byte{b})
			os.Stderr.Write([]byte{b})
		}
		os.Exit(7)
	case "tree":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "sentinel")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Fprint(os.Stdout, "child started")
		time.Sleep(30 * time.Second)
	case "sentinel":
		time.Sleep(400 * time.Millisecond)
		os.WriteFile(os.Getenv("FORGECELL_TEST_SENTINEL"), []byte("escaped"), 0600)
	case "wait":
		time.Sleep(30 * time.Second)
	case "flood":
		for i := 0; i < 100000; i++ {
			fmt.Fprint(os.Stdout, "0123456789")
			fmt.Fprint(os.Stderr, "0123456789")
		}
	}
	os.Exit(0)
}
func options(mode string) Options {
	return Options{Argv: []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode}, Env: append(os.Environ(), "FORGECELL_TEST_PROCESS=1"), Timeout: 5 * time.Second}
}
func TestStructuredInputIsData(t *testing.T) {
	o := options("echo")
	o.Input = map[string]string{"ticket": "$(touch should-not-exist); 'quoted'"}
	o.Dir = t.TempDir()
	r := Run(context.Background(), o)
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got["ticket"] != o.Input.(map[string]string)["ticket"] {
		t.Fatal("input changed")
	}
	if entries, _ := os.ReadDir(o.Dir); len(entries) != 0 {
		t.Fatal("input executed")
	}
}
func TestUnicodeAndNonzeroExit(t *testing.T) {
	r := Run(context.Background(), options("unicode"))
	if r.OK || r.Code != 7 || r.Stdout != "Lab 🧪 café שלום" || r.Stderr != r.Stdout {
		t.Fatalf("%+v", r)
	}
}
func TestTimeoutAndCancellation(t *testing.T) {
	o := options("wait")
	o.Timeout = 50 * time.Millisecond
	r := Run(context.Background(), o)
	if r.OK || !r.TimedOut || r.ElapsedMS > 2000 {
		t.Fatalf("%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = Run(ctx, options("wait"))
	if r.OK || !r.Interrupted {
		t.Fatalf("%+v", r)
	}
}
func TestOutputIsBoundedAndOverflowFails(t *testing.T) {
	o := options("flood")
	o.MaxOutputBytes = 1024
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || len(r.Stdout)+len(r.Stderr) > 1024 {
		t.Fatalf("overflow=%v ok=%v bytes=%d", r.Overflow, r.OK, len(r.Stdout)+len(r.Stderr))
	}
}
func TestMissingExecutableAndInvalidInput(t *testing.T) {
	r := Run(context.Background(), Options{Argv: []string{"/nonexistent/forgecell-test"}})
	if r.OK || r.Error == "" {
		t.Fatalf("%+v", r)
	}
	r = Run(context.Background(), Options{})
	if r.OK || !strings.Contains(r.Error, "argv") {
		t.Fatalf("%+v", r)
	}
	o := options("echo")
	o.Input = make(chan int)
	r = Run(context.Background(), o)
	if r.OK || r.Error == "" {
		t.Fatal("invalid JSON input accepted")
	}
}

func TestCancellationStopsDescendants(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process tree cancellation is only supported on release target operating systems")
	}
	marker := filepath.Join(t.TempDir(), "escaped")
	o := options("tree")
	o.Timeout = 200 * time.Millisecond
	o.Env = append(o.Env, "FORGECELL_TEST_SENTINEL="+marker)
	r := Run(context.Background(), o)
	if !r.TimedOut || !strings.Contains(r.Stdout, "child started") {
		t.Fatalf("fixture did not start its child: %+v", r)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation")
	}
}

func TestRawProviderInputIsNotJSONQuoted(t *testing.T) {
	o := options("echo")
	o.Stdin = []byte("Instruction\n{\"kind\":\"molecule\"}\n")
	r := Run(context.Background(), o)
	if !r.OK || r.Stdout != string(o.Stdin) {
		t.Fatalf("%+v", r)
	}
	o.Input = map[string]string{"kind": "molecule"}
	r = Run(context.Background(), o)
	if r.OK {
		t.Fatal("ambiguous raw and JSON input accepted")
	}
}
