package onboarding

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// pendingView reads the pending proposal back and returns its kind and changes.
func pendingView(t *testing.T, dir, id string) (string, []KeyChange) {
	t.Helper()
	p, err := PendingProposal(dir)
	if err != nil || p == nil || p.ID != id {
		t.Fatalf("pending %+v %v, want %s", p, err, id)
	}
	before, ok := ProposalBefore(dir, *p)
	if !ok {
		t.Fatal("the approved Formula the proposal changes was not established")
	}
	changes, truncated, err := ProposalDiff(before, p.YAML)
	if err != nil || truncated {
		t.Fatalf("diff %v truncated %v", err, truncated)
	}
	lines, truncated := LineDiff(before, p.YAML)
	if truncated || !strings.Contains(fmt.Sprint(lines), "+") {
		t.Fatalf("line diff %v truncated %v", lines, truncated)
	}
	return ProposalKind(changes, true), changes
}

func hasChange(changes []KeyChange, key, change string) bool {
	for _, c := range changes {
		if c.Key == key && c.Change == change {
			return true
		}
	}
	return false
}

func TestProposalViewReadableDiffForEachKind(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		root, runner, dir := refreshLab(t)
		commitWorkflow(t, root, refreshWorkflow("go test -timeout 20m ./..."))
		out, err := refresh(root, runner, dir)
		if err != nil || out.Proposal == nil {
			t.Fatalf("%+v %v", out, err)
		}
		kind, changes := pendingView(t, dir, out.Proposal.ID)
		if kind != KindBaseline || len(changes) == 0 {
			t.Fatalf("kind %s changes %+v", kind, changes)
		}
		for _, c := range changes {
			if !strings.HasPrefix(c.Key, "requiredChecks") {
				t.Fatalf("a baseline refresh changed %s", c.Key)
			}
		}
	})
	t.Run("enable-review", func(t *testing.T) {
		root, runner, dir := reviewLab(t, "claude-code")
		out, err := enableReview(root, runner, dir)
		if err != nil || out.Proposal == nil {
			t.Fatalf("%+v %v", out, err)
		}
		kind, changes := pendingView(t, dir, out.Proposal.ID)
		if kind != KindEnableReview || !hasChange(changes, "atoms[review].type", "added") {
			t.Fatalf("kind %s changes %+v", kind, changes)
		}
	})
	t.Run("model-pin", func(t *testing.T) {
		root, runner, dir := claudeLab(t, nil)
		out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Runner: runner, Model: pinnedModel, ModelSet: true})
		if err != nil || out.Proposal == nil {
			t.Fatalf("%+v %v", out, err)
		}
		kind, changes := pendingView(t, dir, out.Proposal.ID)
		if kind != KindModelPin || !hasChange(changes, "harness.model", "added") {
			t.Fatalf("kind %s changes %+v", kind, changes)
		}
		for _, c := range changes {
			if c.Key == "harness.model" && c.New != `"`+pinnedModel+`"` {
				t.Fatalf("new value %s", c.New)
			}
		}
	})
	t.Run("rebind", func(t *testing.T) {
		root, runner, dir, _ := approvedLab(t)
		out, err := rebind(t, root, runner, dir, "claude-code")
		if err != nil || out.Proposal == nil {
			t.Fatalf("%+v %v", out, err)
		}
		kind, changes := pendingView(t, dir, out.Proposal.ID)
		if kind != KindRebind || !hasChange(changes, "harness.binding", "changed") {
			t.Fatalf("kind %s changes %+v", kind, changes)
		}
		for _, c := range changes {
			if c.Key == "harness.binding" && (c.Old != `"codex"` || c.New != `"claude-code"`) {
				t.Fatalf("binding %s -> %s", c.Old, c.New)
			}
		}
	})
}

func TestPendingProposalIsNilWithoutProposals(t *testing.T) {
	if p, err := PendingProposal(t.TempDir()); err != nil || p != nil {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestProposalBeforeRefusesAnUnmatchedBefore(t *testing.T) {
	p := Proposal{ActiveHashBefore: hash([]byte("a")), CapabilityEvidence: &BindingReport{BeforeYAML: "b"}}
	if _, ok := ProposalBefore(t.TempDir(), p); ok {
		t.Fatal("a before YAML whose hash differs from the recorded one was used")
	}
	if _, ok := ProposalBefore(t.TempDir(), Proposal{}); ok {
		t.Fatal("a proposal without an approved Formula before it has a before")
	}
}

func TestProposalDiffBoundsFailSafe(t *testing.T) {
	var before, after strings.Builder
	for i := 0; i < maxKeyChanges+20; i++ {
		fmt.Fprintf(&before, "k%d: 1\n", i)
		fmt.Fprintf(&after, "k%d: 2\n", i)
	}
	changes, truncated, err := ProposalDiff(before.String(), after.String())
	if err != nil || !truncated || len(changes) != maxKeyChanges {
		t.Fatalf("%d changes, truncated %v, %v", len(changes), truncated, err)
	}
	long := strings.Repeat("é", maxDiffValueBytes)
	changes, truncated, err = ProposalDiff("k: a\n", "k: "+long+"\n")
	if err != nil || !truncated || len(changes) != 1 || !changes[0].Truncated || len(changes[0].New) > maxDiffValueBytes+len("…") || !strings.HasSuffix(changes[0].New, "…") {
		t.Fatalf("%+v %v %v", changes, truncated, err)
	}
	if _, _, err = ProposalDiff("k: [", "k: 1"); err == nil {
		t.Fatal("invalid YAML was diffed")
	}
	var big strings.Builder
	for i := 0; i < maxDiffLines+10; i++ {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	lines, truncated := LineDiff("a\n", big.String())
	if !truncated || len(lines) > maxDiffOutput {
		t.Fatalf("%d lines, truncated %v", len(lines), truncated)
	}
	lines, truncated = LineDiff("a\nb\nc\nd\ne\nf\ng\nh\ni\n", "a\nb\nc\nd\nE\nf\ng\nh\ni\n")
	want := []DiffLine{{"…", ""}, {" ", "b"}, {" ", "c"}, {" ", "d"}, {"-", "e"}, {"+", "E"}, {" ", "f"}, {" ", "g"}, {" ", "h"}, {"…", ""}}
	if truncated || fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("%v", lines)
	}
}

func TestProposalKindNamesOnlyExactShapes(t *testing.T) {
	for want, changes := range map[string][]KeyChange{
		KindBaseline:     {{Key: "requiredChecks[go-test].argv", Change: "changed"}},
		KindEnableReview: {{Key: "atoms[review].id", Change: "added"}, {Key: "atoms[review].type", Change: "added"}},
		KindModelPin:     {{Key: "harness.model", Change: "added"}, {Key: "harnesses.coding.model", Change: "changed"}},
		KindRebind:       {{Key: "harness.binding", Change: "changed"}, {Key: "atoms[learn].command", Change: "changed"}},
		KindChange:       {{Key: "harness.binding", Change: "changed"}, {Key: "requiredChecks", Change: "added"}},
	} {
		if got := ProposalKind(changes, true); got != want {
			t.Errorf("%+v: kind %s, want %s", changes, got, want)
		}
	}
	if ProposalKind(nil, false) != KindBootstrap {
		t.Error("a proposal with nothing before it is a bootstrap")
	}
	if _, err := formula.Parse([]byte(roleFormula(""))); err != nil {
		t.Fatal(err)
	}
}
