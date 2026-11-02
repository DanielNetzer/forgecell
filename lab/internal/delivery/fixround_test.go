package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const fixRoundTestURL = "https://github.com/owner/repo/pull/10"

// fixRoundFixture is a published delivery, through GitHub or a plugin forge,
// with a real bare origin. Its runner runs git for real against the bare
// repository and answers gh from the pushed commit; argv records every remote
// call of the push under test.
type fixRoundFixture struct {
	*pluginPublishFixture
	t         *testing.T
	plugin    bool
	argv      [][]string
	view      func(pushed string) string
	workspace string
	// beforePush runs once a git push reaches the runner, before it runs.
	beforePush func()
}

type fixRoundGates struct{ unverified bool }

func newFixRoundFixture(t *testing.T, plugin bool) *fixRoundFixture {
	t.Helper()
	fx := &fixRoundFixture{pluginPublishFixture: newPluginPublishFixture(t), t: t, plugin: plugin}
	fx.workspace = filepath.Join(fx.o.LabDir, "workspaces", "fixture")
	var err error
	if plugin {
		_, err = fx.publish()
	} else {
		_, err = Publish(context.Background(), PublishOptions{Options: fx.o, Approve: fx.digest, Title: "Fixture", Runner: fx.run})
	}
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	fx.argv = nil
	return fx
}

// run is the remote runner: git against the bare origin, gh answered.
func (fx *fixRoundFixture) run(ctx context.Context, c process.Options) process.Result {
	fx.argv = append(fx.argv, append([]string(nil), c.Argv...))
	if c.Argv[0] != "gh" {
		if fx.beforePush != nil && len(c.Argv) > 1 && c.Argv[1] == "push" {
			fx.beforePush()
		}
		return fx.runner(ctx, c)
	}
	switch {
	case c.Argv[1] == "api" && strings.Contains(c.Argv[4], "/rules/"):
		return process.Result{OK: true, Stdout: "[]"}
	case c.Argv[1] == "api":
		return process.Result{OK: true, Stdout: "{}"}
	case c.Argv[2] == "list":
		return process.Result{OK: true, Stdout: "[]"}
	case c.Argv[2] == "create":
		return process.Result{OK: true, Stdout: fixRoundTestURL + "\n"}
	case c.Argv[2] == "view":
		if fx.view != nil {
			return process.Result{OK: true, Stdout: fx.view(fx.pushed)}
		}
		return process.Result{OK: true, Stdout: fx.prJSON(fx.pushed, "OPEN")}
	}
	fx.t.Fatalf("unexpected remote call %q", c.Argv)
	return process.Result{}
}

func (fx *fixRoundFixture) prJSON(head, state string) string {
	return lifecycleJSON(Receipt{URL: fixRoundTestURL, Commit: head, Preview: PreviewResult{BaseBranch: "main", Branch: "forgecell/fixture"}}, state)
}

func (fx *fixRoundFixture) options(approve string, confirm bool) FixRoundPushOptions {
	o := FixRoundPushOptions{LabDir: fx.o.LabDir, MoleculeID: fx.o.MoleculeID, Approve: approve, Confirm: confirm, Runner: fx.run, Now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }}
	if fx.plugin {
		o.Changes, o.Plugin = fx.fake, PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: fx.calls}
	}
	return o
}

func (fx *fixRoundFixture) git(args ...string) string {
	fx.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", fx.workspace}, args...)...).CombinedOutput()
	if err != nil {
		fx.t.Fatalf("git %q: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (fx *fixRoundFixture) receipt() Receipt {
	fx.t.Helper()
	r, err := ReadFixRoundReceipt(fx.o.LabDir, fx.o.MoleculeID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func (fx *fixRoundFixture) record() molecule.Record {
	fx.t.Helper()
	r, err := readFixtureRecord(fx.o)
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func (fx *fixRoundFixture) writeRecord(r molecule.Record) {
	fx.t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.o.LabDir, "ledgers", fx.o.MoleculeID+".json"), raw, 0600); err != nil {
		fx.t.Fatal(err)
	}
}

// fixRoundFingerprint is the finding fixture round n answers.
func fixRoundFingerprint(n int) string { return strings.Repeat(fmt.Sprint(n), 64) }

// prepare records fix round n as the ledger would after its run: a fix-round
// revision on the receipt head, approved, coded (a.txt rewritten in the
// workspace) and verified, with a review pass unless gates say otherwise.
func (fx *fixRoundFixture) prepare(n int, gates fixRoundGates) {
	t := fx.t
	t.Helper()
	head, headTree := ReceiptHead(fx.receipt())
	r := fx.record()
	s := *r.Readiness
	previous := s.Plans[len(s.Plans)-1]
	p := previous.Plan
	p.Revision, p.ParentDigest, p.PausedTree = previous.Plan.Revision+1, previous.Digest, headTree
	fp := fixRoundFingerprint(n)
	p.FixRound = &readiness.FixRoundRef{Round: n, Head: head, HeadTree: headTree, Findings: []readiness.FixRoundFinding{{Fingerprint: fp, Reviewer: "0:critic", Severity: "high", Path: "a.txt", Line: 1, Title: "Fix a"}}, IssueBodySHA256: readiness.IssueBodySHA256(p.Inputs.Issue.Body)}
	// The previous round's finding citations are not this round's findings.
	kept := []readiness.Criterion{}
	for _, c := range p.Analysis.Acceptance {
		if !strings.HasPrefix(c.Evidence[0], readiness.FindingEvidencePrefix) {
			kept = append(kept, c)
		}
	}
	p.Analysis.Acceptance = append(kept, readiness.Criterion{Description: fmt.Sprintf("Fix the reviewer finding [high] a.txt:1: Fix a round %d (finding:%s)", n, fp), Evidence: []string{readiness.FindingEvidenceID(fp)}})
	var err error
	if s, err = s.Amend(p, "amend"); err != nil {
		t.Fatalf("amend: %v", err)
	}
	d := s.Plans[len(s.Plans)-1].Digest
	if s, err = s.Approve(d, p.Inputs, "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	s, _ = s.StartAttempt("start")
	s, _ = s.FinishAttempt("check-pending", "captured", "end")
	if s, err = s.RecordVerification(true, "fix round verified", "checked"); err != nil {
		t.Fatal(err)
	}
	r.Readiness = &s
	if err := os.WriteFile(filepath.Join(fx.workspace, "a.txt"), []byte(fmt.Sprintf("fixed %d\n", n)), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := verification.Capture(context.Background(), fx.workspace, head, []string{"a.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("fix-%d", n)
	report := `{"schemaVersion":"v1","outcome":"completed","reason":"Fixed","paths":[]}`
	coding := harness.CodingOutcomeFromResult(report)
	result := process.Result{OK: true, Reconciled: true, Stdout: report}
	evidence, _ := json.Marshal(struct {
		process.Result
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}{result, id, d})
	path := filepath.ToSlash(filepath.Join("verification-history", r.ID, "harness-"+id+"-result.json"))
	if err := os.WriteFile(filepath.Join(fx.o.LabDir, path), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	result.Stdout = ""
	r.HarnessAttempts = append(r.HarnessAttempts, molecule.HarnessAttempt{ID: id, PlanDigest: d, Result: result, Coding: &coding, Evidence: &molecule.VerificationFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(evidence))}, Capture: &c})
	r.Capture = &c
	r.Verification = &verification.Result{SourceTree: c.Tree, RequiredChecksPassed: true, IndependentAcceptanceVerified: !gates.unverified, Checks: []verification.Observation{{ID: "acceptance", Category: "independent-acceptance", SourceTree: c.Tree, Command: p.Analysis.Checks[0], Result: process.Result{OK: true}}}}
	r.VerificationAttempts = append(r.VerificationAttempts, molecule.VerificationAttempt{PlanDigest: d, SourceTree: c.Tree, StartedAt: "checked", Status: "passed"})
	// The fixture's Formula has no review Atom, and a record's Atoms are exactly
	// its Formula's, so a pass here never makes the review green: every push of
	// this fixture needs --confirm. TestFixRoundReviewGreen covers the green rule.
	r.ReviewAttempts = append(r.ReviewAttempts, molecule.ReviewAttempt{Ordinal: len(r.ReviewAttempts), PlanDigest: d, Tree: c.Tree, Status: molecule.ReviewNoHighFindings})
	fx.writeRecord(r)
}

func TestFixRoundReviewGreen(t *testing.T) {
	digest, tree := strings.Repeat("d", 64), hexCommit("a")
	withAtom := []molecule.Atom{{ID: "coding", Type: "harness"}, {ID: "review", Type: "review"}}
	pass := molecule.ReviewAttempt{PlanDigest: digest, Tree: tree, Status: molecule.ReviewNoHighFindings}
	for _, tc := range []struct {
		name     string
		atoms    []molecule.Atom
		attempts []molecule.ReviewAttempt
		green    bool
	}{
		{"green", withAtom, []molecule.ReviewAttempt{pass}, true},
		{"no review Atom", withAtom[:1], []molecule.ReviewAttempt{pass}, false},
		{"no pass", withAtom, nil, false},
		{"latest pass blocked", withAtom, []molecule.ReviewAttempt{pass, {PlanDigest: digest, Tree: tree, Status: molecule.ReviewBlocked}}, false},
		{"pass of another plan", withAtom, []molecule.ReviewAttempt{{PlanDigest: strings.Repeat("e", 64), Tree: tree, Status: molecule.ReviewNoHighFindings}}, false},
		{"pass on another tree", withAtom, []molecule.ReviewAttempt{{PlanDigest: digest, Tree: hexCommit("b"), Status: molecule.ReviewNoHighFindings}}, false},
		{"a later plan's pass does not hide this plan's block", withAtom, []molecule.ReviewAttempt{{PlanDigest: digest, Tree: tree, Status: molecule.ReviewFailed}, {PlanDigest: strings.Repeat("e", 64), Tree: tree, Status: molecule.ReviewNoHighFindings}}, false},
	} {
		if got := fixRoundReviewGreen(molecule.Record{Atoms: tc.atoms, ReviewAttempts: tc.attempts}, digest, tree); got != tc.green {
			t.Fatalf("%s: green = %t", tc.name, got)
		}
	}
	if (FixRoundPreview{ReviewGreen: true}).NeedsConfirm() || !(FixRoundPreview{}).NeedsConfirm() || !(FixRoundPreview{ReviewGreen: true, AcceptanceUnverified: true}).NeedsConfirm() {
		t.Fatal("NeedsConfirm differs from its gates")
	}
}

// approved previews the round and pushes exactly its digest.
func (fx *fixRoundFixture) approved(confirm bool) (Receipt, FixRoundPreview, error) {
	fx.t.Helper()
	_, err := PushFixRound(context.Background(), fx.options("", false))
	var pending *FixRoundApprovalRequired
	if !errors.As(err, &pending) {
		fx.t.Fatalf("preview: %v", err)
	}
	r, err := PushFixRound(context.Background(), fx.options(pending.Preview.Digest, confirm))
	return r, pending.Preview, err
}

// pushArgv is every git push the runner saw.
func (fx *fixRoundFixture) pushArgv() [][]string {
	var out [][]string
	for _, a := range fx.argv {
		if len(a) > 1 && a[0] == "git" && a[1] == "push" {
			out = append(out, a)
		}
	}
	return out
}

func assertNoForcePush(t *testing.T, argv [][]string) {
	t.Helper()
	for _, a := range argv {
		for _, arg := range a {
			if arg == "-f" || strings.HasPrefix(arg, "--force") || (strings.HasPrefix(arg, "+") && strings.Contains(arg, ":")) {
				t.Fatalf("a force push: %q", a)
			}
		}
	}
}

func eachForge(t *testing.T, test func(t *testing.T, plugin bool)) {
	for _, plugin := range []bool{false, true} {
		name := "github"
		if plugin {
			name = "plugin"
		}
		t.Run(name, func(t *testing.T) { test(t, plugin) })
	}
}

func TestFixRoundPushTwoStepDigestAndRounds(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		head, headTree := ReceiptHead(fx.receipt())
		fx.prepare(1, fixRoundGates{})
		before := fx.receipt()

		_, err := PushFixRound(context.Background(), fx.options("", false))
		var pending *FixRoundApprovalRequired
		if !errors.As(err, &pending) || !strings.Contains(err.Error(), pending.Preview.Digest) {
			t.Fatalf("without an approval: %v", err)
		}
		p := pending.Preview
		r := fx.record()
		latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		want := FixRoundPreview{MoleculeID: fx.o.MoleculeID, Repo: "owner/repo", Branch: "forgecell/fixture", Parent: head, Tree: r.Verification.SourceTree, Files: []string{"a.txt"}, PlanDigest: latest.Digest, Round: 1, Findings: []string{fixRoundFingerprint(1)}, Title: "fix: address reviewer findings, round 1 (#9)"}
		want.Digest = fixRoundPreviewDigest(want)
		if !reflect.DeepEqual(p, want) || !p.NeedsConfirm() || p.Tree == headTree {
			t.Fatalf("preview = %+v\nwant %+v", p, want)
		}
		if preview, err := PreviewFixRound(context.Background(), fx.options("", false)); err != nil || !reflect.DeepEqual(preview, p) {
			t.Fatalf("PreviewFixRound = %+v, %v", preview, err)
		}
		if len(fx.argv) != 0 || fx.git("rev-parse", "HEAD") != head {
			t.Fatalf("the preview ran %q or committed", fx.argv)
		}

		// A digest other than the preview's is stale: nothing is committed or pushed.
		if _, err := PushFixRound(context.Background(), fx.options(strings.Repeat("0", 64), true)); err == nil || !strings.Contains(err.Error(), "approval is stale") {
			t.Fatalf("a stale digest: %v", err)
		}
		if len(fx.argv) != 0 || fx.git("rev-parse", "HEAD") != head || !sameReceipt(fx.receipt(), before) {
			t.Fatal("a stale digest committed, pushed or recorded")
		}

		out, err := PushFixRound(context.Background(), fx.options(p.Digest, true))
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		commit := fx.git("rev-parse", "HEAD")
		if len(out.FixRounds) != 1 {
			t.Fatalf("fix rounds = %+v", out.FixRounds)
		}
		fr := out.FixRounds[0]
		if fr.Round != 1 || fr.Parent != head || fr.Commit != commit || fr.Tree != p.Tree || fr.VerificationTree != p.Tree || fr.PlanDigest != p.PlanDigest || fr.ObservedHead != commit || !reflect.DeepEqual(fr.Findings, p.Findings) || fr.PushedAt != "2026-11-02T12:00:00Z" {
			t.Fatalf("fix round = %+v", fr)
		}
		if got, _ := ReceiptHead(fx.receipt()); got != commit {
			t.Fatalf("receipt head = %s, want %s", got, commit)
		}
		if remote, err := exec.Command("git", "-C", fx.bare, "rev-parse", "refs/heads/forgecell/fixture").Output(); err != nil || strings.TrimSpace(string(remote)) != commit {
			t.Fatalf("origin branch = %s %v", remote, err)
		}
		if fx.git("rev-list", "--parents", "-n", "1", commit) != commit+" "+head || fx.git("show", "-s", "--format=%B", commit) != p.Title {
			t.Fatal("the fix-round commit has another parent or message")
		}
		if want := [][]string{{"git", "push", "origin", commit + ":refs/heads/forgecell/fixture"}}; !reflect.DeepEqual(fx.pushArgv(), want) {
			t.Fatalf("pushes = %q", fx.pushArgv())
		}
		if plugin {
			tools := pluginTools(out.PluginCalls)
			// The change is read before the commit and re-read after the push.
			if len(tools) < 3 || !reflect.DeepEqual(tools[len(tools)-3:], []string{"forge_describe", "forge_change", "forge_change"}) {
				t.Fatalf("plugin calls = %q", tools)
			}
			if len(fx.fake.published) != 1 {
				t.Fatal("a fix round published a draft")
			}
		} else {
			for _, a := range fx.argv {
				if a[0] == "gh" && (a[2] != "view" || len(a) < 4 || a[3] != fixRoundTestURL) {
					t.Fatalf("a fix round called %q", a)
				}
			}
		}

		// Round 2 builds on round 1's commit.
		fx.argv = nil
		fx.prepare(2, fixRoundGates{})
		out, _, err = fx.approved(true)
		if err != nil {
			t.Fatalf("round 2: %v", err)
		}
		second := fx.git("rev-parse", "HEAD")
		if len(out.FixRounds) != 2 || out.FixRounds[1].Parent != commit || out.FixRounds[1].Commit != second || out.FixRounds[1].Round != 2 {
			t.Fatalf("round 2 = %+v", out.FixRounds)
		}
		if got, _ := ReceiptHead(fx.receipt()); got != second {
			t.Fatalf("receipt head after round 2 = %s", got)
		}
		assertNoForcePush(t, fx.pushArgv())
	})
}

func TestFixRoundPushRefusesBeforeAnyProcess(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fx *fixRoundFixture)
		want   string
	}{
		{"demo Lab", func(fx *fixRoundFixture) {
			os.WriteFile(filepath.Join(fx.o.LabDir, "lab.json"), []byte(`{"activeFormulaId":"forgecell-demo"}`), 0600)
		}, "demo Lab"},
		{"unpublished receipt", func(fx *fixRoundFixture) {
			r := fx.receipt()
			r.State = "committed"
			writeReceipt(fx.o, r)
		}, "not published"},
		{"latest plan without a fix round", func(fx *fixRoundFixture) {
			r := fx.record()
			r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.FixRound = nil
			fx.writeRecord(r)
		}, "not a fix round"},
		{"fix round on another head", func(fx *fixRoundFixture) {
			r := fx.record()
			r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.FixRound.Head = strings.Repeat("e", 40)
			fx.writeRecord(r)
		}, "propose the fix round again"},
		{"another round", func(fx *fixRoundFixture) {
			r := fx.record()
			r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.FixRound.Round = 2
			fx.writeRecord(r)
		}, "next fix round is 1"},
		{"more than three rounds", func(fx *fixRoundFixture) {
			r := fx.receipt()
			parent := r.Commit
			for i, c := range []string{"1", "2", "3"} {
				fr := FixRound{Round: i + 1, Parent: parent, Commit: hexCommit(c), Tree: hexCommit("e"), PlanDigest: strings.Repeat("d", 64), VerificationTree: hexCommit("e"), Findings: []string{}, PushedAt: "2026-11-02T12:00:00Z", ObservedHead: hexCommit(c)}
				r.FixRounds = append(r.FixRounds, fr)
				parent = fr.Commit
			}
			writeReceipt(fx.o, r)
		}, "already has 3 fix rounds"},
		{"verification not passed", func(fx *fixRoundFixture) {
			r := fx.record()
			r.Verification.RequiredChecksPassed = false
			fx.writeRecord(r)
		}, "no passing verification"},
		{"verification of another plan", func(fx *fixRoundFixture) {
			r := fx.record()
			r.VerificationAttempts[len(r.VerificationAttempts)-1].Status = "failed"
			fx.writeRecord(r)
		}, "no passing verification"},
		{"unverified acceptance without confirm", func(fx *fixRoundFixture) {
			r := fx.record()
			r.Verification.IndependentAcceptanceVerified = false
			fx.writeRecord(r)
		}, "independent acceptance of fix round 1 is unverified"},
		// The fixture's Formula has no review Atom, so its review is never green.
		{"no review Atom without confirm", func(*fixRoundFixture) {}, "no review Atom"},
	}
	for _, tc := range cases {
		eachForge(t, func(t *testing.T, plugin bool) {
			t.Run(tc.name, func(t *testing.T) {
				fx := newFixRoundFixture(t, plugin)
				head := fx.git("rev-parse", "HEAD")
				fx.prepare(1, fixRoundGates{})
				p, err := PreviewFixRound(context.Background(), fx.options("", false))
				if err != nil {
					t.Fatal(err)
				}
				receiptPath := filepath.Join(fx.o.LabDir, "deliveries", fx.o.MoleculeID+".json")
				tc.mutate(fx)
				before, _ := os.ReadFile(receiptPath)
				fx.order = nil
				_, err = PushFixRound(context.Background(), fx.options(p.Digest, false))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				if strings.Contains(err.Error(), fx.o.LabDir) {
					t.Fatalf("the refusal names the Lab: %v", err)
				}
				if len(fx.argv) != 0 || len(fx.order) != 0 || fx.git("rev-parse", "HEAD") != head {
					t.Fatalf("refused after remote calls %q / %q or a commit", fx.argv, fx.order)
				}
				if after, _ := os.ReadFile(receiptPath); string(after) != string(before) {
					t.Fatal("a refusal changed the receipt")
				}
			})
		})
	}
}

func TestFixRoundPushRefusesAWorkspaceThatIsNotTheVerifiedTree(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		head := fx.git("rev-parse", "HEAD")
		fx.prepare(1, fixRoundGates{})
		if err := os.WriteFile(filepath.Join(fx.workspace, "a.txt"), []byte("unverified\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := PushFixRound(context.Background(), fx.options("", false))
		if err == nil || !strings.Contains(err.Error(), "not the verified tree") {
			t.Fatalf("err = %v", err)
		}
		if len(fx.argv) != 0 || fx.git("rev-parse", "HEAD") != head {
			t.Fatal("a changed workspace was committed or pushed")
		}
	})
}

func TestFixRoundPushConfirmGates(t *testing.T) {
	for _, gates := range []fixRoundGates{{unverified: true}, {}} {
		eachForge(t, func(t *testing.T, plugin bool) {
			fx := newFixRoundFixture(t, plugin)
			fx.prepare(1, gates)
			p, err := PreviewFixRound(context.Background(), fx.options("", false))
			if err != nil || !p.NeedsConfirm() || p.AcceptanceUnverified != gates.unverified || p.ReviewGreen {
				t.Fatalf("preview = %+v, %v", p, err)
			}
			if _, err := PushFixRound(context.Background(), fx.options("", false)); err == nil || !strings.Contains(err.Error(), "with --confirm") {
				t.Fatalf("the pending push does not ask for --confirm: %v", err)
			}
			if _, err := PushFixRound(context.Background(), fx.options(p.Digest, false)); err == nil || !strings.Contains(err.Error(), "--confirm") || len(fx.argv) != 0 {
				t.Fatalf("without --confirm: %v, calls %q", err, fx.argv)
			}
			out, err := PushFixRound(context.Background(), fx.options(p.Digest, true))
			if err != nil || len(out.FixRounds) != 1 {
				t.Fatalf("with --confirm: %+v, %v", out.FixRounds, err)
			}
		})
	}
}

func TestFixRoundPushRejectedAsNotAFastForward(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		head, headTree := ReceiptHead(fx.receipt())
		fx.prepare(1, fixRoundGates{})
		// Someone else moves the branch past the head after the pre-commit
		// checks, just before the push.
		other := fx.git("commit-tree", headTree, "-p", head, "-m", "someone else")
		fx.beforePush = func() {
			fx.beforePush = nil
			if out, err := exec.Command("git", "-C", fx.workspace, "push", fx.bare, other+":refs/heads/forgecell/fixture").CombinedOutput(); err != nil {
				t.Fatalf("move the branch: %v %s", err, out)
			}
		}
		before := fx.receipt()
		_, p, err := fx.approved(true)
		if !errors.Is(err, ErrFixRoundBranchMoved) || err.Error() != "the change request's branch moved; collect again" {
			t.Fatalf("err = %v", err)
		}
		if !sameReceipt(fx.receipt(), before) {
			t.Fatal("a rejected push was recorded")
		}
		if plugin && fx.order[len(fx.order)-1] != "git push" {
			t.Fatalf("a rejected push went on to %q", fx.order)
		}
		assertNoForcePush(t, fx.pushArgv())

		// The commit is kept, and a retry reuses it rather than committing again.
		commit := fx.git("rev-parse", "HEAD")
		if commit == head {
			t.Fatal("the fix-round commit was not kept")
		}
		again, err := PreviewFixRound(context.Background(), fx.options("", false))
		if err != nil || again.Digest != p.Digest {
			t.Fatalf("retry preview = %+v, %v", again, err)
		}
		// The retry finds the branch moved before it pushes anything.
		fx.argv = nil
		if _, err := PushFixRound(context.Background(), fx.options(again.Digest, true)); !errors.Is(err, ErrFixRoundBranchMoved) || len(fx.pushArgv()) != 0 {
			t.Fatalf("retry on a moved branch: %v, pushes %q", err, fx.pushArgv())
		}
	})
}

// TestFixRoundPushRefusesAChangeThatCannotTakeIt reads the change request
// before the commit: a merged or closed one, one whose head moved, or one
// whose branch is gone is never pushed to, the workspace stays on the
// delivered head, and no branch is recreated.
func TestFixRoundPushRefusesAChangeThatCannotTakeIt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fx *fixRoundFixture, head, headTree string)
		want   string
	}{
		{"merged", func(fx *fixRoundFixture, _, _ string) {
			fx.fake.change = func(pushed string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "merged", HeadCommit: pushed, URL: pluginTestURL}, nil
			}
			fx.view = func(pushed string) string { return fx.prJSON(pushed, "MERGED") }
		}, "is MERGED"},
		{"closed", func(fx *fixRoundFixture, _, _ string) {
			fx.fake.change = func(pushed string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "closed", HeadCommit: pushed, URL: pluginTestURL}, nil
			}
			fx.view = func(pushed string) string { return fx.prJSON(pushed, "CLOSED") }
		}, "is CLOSED"},
		{"moved head", func(fx *fixRoundFixture, _, _ string) {
			fx.fake.change = func(string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: strings.Repeat("f", 40), URL: pluginTestURL}, nil
			}
			fx.view = func(string) string { return fx.prJSON(strings.Repeat("f", 40), "OPEN") }
		}, "head commit missing or moved"},
		{"another URL", func(fx *fixRoundFixture, _, _ string) {
			fx.fake.change = func(pushed string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: pushed, URL: pluginTestURL + "0"}, nil
			}
			fx.view = func(pushed string) string {
				return lifecycleJSON(Receipt{URL: fixRoundTestURL + "0", Commit: pushed, Preview: PreviewResult{BaseBranch: "main", Branch: "forgecell/fixture"}}, "OPEN")
			}
		}, "URL mismatch"},
		// A plugin change names no branch; the remote branch read covers it.
		{"another branch", func(fx *fixRoundFixture, _, _ string) {
			fx.view = func(pushed string) string {
				return lifecycleJSON(Receipt{URL: fixRoundTestURL, Commit: pushed, Preview: PreviewResult{BaseBranch: "main", Branch: "forgecell/other"}}, "OPEN")
			}
		}, "head branch missing or mismatched"},
		{"missing branch", func(fx *fixRoundFixture, _, _ string) {
			if out, err := exec.Command("git", "-C", fx.bare, "update-ref", "-d", "refs/heads/forgecell/fixture").CombinedOutput(); err != nil {
				fx.t.Fatalf("delete the branch: %v %s", err, out)
			}
		}, "never recreates it"},
		{"branch moved", func(fx *fixRoundFixture, head, headTree string) {
			other := fx.git("commit-tree", headTree, "-p", head, "-m", "someone else")
			if out, err := exec.Command("git", "-C", fx.workspace, "push", fx.bare, other+":refs/heads/forgecell/fixture").CombinedOutput(); err != nil {
				fx.t.Fatalf("move the branch: %v %s", err, out)
			}
		}, "the change request's branch moved; collect again"},
	}
	for _, tc := range cases {
		eachForge(t, func(t *testing.T, plugin bool) {
			t.Run(tc.name, func(t *testing.T) {
				if plugin && tc.name == "another branch" {
					t.Skip("a plugin change names no branch")
				}
				fx := newFixRoundFixture(t, plugin)
				head, headTree := ReceiptHead(fx.receipt())
				fx.prepare(1, fixRoundGates{})
				p, err := PreviewFixRound(context.Background(), fx.options("", false))
				if err != nil {
					t.Fatal(err)
				}
				tc.mutate(fx, head, headTree)
				remoteBefore, _ := exec.Command("git", "-C", fx.bare, "for-each-ref", "--format=%(refname) %(objectname)").Output()
				before := fx.receipt()
				_, err = PushFixRound(context.Background(), fx.options(p.Digest, true))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				if len(fx.pushArgv()) != 0 || fx.git("rev-parse", "HEAD") != head || !sameReceipt(fx.receipt(), before) {
					t.Fatalf("refused after pushes %q, a commit or a record", fx.pushArgv())
				}
				if remoteAfter, _ := exec.Command("git", "-C", fx.bare, "for-each-ref", "--format=%(refname) %(objectname)").Output(); string(remoteAfter) != string(remoteBefore) {
					t.Fatalf("the remote changed: %s -> %s", remoteBefore, remoteAfter)
				}
			})
		})
	}
}

// TestFixRoundPushChecksOriginBeforeTheCommit retargets origin: the refusal
// comes before any forge call or commit, so the workspace stays on the
// delivered head.
func TestFixRoundPushChecksOriginBeforeTheCommit(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		head := fx.git("rev-parse", "HEAD")
		fx.prepare(1, fixRoundGates{})
		p, err := PreviewFixRound(context.Background(), fx.options("", false))
		if err != nil {
			t.Fatal(err)
		}
		fx.git("remote", "set-url", "--push", "origin", "https://github.com/other/repo.git")
		fx.order = nil
		_, err = PushFixRound(context.Background(), fx.options(p.Digest, true))
		if err == nil || !strings.Contains(err.Error(), "differs from intake") {
			t.Fatalf("err = %v", err)
		}
		if len(fx.argv) != 0 || len(fx.order) != 0 || fx.git("rev-parse", "HEAD") != head {
			t.Fatalf("a retargeted origin reached %q / %q or left a commit", fx.argv, fx.order)
		}
	})
}

// TestFixRoundPushChecksThePluginHostBeforeTheCommit refuses another host
// before any commit.
func TestFixRoundPushChecksThePluginHostBeforeTheCommit(t *testing.T) {
	fx := newFixRoundFixture(t, true)
	head := fx.git("rev-parse", "HEAD")
	fx.prepare(1, fixRoundGates{})
	p, err := PreviewFixRound(context.Background(), fx.options("", false))
	if err != nil {
		t.Fatal(err)
	}
	fx.fake.describe = func() (contract.ForgeDescription, error) {
		return contract.ForgeDescription{Identity: "forge.example/acme/widgets", Host: "other.example", DisplayName: "Fake GitLab", ChangeNoun: "merge request", DefaultBranch: "main", CIKind: "gitlab-ci", WebURL: "https://other.example/acme/widgets"}, nil
	}
	_, err = PushFixRound(context.Background(), fx.options(p.Digest, true))
	if err == nil || !strings.Contains(err.Error(), "host differs") {
		t.Fatalf("err = %v", err)
	}
	if len(fx.argv) != 0 || fx.git("rev-parse", "HEAD") != head {
		t.Fatalf("another host reached %q or left a commit", fx.argv)
	}
}

func TestFixRoundPushHeadReReadMismatchRecordsNothing(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		head, _ := ReceiptHead(fx.receipt())
		fx.prepare(1, fixRoundGates{})
		// The change reads at the head before the push, and at another commit after.
		seen := func(pushed string) string {
			if pushed == head {
				return head
			}
			return strings.Repeat("f", 40)
		}
		if plugin {
			fx.fake.change = func(pushed string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: seen(pushed), URL: pluginTestURL}, nil
			}
		} else {
			fx.view = func(pushed string) string { return fx.prJSON(seen(pushed), "OPEN") }
		}
		before := fx.receipt()
		_, _, err := fx.approved(true)
		if err == nil || !strings.Contains(err.Error(), "head commit missing or moved") || !strings.Contains(err.Error(), "nothing was recorded") {
			t.Fatalf("err = %v", err)
		}
		if !sameReceipt(fx.receipt(), before) {
			t.Fatal("a mismatched head was recorded")
		}
		if len(fx.pushArgv()) != 1 {
			t.Fatalf("pushes = %q", fx.pushArgv())
		}

		// A change request closed during the push is not confirmed either. The
		// first push landed, so the branch and the change are now at its commit.
		fx.argv = nil
		state := func(pushed string) string {
			if fx.pushArgv() == nil {
				return "OPEN"
			}
			return "CLOSED"
		}
		if plugin {
			fx.fake.change = func(pushed string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: strings.ToLower(state(pushed)), HeadCommit: pushed, URL: pluginTestURL}, nil
			}
		} else {
			fx.view = func(pushed string) string { return fx.prJSON(pushed, state(pushed)) }
		}
		if _, _, err = fx.approved(true); err == nil || !strings.Contains(err.Error(), "CLOSED") || !sameReceipt(fx.receipt(), before) {
			t.Fatalf("a closed change request: %v", err)
		}
	})
}

func TestFixRoundPushRefusesAnotherForge(t *testing.T) {
	eachForge(t, func(t *testing.T, plugin bool) {
		fx := newFixRoundFixture(t, plugin)
		fx.prepare(1, fixRoundGates{})
		p, err := PreviewFixRound(context.Background(), fx.options("", false))
		if err != nil {
			t.Fatal(err)
		}
		o := fx.options(p.Digest, false)
		if plugin {
			o.Changes = nil
		} else {
			o.Changes, o.Plugin = fx.fake, PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: fx.calls}
		}
		if _, err := PushFixRound(context.Background(), o); err == nil || !strings.Contains(err.Error(), "the Formula") || len(fx.argv) != 0 {
			t.Fatalf("another forge: %v, calls %q", err, fx.argv)
		}
	})
}

func TestDefaultFixRoundTitle(t *testing.T) {
	for _, tc := range []struct {
		issue molecule.Issue
		want  string
	}{
		{molecule.Issue{Number: 346}, "fix: address reviewer findings, round 2 (#346)"},
		{molecule.Issue{Key: "ENG-12"}, "fix: address reviewer findings, round 2 (#ENG-12)"},
		{molecule.Issue{Key: "ENG-12\nforged"}, "fix: address reviewer findings, round 2"},
	} {
		if got := DefaultFixRoundTitle(2, tc.issue); got != tc.want {
			t.Fatalf("title(%+v) = %q, want %q", tc.issue, got, tc.want)
		}
	}
	fx := newFixRoundFixture(t, false)
	fx.prepare(1, fixRoundGates{})
	o := fx.options("", false)
	o.Title = "two\nlines"
	if _, err := PreviewFixRound(context.Background(), o); err == nil || !strings.Contains(err.Error(), "one trimmed line") {
		t.Fatalf("a multi-line title: %v", err)
	}
}
