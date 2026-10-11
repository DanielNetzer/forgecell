package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

func stubLoop(t *testing.T, fn func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error)) {
	t.Helper()
	previous := runLoop
	runLoop = fn
	t.Cleanup(func() { runLoop = previous })
}

// loopFakeGH puts a gh on PATH that records that it was called.
func loopFakeGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "gh-called")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho called >> '"+marker+"'\nprintf '[]'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return marker
}

func runLoopCLI(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), append([]string{"loop"}, args...), nil, &out, &stderr, "test")
	return code, out.String(), stderr.String()
}

func TestLoopHelpListsTheCommand(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, nil, &out, &stderr, "test"); code != 0 {
		t.Fatal(stderr.String())
	}
	for _, want := range []string{"loop [--lab DIR] [--select oldest|label:L|milestone:M] [--max-concurrent N] [--once]", "never delivers, merges or approves a human gate"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help does not mention %q", want)
		}
	}
}

func TestLoopRejectsInvalidArgumentsBeforeAnyEffect(t *testing.T) {
	marker := loopFakeGH(t)
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		t.Error("the loop ran despite invalid arguments")
		return molecule.LoopReport{}, nil
	})
	lab := filepath.Join(t.TempDir(), "absent")
	for _, extra := range [][]string{
		{"--select", "newest"},
		{"--select", "label:"},
		{"--select", "milestone:"},
		{"--select", "label:   "},
		{"--select", "oldest:x"},
		{"--select", ""},
		{"--max-concurrent", "0"},
		{"--max-concurrent", "-3"},
		{"--max-concurrent", "many"},
		{"stray"},
		{"--once", "stray"},
		{"--once=maybe"},
		{"--bogus"},
	} {
		code, out, stderr := runLoopCLI(append([]string{"--lab", lab}, extra...)...)
		if code != 1 || out != "" || stderr == "" {
			t.Fatalf("%v: exit %d, output %q, stderr %q", extra, code, out, stderr)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("GitHub was called for an invalid invocation")
	}
	if _, err := os.Stat(lab); !os.IsNotExist(err) {
		t.Fatal("a Lab was created for an invalid invocation")
	}
}

func TestLoopWithoutAnApprovedFormulaExitsOneWithoutCallingGitHub(t *testing.T) {
	marker := loopFakeGH(t)
	lab := t.TempDir()
	code, out, stderr := runLoopCLI("--lab", lab, "--once")
	if code != 1 || !strings.Contains(stderr, "approve a Formula") || out != "" {
		t.Fatalf("exit %d, output %q, stderr %q", code, out, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("GitHub was called without an approved Formula")
	}
	if _, err := os.Stat(filepath.Join(lab, "ledgers")); !os.IsNotExist(err) {
		t.Fatal("a ledger directory was created without an approved Formula")
	}
}

func TestLoopPassesOptionsAndMapsExitCodes(t *testing.T) {
	lab := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var got molecule.LoopOptions
	var failure error
	report := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{
		Items: []molecule.LoopItem{
			{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11-a", Started: true, Approved: true, State: molecule.LoopWaiting, Reason: "waiting at the review gate"},
			{Repository: "acme/widgets", Issue: 12, MoleculeID: "mol-12-b", State: molecule.LoopWaiting, Reason: "the scope gate needs a human"},
		},
		Notes: []string{"ledger inventory: sample diagnostic"},
	}}}
	stubLoop(t, func(_ context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		got = o
		return report, failure
	})

	code, out, stderr := runLoopCLI("--lab", lab, "--select", "milestone:v1", "--max-concurrent", "3", "--once")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got.Template.LabDir != lab || got.Template.SourceRoot != cwd || got.Select != (intake.Selector{Kind: "milestone", Value: "v1"}) || got.MaxConcurrent != 3 || !got.Once {
		t.Fatalf("%+v", got)
	}
	for _, want := range []string{"Lab loop for acme/widgets · select milestone:v1 · max concurrent 3", "acme/widgets#11 · waiting · Molecule mol-11-a · started intake · approved by policy", "waiting at the review gate", "acme/widgets#12 · waiting · Molecule mol-12-b\n  the scope gate needs a human", "Warning: ledger inventory: sample diagnostic"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
	}

	if code, _, _ = runLoopCLI("--lab", lab); code != 0 || got.Select != (intake.Selector{Kind: "oldest"}) || got.MaxConcurrent != 1 || got.Once {
		t.Fatalf("defaults: exit %d, %+v", code, got)
	}

	report.Passes[0].Items[1].Failed = true
	if code, _, _ = runLoopCLI("--lab", lab, "--once"); code != 2 {
		t.Fatalf("a failed Molecule exited %d, want 2", code)
	}
	report.Passes[0].Items[1].Failed = false

	failure = errors.New("GitHub data unavailable")
	code, out, stderr = runLoopCLI("--lab", lab, "--once")
	if code != 1 || !strings.Contains(stderr, "GitHub data unavailable") || !strings.Contains(out, "acme/widgets#11") {
		t.Fatalf("a loop-level error exited %d: %q %q", code, out, stderr)
	}
}

func TestLoopResolvesTheDefaultLab(t *testing.T) {
	lab := t.TempDir()
	t.Setenv("FORGECELL_LAB", lab)
	var got molecule.LoopOptions
	calls := 0
	stubLoop(t, func(_ context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		got = o
		calls++
		return molecule.LoopReport{}, nil
	})
	if code, _, stderr := runLoopCLI("--once"); code != 0 || got.Template.LabDir != lab {
		t.Fatalf("exit %d, Lab %q, stderr %q", code, got.Template.LabDir, stderr)
	}
	if code, _, stderr := runLoopCLI("--lab-name", "second", "--once"); code != 0 || got.Template.LabDir != lab {
		t.Fatalf("--lab-name was not accepted: exit %d, stderr %q", code, stderr)
	}
	before := calls
	if code, _, _ := runLoopCLI("--lab-name", "not valid!", "--once"); code != 1 || calls != before {
		t.Fatalf("an invalid Lab name exited %d", code)
	}
}
