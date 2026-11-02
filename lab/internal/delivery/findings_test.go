package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/reviewer"
)

const (
	findingsTestID     = "mol-7"
	findingsTestCommit = "0123456789abcdef0123456789abcdef01234567"
	findingsTestOther  = "89abcdef0123456789abcdef0123456789abcdef"
	findingsTestRepo   = "acme/widgets"
	findingsTestPR     = "https://github.com/acme/widgets/pull/7"
)

// findingsReviewer is an in-process reviewer. It records every request.
type findingsReviewer struct {
	desc     reviewer.Description
	descErr  error
	result   reviewer.Result
	err      error
	requests []reviewer.Request
}

func (r *findingsReviewer) Describe(context.Context) (reviewer.Description, error) {
	return r.desc, r.descErr
}

func (r *findingsReviewer) Findings(_ context.Context, req reviewer.Request) (reviewer.Result, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}

func complete(findings ...reviewer.Finding) *findingsReviewer {
	return &findingsReviewer{desc: reviewer.Description{DisplayName: "Critic"}, result: reviewer.Result{Status: reviewer.StatusComplete, Findings: findings}}
}

func finding(severity, path, title string) reviewer.Finding {
	return reviewer.Finding{ID: "F-" + title, Severity: severity, Path: path, Line: 4, Title: title, Body: "Why: " + title}
}

// findingsChanges is a plugin forge that serves comments and checks and
// counts the reads.
type findingsChanges struct {
	forge.Changes
	commentReads, checkReads int
	commentsErr              error
	partialChecks            bool
}

func (c *findingsChanges) Comments(_ context.Context, ref, since string) (contract.ForgeComments, error) {
	c.commentReads++
	if c.commentsErr != nil {
		return contract.ForgeComments{}, c.commentsErr
	}
	path, line := "src/a.go", int64(9)
	return contract.ForgeComments{Comments: []contract.ForgeComment{{ID: "n1", Author: "bot", IsBot: true, Body: "Nit", Path: &path, Line: &line, CreatedAt: "2026-11-01T10:00:00Z"}}}, nil
}

func (c *findingsChanges) Checks(_ context.Context, ref, commit string) (contract.ForgeChecks, error) {
	c.checkReads++
	url := "https://forge.example/ci/1"
	return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "completed", Conclusion: "success", URL: &url}}, Complete: !c.partialChecks}, nil
}

// findingsLab writes a receipt into a fresh Lab and returns the Lab and the
// receipt's bytes.
func findingsLab(t *testing.T, r Receipt) (string, []byte) {
	t.Helper()
	lab := t.TempDir()
	if err := os.Mkdir(filepath.Join(lab, "deliveries"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", findingsTestID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return lab, raw
}

func githubReceipt() Receipt {
	return Receipt{Preview: PreviewResult{MoleculeID: findingsTestID, Repo: findingsTestRepo}, Title: "Fix", State: "published", Commit: findingsTestCommit, URL: findingsTestPR}
}

func pluginReceipt() Receipt {
	return Receipt{Preview: PreviewResult{MoleculeID: findingsTestID, Repo: findingsTestRepo}, Title: "Fix", State: "published", Commit: findingsTestCommit, URL: "https://forge.example/acme/widgets/-/merge_requests/7", Forge: "gitlab", Project: findingsTestRepo, ForgeHost: "forge.example", ChangeNoun: "merge request", Ref: "!7"}
}

// clock returns a Now that advances one minute per call.
func clock() func() time.Time {
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	return func() time.Time { at = at.Add(time.Minute); return at }
}

func collect(t *testing.T, lab string, o FindingsOptions) (FindingsRecord, error) {
	t.Helper()
	o.LabDir, o.MoleculeID = lab, findingsTestID
	if o.Repo == "" {
		o.Repo = findingsTestRepo
	}
	// A plugin forge defaults to the one pluginReceipt was published through.
	if o.Changes != nil && o.ForgePlugin == "" {
		o.ForgePlugin, o.ForgeProject = "gitlab", findingsTestRepo
	}
	return Collect(context.Background(), o)
}

func named(rs ...*findingsReviewer) []FindingsReviewer {
	out := make([]FindingsReviewer, len(rs))
	for i, r := range rs {
		out[i] = FindingsReviewer{Plugin: fmt.Sprint("rev", i), Reviewer: r}
	}
	return out
}

func TestCollectRecordsEachReviewerApart(t *testing.T) {
	lab, receipt := findingsLab(t, githubReceipt())
	good := complete(finding("high", "src/a.go", "Unchecked error"))
	pending := &findingsReviewer{desc: reviewer.Description{DisplayName: "Slow"}, result: reviewer.Result{Status: reviewer.StatusPending, Findings: []reviewer.Finding{finding("low", "", "Partial")}}}
	failing := &findingsReviewer{desc: reviewer.Description{DisplayName: "Down"}, err: &mcpadapter.Error{Plugin: "down", Tool: "reviewer_findings", Reason: mcp.AuthMissing, Diagnostics: "Bearer secret /home/me"}}
	invalid := &findingsReviewer{desc: reviewer.Description{DisplayName: "Odd"}, err: &mcpadapter.Error{Plugin: "odd", Tool: "reviewer_findings", Reason: mcp.SchemaInvalid}}
	record, err := collect(t, lab, FindingsOptions{Reviewers: named(failing, good, invalid, pending), Now: clock()})
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Rounds) != 1 {
		t.Fatalf("rounds = %d", len(record.Rounds))
	}
	round := record.Rounds[0]
	if round.Commit != findingsTestCommit || round.Ref != "7" || !round.Complete || round.CollectedAt != "2026-11-02T10:01:00Z" {
		t.Fatalf("round = %+v", round)
	}
	want := []struct{ status, err string }{
		{FindingsError, "plugin down reviewer_findings: auth-missing"},
		{FindingsComplete, ""},
		{FindingsError, "plugin odd reviewer_findings: schema-invalid"},
		{FindingsPending, ""},
	}
	for i, w := range want {
		got := round.Reviewers[i]
		if got.Plugin != fmt.Sprint("rev", i) || got.Reviewer != ReviewerKey(i, got.Plugin) || got.Status != w.status || got.Error != w.err {
			t.Fatalf("reviewer %d = %+v, want %+v", i, got, w)
		}
	}
	if f := round.Reviewers[1].Findings; len(f) != 1 || f[0].Title != "Unchecked error" || f[0].PluginID != "F-Unchecked error" || f[0].Path != "src/a.go" || f[0].Line != 4 || f[0].Fingerprint != FindingFingerprint("1:rev1", "high", "src/a.go", "Unchecked error") {
		t.Fatalf("findings = %+v", f)
	}
	if len(round.Reviewers[3].Findings) != 1 {
		t.Fatalf("a pending reviewer's findings were not kept: %+v", round.Reviewers[3])
	}
	// Every reviewer was asked for exactly the delivered commit and the pull
	// request, with no comments or checks it did not describe needing.
	for _, r := range []*findingsReviewer{good, pending, failing, invalid} {
		if len(r.requests) != 1 || r.requests[0].Commit != findingsTestCommit || r.requests[0].Ref != "7" || r.requests[0].Comments != nil || r.requests[0].Checks != nil || r.requests[0].Forge["kind"] != "github" {
			t.Fatalf("requests = %+v", r.requests)
		}
	}
	raw, err := os.ReadFile(filepath.Join(lab, "deliveries", findingsTestID+".findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), lab) {
		t.Fatalf("the record leaks diagnostics or a local path:\n%s", raw)
	}
	read, err := ReadFindings(lab, findingsTestID)
	if err != nil || len(read.Rounds) != 1 || read.Rounds[0].Reviewers[1].Findings[0].Fingerprint != round.Reviewers[1].Findings[0].Fingerprint {
		t.Fatalf("ReadFindings = %+v, %v", read, err)
	}
	after, _ := os.ReadFile(filepath.Join(lab, "deliveries", findingsTestID+".json"))
	if !bytes.Equal(after, receipt) {
		t.Fatal("the receipt changed")
	}
	if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatalf("the delivery lock was left: %v", err)
	}

	// A describe failure is that reviewer's alone too.
	broken := &findingsReviewer{descErr: errors.New("boom /home/me")}
	record, err = collect(t, lab, FindingsOptions{Reviewers: named(broken, good)})
	if err != nil {
		t.Fatal(err)
	}
	last := record.Rounds[1]
	if last.Reviewers[0].Status != FindingsError || last.Reviewers[0].Error != "the reviewer failed: failed" || len(broken.requests) != 0 || last.Reviewers[1].Status != FindingsComplete {
		t.Fatalf("round = %+v", last)
	}
}

func TestFindingFingerprintIsStable(t *testing.T) {
	a, _ := recordFinding("critic", reviewer.Finding{ID: "A-1", Severity: "high", Path: "src/a.go", Title: "Unchecked  Error"})
	b, _ := recordFinding("critic", reviewer.Finding{ID: "B-9", Severity: "high", Path: "src/a.go", Title: "\tunchecked error \n"})
	if a.Fingerprint != b.Fingerprint || a.PluginID == b.PluginID {
		t.Fatalf("the fingerprint depends on the plugin id or title spacing: %s %s", a.Fingerprint, b.Fingerprint)
	}
	for _, other := range []string{
		FindingFingerprint("lint", "high", "src/a.go", "unchecked error"),
		FindingFingerprint("critic", "low", "src/a.go", "unchecked error"),
		FindingFingerprint("critic", "high", "src/b.go", "unchecked error"),
		FindingFingerprint("critic", "high", "src/a.go", "unchecked errors"),
	} {
		if other == a.Fingerprint {
			t.Fatal("the fingerprint ignores the reviewer, severity, path or title")
		}
	}
	if !fingerprintHex.MatchString(a.Fingerprint) {
		t.Fatalf("fingerprint = %q", a.Fingerprint)
	}
	// A pipe in a path or title cannot shift the fields into a collision.
	if FindingFingerprint("0:critic", "high", "a|high", "b") == FindingFingerprint("0:critic", "high", "a", "high|b") {
		t.Fatal("a path or title holding a pipe collides")
	}
	if FindingFingerprint("0:critic", "high", "src/a|b.go", "x") == FindingFingerprint("0:critic", "high", "src/a", "b.go|x") {
		t.Fatal("a path holding a pipe collides")
	}
	// The same plugin at another Formula index is another reviewer.
	if FindingFingerprint("0:critic", "high", "a", "b") == FindingFingerprint("1:critic", "high", "a", "b") {
		t.Fatal("the fingerprint ignores the Formula index")
	}
}

// roundOf builds a recorded round by hand.
func roundOf(commit string, complete bool, entries ...ReviewerRound) FindingsRound {
	return FindingsRound{Commit: commit, Ref: "7", CollectedAt: "2026-11-02T10:00:00Z", Complete: complete, Reviewers: entries}
}

func entryOf(index int, plugin, status string, findings ...reviewer.Finding) ReviewerRound {
	key := ReviewerKey(index, plugin)
	e := ReviewerRound{Reviewer: key, Plugin: plugin, Status: status, Findings: []Finding{}}
	for _, f := range findings {
		recorded, _ := recordFinding(key, f)
		e.Findings = append(e.Findings, recorded)
	}
	return e
}

func TestSummarizeInfersResolution(t *testing.T) {
	gone := finding("high", "src/a.go", "Gone later")
	kept := finding("low", "src/b.go", "Still there")
	info := finding("info", "", "Style")
	record := FindingsRecord{SchemaVersion: "v1", MoleculeID: findingsTestID, Rounds: []FindingsRound{
		roundOf(findingsTestCommit, true, entryOf(0, "critic", FindingsComplete, gone, kept), entryOf(1, "lint", FindingsComplete, info)),
		// Same title, new spacing and plugin id: the same finding.
		roundOf(findingsTestOther, true, entryOf(0, "critic", FindingsComplete, reviewer.Finding{ID: "X", Severity: "low", Path: "src/b.go", Title: "still  THERE"}), entryOf(1, "lint", FindingsPending)),
	}}
	s := Summarize(record, map[string]string{"0:critic": "low", "1:lint": "none"})
	if s.Round != 1 || s.Commit != findingsTestOther || s.NewerIncomplete {
		t.Fatalf("summary = %+v", s)
	}
	if s.OpenCount() != 1 || s.Open[0].Severity != "low" || s.Open[0].Findings[0].Title != "still  THERE" || !s.Open[0].Findings[0].AutoFix {
		t.Fatalf("open = %+v", s.Open)
	}
	// The critic's finding is resolved; the lint one is not, because lint is
	// still pending on the newer commit.
	if len(s.Resolved) != 1 || s.Resolved[0].Title != "Gone later" || !s.Resolved[0].Resolved || s.Resolved[0].Plugin != "critic" || s.Resolved[0].Reviewer != "0:critic" {
		t.Fatalf("resolved = %+v", s.Resolved)
	}
	if len(s.Pending) != 1 || s.Pending[0] != "1:lint" {
		t.Fatalf("pending = %v", s.Pending)
	}

	// A newer incomplete round never infers resolution and is not summarized.
	record.Rounds = append(record.Rounds, roundOf("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, entryOf(0, "critic", FindingsComplete)))
	s = Summarize(record, nil)
	if s.Round != 1 || !s.NewerIncomplete || s.OpenCount() != 1 || len(s.Resolved) != 1 || s.Open[0].Findings[0].AutoFix {
		t.Fatalf("summary with an incomplete newer round = %+v", s)
	}

	// A later round on the same commit infers nothing.
	same := FindingsRecord{Rounds: []FindingsRound{
		roundOf(findingsTestCommit, true, entryOf(0, "critic", FindingsComplete, gone)),
		roundOf(findingsTestCommit, true, entryOf(0, "critic", FindingsComplete)),
	}}
	if s := Summarize(same, nil); len(s.Resolved) != 0 || s.OpenCount() != 0 {
		t.Fatalf("same-commit summary = %+v", s)
	}
	// AutoFix marks findings at or above the threshold only.
	marked := Summarize(FindingsRecord{Rounds: []FindingsRound{roundOf(findingsTestCommit, true, entryOf(0, "critic", FindingsComplete, gone, kept, info))}}, map[string]string{"0:critic": "medium"})
	for _, g := range marked.Open {
		for _, f := range g.Findings {
			if f.AutoFix != (f.Severity == "high") {
				t.Fatalf("autoFix mark on %s = %t", f.Severity, f.AutoFix)
			}
		}
	}
	if len(marked.Open) != 3 || marked.Open[0].Severity != "high" || marked.Open[2].Severity != "info" {
		t.Fatalf("groups = %+v", marked.Open)
	}
	if s := Summarize(FindingsRecord{}, nil); s.Round != -1 || s.OpenCount() != 0 {
		t.Fatalf("empty summary = %+v", s)
	}
}

// The same plugin declared twice with different config and autoFix is two
// reviewers: findings, thresholds and resolution are never shared.
func TestFindingsSamePluginTwice(t *testing.T) {
	lab, _ := findingsLab(t, githubReceipt())
	x := finding("low", "src/a.go", "Same text")
	strict, lax := complete(x), complete(x)
	record, err := collect(t, lab, FindingsOptions{Reviewers: []FindingsReviewer{{Plugin: "critic", Reviewer: strict}, {Plugin: "critic", Reviewer: lax}}})
	if err != nil {
		t.Fatal(err)
	}
	round := record.Rounds[0]
	a, b := round.Reviewers[0], round.Reviewers[1]
	if a.Reviewer != "0:critic" || b.Reviewer != "1:critic" || a.Plugin != "critic" || b.Plugin != "critic" {
		t.Fatalf("reviewers = %q %q", a.Reviewer, b.Reviewer)
	}
	if a.Findings[0].Fingerprint == b.Findings[0].Fingerprint {
		t.Fatal("two declarations of one plugin share a fingerprint")
	}
	if _, err := ReadFindings(lab, findingsTestID); err != nil {
		t.Fatalf("the record does not read back: %v", err)
	}
	autoFix := map[string]string{"0:critic": "high", "1:critic": "low"}
	s := Summarize(record, autoFix)
	if s.OpenCount() != 2 {
		t.Fatalf("open = %+v", s.Open)
	}
	for _, f := range s.Open[0].Findings {
		if f.AutoFix != (f.Reviewer == "1:critic") {
			t.Fatalf("autoFix of %s = %t", f.Reviewer, f.AutoFix)
		}
	}

	// On a newer commit only the first declaration stopped reporting it: only
	// its finding is inferred resolved.
	later := FindingsRecord{Rounds: []FindingsRound{
		roundOf(findingsTestCommit, true, entryOf(0, "critic", FindingsComplete, x), entryOf(1, "critic", FindingsComplete, x)),
		roundOf(findingsTestOther, true, entryOf(0, "critic", FindingsComplete), entryOf(1, "critic", FindingsComplete, x)),
	}}
	s = Summarize(later, autoFix)
	if len(s.Resolved) != 1 || s.Resolved[0].Reviewer != "0:critic" || s.OpenCount() != 1 || s.Open[0].Findings[0].Reviewer != "1:critic" || !s.Open[0].Findings[0].AutoFix {
		t.Fatalf("summary = %+v", s)
	}
	// And a pending second declaration never lets the first infer for it.
	later.Rounds[1].Reviewers[1] = entryOf(1, "critic", FindingsPending)
	s = Summarize(later, autoFix)
	if len(s.Resolved) != 1 || s.Resolved[0].Reviewer != "0:critic" || len(s.Pending) != 1 || s.Pending[0] != "1:critic" {
		t.Fatalf("summary with a pending declaration = %+v", s)
	}

	// A record whose reviewer identity does not match its position is refused.
	file := filepath.Join(lab, "deliveries", findingsTestID+".findings.json")
	raw, _ := os.ReadFile(file)
	tampered := strings.Replace(string(raw), `"reviewer":"1:critic"`, `"reviewer":"0:critic"`, 1)
	if tampered == string(raw) {
		t.Fatal("the edit changed nothing")
	}
	if err := os.WriteFile(file, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFindings(lab, findingsTestID); err == nil {
		t.Fatal("a duplicated reviewer identity was accepted")
	}
}

func many(n int, severity string, body string) []reviewer.Finding {
	out := make([]reviewer.Finding, n)
	for i := range out {
		out[i] = reviewer.Finding{ID: fmt.Sprint("F-", i), Severity: severity, Path: "src/a.go", Line: int64(i + 1), Title: fmt.Sprint(severity, " finding ", i), Body: body}
	}
	return out
}

func recordSize(t *testing.T, lab string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(lab, "deliveries", findingsTestID+".findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestCollectBounds(t *testing.T) {
	body := strings.Repeat("b", MaxFindingBodyBytes)

	t.Run("501 findings", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(complete(many(501, "low", "")...))})
		if err != nil {
			t.Fatal(err)
		}
		e := record.Rounds[0].Reviewers[0]
		if len(e.Findings) != MaxFindingsPerReviewer || !e.Truncated || e.Dropped != 1 || record.Rounds[0].Complete {
			t.Fatalf("entry: %d findings, truncated %t, dropped %d, complete %t", len(e.Findings), e.Truncated, e.Dropped, record.Rounds[0].Complete)
		}
	})

	t.Run("21 rounds", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		now := clock()
		var record FindingsRecord
		for range MaxFindingsRounds + 1 {
			var err error
			if record, err = collect(t, lab, FindingsOptions{Reviewers: named(complete()), Now: now}); err != nil {
				t.Fatal(err)
			}
		}
		if len(record.Rounds) != MaxFindingsRounds || record.RoundsDropped != 1 || record.Rounds[0].CollectedAt != "2026-11-02T10:02:00Z" {
			t.Fatalf("%d rounds, %d dropped, oldest %s", len(record.Rounds), record.RoundsDropped, record.Rounds[0].CollectedAt)
		}
	})

	t.Run("4 MiB record", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		big := complete(many(60, "medium", body)...) // about 1 MiB a round
		var record FindingsRecord
		for range 5 {
			var err error
			if record, err = collect(t, lab, FindingsOptions{Reviewers: named(big)}); err != nil {
				t.Fatal(err)
			}
		}
		if record.RoundsDropped == 0 || len(record.Rounds) >= 5 || recordSize(t, lab) > MaxFindingsRecordBytes {
			t.Fatalf("%d rounds, %d dropped, %d bytes", len(record.Rounds), record.RoundsDropped, recordSize(t, lab))
		}
		for _, r := range record.Rounds {
			if !r.Complete || len(r.Reviewers[0].Findings) != 60 {
				t.Fatal("a kept round lost findings")
			}
		}
	})

	t.Run("one round over 4 MiB", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		var list []reviewer.Finding
		for _, severity := range []string{"low", "high", "info", "medium"} {
			list = append(list, many(100, severity, body)...)
		}
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(complete(list...), complete(finding("info", "", "Tiny")))})
		if err != nil {
			t.Fatal(err)
		}
		round := record.Rounds[0]
		counts := map[string]int{}
		lows := []int64{}
		for _, f := range round.Reviewers[0].Findings {
			counts[f.Severity]++
			if f.Severity == "low" {
				lows = append(lows, f.Line)
			}
		}
		if round.Complete || counts["high"] != 100 || counts["medium"] != 100 || counts["info"] != 0 || counts["low"] == 0 || counts["low"] == 100 {
			t.Fatalf("complete %t, kept %v", round.Complete, counts)
		}
		// Within a severity the latest go first, so the earliest lows remain.
		for i, line := range lows {
			if line != int64(i+1) {
				t.Fatalf("kept lows %v", lows)
			}
		}
		e := round.Reviewers[0]
		if !e.Truncated || e.Dropped != 400-len(e.Findings) {
			t.Fatalf("truncated %t, dropped %d", e.Truncated, e.Dropped)
		}
		// The other reviewer's info finding is the latest info, so it goes first.
		if other := round.Reviewers[1]; !other.Truncated || other.Dropped != 1 || len(other.Findings) != 0 {
			t.Fatalf("other = %+v", other)
		}
		if recordSize(t, lab) > MaxFindingsRecordBytes {
			t.Fatalf("record is %d bytes", recordSize(t, lab))
		}
		// An incomplete round is not summarized.
		if s := Summarize(record, nil); s.Round != -1 || !s.NewerIncomplete {
			t.Fatalf("summary = %+v", s)
		}
	})

	t.Run("64 KiB body", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		long := strings.Repeat("€", (64<<10)/3)
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(complete(reviewer.Finding{ID: "F", Severity: "low", Title: "Long", Body: long}))})
		if err != nil {
			t.Fatal(err)
		}
		f := record.Rounds[0].Reviewers[0].Findings[0]
		if !f.BodyTruncated || len(f.Body) > MaxFindingBodyBytes || len(f.Body) < MaxFindingBodyBytes-3 || !utf8.ValidString(f.Body) || !strings.HasPrefix(long, f.Body) {
			t.Fatalf("body is %d bytes, truncated %t", len(f.Body), f.BodyTruncated)
		}
		if !record.Rounds[0].Complete {
			t.Fatal("a cut body made the round incomplete")
		}
	})
}

func TestGitHubPullRef(t *testing.T) {
	ref, n, err := GitHubPullRef(findingsTestPR, "Acme/Widgets")
	if err != nil || ref != "7" || n != 7 {
		t.Fatalf("GitHubPullRef = %q, %d, %v", ref, n, err)
	}
	if ref, n, err := GitHubPullRef("https://github.com/ACME/widgets/pull/9999999999", findingsTestRepo); err != nil || ref != "9999999999" || n != 9999999999 {
		t.Fatalf("a 10-digit pull request = %q, %d, %v", ref, n, err)
	}
	for _, url := range []string{
		"https://github.com/other/widgets/pull/7",
		"https://github.com/acme/widgets-x/pull/7",
		"http://github.com/acme/widgets/pull/7",
		"https://GitHub.com/acme/widgets/pull/7",
		"https://github.com.evil/acme/widgets/pull/7",
		"https://github.com/acme/widgets/pull/07",
		"https://github.com/acme/widgets/pull/0",
		"https://github.com/acme/widgets/pull/-7",
		"https://github.com/acme/widgets/pull/12345678901",
		"https://github.com/acme/widgets/pull/7/",
		"https://github.com/acme/widgets/pull/7/files",
		"https://github.com/acme/widgets/pull/7?x=1",
		"https://github.com/acme/widgets/pull/7#top",
		"https://github.com/acme/widgets/pulls/7",
		"https://github.com/acme/widgets/issues/7",
		"https://github.com/x/acme/widgets/pull/7",
		"",
	} {
		if _, _, err := GitHubPullRef(url, findingsTestRepo); !errors.Is(err, ErrNoChangeRequest) {
			t.Errorf("%q was accepted: %v", url, err)
		}
	}
	if _, _, err := GitHubPullRef(findingsTestPR, ""); !errors.Is(err, ErrNoChangeRequest) {
		t.Fatal("an empty repository was accepted")
	}
	// Only ASCII case is ignored: the Kelvin sign folds to k under Unicode
	// rules, but never matches here, and a non-ASCII repository never matches.
	if !strings.EqualFold("Kcme", "kcme") {
		t.Fatal("the Kelvin sign no longer folds to k; the test proves nothing")
	}
	for _, c := range []struct{ url, repo string }{
		{"https://github.com/Kcme/widgets/pull/7", "kcme/widgets"},
		{"https://github.com/kcme/widgets/pull/7", "Kcme/widgets"},
		{"https://github.com/Kcme/widgets/pull/7", "Kcme/widgets"},
		{"https://github.com/acme/wıdgets/pull/7", "acme/widgets"},
	} {
		if _, _, err := GitHubPullRef(c.url, c.repo); !errors.Is(err, ErrNoChangeRequest) {
			t.Errorf("%q for %q was accepted: %v", c.url, c.repo, err)
		}
	}
	if ref, _, err := GitHubPullRef("https://github.com/KCME/Widgets/pull/7", "kcme/widgets"); err != nil || ref != "7" {
		t.Fatalf("ASCII case was not ignored: %q, %v", ref, err)
	}

	// Collect refuses a foreign URL before any reviewer is called.
	r := githubReceipt()
	r.URL = "https://github.com/other/widgets/pull/7"
	lab, _ := findingsLab(t, r)
	rev := complete()
	if _, err := collect(t, lab, FindingsOptions{Reviewers: named(rev)}); err == nil || err.Error() != "the delivery receipt names no change request" || len(rev.requests) != 0 {
		t.Fatalf("a foreign URL: %v, %d requests", err, len(rev.requests))
	}
}

func TestCollectRefusals(t *testing.T) {
	refused := func(t *testing.T, lab string, o FindingsOptions, want string) {
		t.Helper()
		rev := complete()
		if o.Reviewers == nil {
			o.Reviewers = named(rev)
		}
		_, err := collect(t, lab, o)
		if err == nil || !strings.Contains(err.Error(), want) || len(rev.requests) != 0 {
			t.Fatalf("err = %v, want %q; %d requests", err, want, len(rev.requests))
		}
		if strings.Contains(err.Error(), lab) {
			t.Fatalf("the error names a local path: %v", err)
		}
		if _, statErr := os.Lstat(filepath.Join(lab, "deliveries", findingsTestID+".findings.json")); !os.IsNotExist(statErr) && want != "not a bounded regular file" && want != "not a regular file" {
			t.Fatalf("a refused collection wrote a record: %v", statErr)
		}
	}
	t.Run("unpublished", func(t *testing.T) {
		r := githubReceipt()
		r.State = "committed"
		lab, _ := findingsLab(t, r)
		refused(t, lab, FindingsOptions{}, "only for a published delivery")
	})
	t.Run("no commit", func(t *testing.T) {
		r := githubReceipt()
		r.Commit = ""
		lab, _ := findingsLab(t, r)
		refused(t, lab, FindingsOptions{}, "only for a published delivery")
	})
	t.Run("no receipt", func(t *testing.T) {
		lab := t.TempDir()
		refused(t, lab, FindingsOptions{}, "no delivery receipt")
	})
	t.Run("bad id", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		for _, id := range []string{"../mol-7", "mol 7", "", "mol-7/x"} {
			if _, err := Collect(context.Background(), FindingsOptions{LabDir: lab, MoleculeID: id, Reviewers: named(complete())}); err == nil || err.Error() != "invalid Molecule id" {
				t.Fatalf("%q: %v", id, err)
			}
			if _, err := ReadFindings(lab, id); err == nil {
				t.Fatalf("ReadFindings(%q) was accepted", id)
			}
		}
	})
	t.Run("no reviewers", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		if _, err := collect(t, lab, FindingsOptions{}); !errors.Is(err, ErrNoReviewers) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlinked record", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(lab, "deliveries", findingsTestID+".findings.json")); err != nil {
			t.Fatal(err)
		}
		refused(t, lab, FindingsOptions{}, "not a bounded regular file")
		if _, err := ReadFindings(lab, findingsTestID); err == nil {
			t.Fatal("ReadFindings followed a symlink")
		}
		if raw, _ := os.ReadFile(target); string(raw) != "{}" {
			t.Fatal("the symlink target was written")
		}
		// writeFindings refuses it as well.
		if err := writeFindings(lab, findingsTestID, FindingsRecord{SchemaVersion: "v1", MoleculeID: findingsTestID}); err == nil {
			t.Fatal("writeFindings replaced a symlink")
		}
	})
	t.Run("symlinked deliveries", func(t *testing.T) {
		lab := t.TempDir()
		real := t.TempDir()
		if err := os.Symlink(real, filepath.Join(lab, "deliveries")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFindings(lab, findingsTestID); err == nil {
			t.Fatal("ReadFindings followed a symlinked deliveries directory")
		}
	})
	t.Run("forge mismatch", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		refused(t, lab, FindingsOptions{Changes: &findingsChanges{}}, "published through GitHub")
		refused(t, lab, FindingsOptions{ForgePlugin: "gitlab", ForgeProject: findingsTestRepo}, "published through GitHub")
		lab, _ = findingsLab(t, pluginReceipt())
		refused(t, lab, FindingsOptions{}, "published through a forge plugin")
	})
	// The Formula's forge plugin or project changed after publishing: the Lab's
	// forge is not the one the change request lives on.
	t.Run("forge changed after publishing", func(t *testing.T) {
		lab, _ := findingsLab(t, pluginReceipt())
		for _, o := range []FindingsOptions{
			{Changes: &findingsChanges{}, ForgePlugin: "forgejo", ForgeProject: findingsTestRepo},
			{Changes: &findingsChanges{}, ForgePlugin: "gitlab", ForgeProject: "acme/other"},
			{Changes: &findingsChanges{}, ForgePlugin: "gitlab", ForgeProject: "ACME/widgets"},
		} {
			changes := o.Changes.(*findingsChanges)
			refused(t, lab, o, "another forge plugin or project")
			if changes.commentReads+changes.checkReads != 0 {
				t.Fatal("the forge was read")
			}
		}
	})
	t.Run("plugin receipt without a ref", func(t *testing.T) {
		r := pluginReceipt()
		r.Ref = ""
		lab, _ := findingsLab(t, r)
		refused(t, lab, FindingsOptions{Changes: &findingsChanges{}}, "names no change request")
	})
	t.Run("lock held", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		if err := os.Mkdir(filepath.Join(lab, ".delivery-lock"), 0o700); err != nil {
			t.Fatal(err)
		}
		refused(t, lab, FindingsOptions{}, "conflict")
		if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); err != nil {
			t.Fatal("another operation's lock was removed")
		}
	})
	t.Run("traversing finding path", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(complete(finding("high", "../../etc/passwd", "Escape"), finding("high", "/etc/passwd", "Absolute"), finding("high", `src\a.go`, "Backslash")))})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range record.Rounds[0].Reviewers[0].Findings {
			if f.Path != "" || f.Note != mcpadapter.PathClearedNote || f.Title == "" {
				t.Fatalf("finding = %+v", f)
			}
		}
	})
}

func TestCollectReadsForgeInputsOnlyWhenNeeded(t *testing.T) {
	both := reviewer.Description{DisplayName: "Reader", Capabilities: []string{reviewer.CapabilityComments, reviewer.CapabilityChecks}}

	t.Run("plugin forge", func(t *testing.T) {
		lab, _ := findingsLab(t, pluginReceipt())
		plain := complete()
		reader := complete()
		reader.desc = both
		second := complete()
		second.desc = both
		changes := &findingsChanges{}
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(plain, reader, second), Changes: changes})
		if err != nil {
			t.Fatal(err)
		}
		if changes.commentReads != 1 || changes.checkReads != 1 {
			t.Fatalf("comments read %d times, checks %d", changes.commentReads, changes.checkReads)
		}
		if req := plain.requests[0]; req.Comments != nil || req.Checks != nil || req.Ref != "!7" || req.Forge["project"] != findingsTestRepo {
			t.Fatalf("the plain reviewer got %+v", req)
		}
		req := reader.requests[0]
		if len(req.Comments) != 1 || req.Comments[0].Path != "src/a.go" || req.Comments[0].Line != 9 || len(req.Checks) != 1 || req.Checks[0] != (reviewer.Check{Name: "build", Status: "completed", Conclusion: "success", URL: "https://forge.example/ci/1"}) {
			t.Fatalf("the reader got %+v", req)
		}
		if !record.Rounds[0].Complete {
			t.Fatal("round incomplete")
		}

		// A failed comments read fails only the reviewers that need them.
		changes = &findingsChanges{commentsErr: mcp.Fail(mcp.RateLimited, "forge_comments", "slow down")}
		reader.requests, plain.requests = nil, nil
		record, err = collect(t, lab, FindingsOptions{Reviewers: named(reader, plain), Changes: changes})
		if err != nil {
			t.Fatal(err)
		}
		if e := record.Rounds[1].Reviewers[0]; e.Status != FindingsError || e.Error != "the change request's comments could not be read (rate-limited)" || len(reader.requests) != 0 {
			t.Fatalf("reader = %+v", e)
		}
		if e := record.Rounds[1].Reviewers[1]; e.Status != FindingsComplete {
			t.Fatalf("plain = %+v", e)
		}
	})

	// The forge says its checks list is partial: the reviewer's input is
	// incomplete, so the round is too, and it never infers resolution.
	t.Run("partial checks", func(t *testing.T) {
		lab, _ := findingsLab(t, pluginReceipt())
		x := finding("high", "src/a.go", "Was there")
		record := FindingsRecord{SchemaVersion: "v1", MoleculeID: findingsTestID, Rounds: []FindingsRound{roundOf(findingsTestOther, true, entryOf(0, "rev0", FindingsComplete, x), entryOf(1, "rev1", FindingsComplete))}}
		if err := writeFindings(lab, findingsTestID, record); err != nil {
			t.Fatal(err)
		}
		checker := complete()
		checker.desc = reviewer.Description{DisplayName: "CI", Capabilities: []string{reviewer.CapabilityChecks}}
		plain := complete()
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(checker, plain), Changes: &findingsChanges{partialChecks: true}})
		if err != nil {
			t.Fatal(err)
		}
		round := record.Rounds[1]
		if round.Complete || !round.Reviewers[0].InputIncomplete || round.Reviewers[0].Status != FindingsComplete || round.Reviewers[1].InputIncomplete {
			t.Fatalf("round = %+v", round)
		}
		if len(checker.requests) != 1 || len(checker.requests[0].Checks) != 1 {
			t.Fatalf("requests = %+v", checker.requests)
		}
		s := Summarize(record, nil)
		if s.Round != 0 || !s.NewerIncomplete || len(s.Resolved) != 0 || s.OpenCount() != 1 {
			t.Fatalf("summary = %+v", s)
		}
	})

	t.Run("github", func(t *testing.T) {
		lab, _ := findingsLab(t, githubReceipt())
		head := findingsTestCommit
		var argv []string
		run := func(_ context.Context, o process.Options) process.Result {
			argv = o.Argv
			out := fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/acme/widgets/runs/1"},{"__typename":"StatusContext","context":"ci/legacy","state":"PENDING","targetUrl":""}]}`, head)
			return process.Result{OK: true, Stdout: out}
		}
		commentsOnly := complete()
		commentsOnly.desc = reviewer.Description{DisplayName: "Talker", Capabilities: []string{reviewer.CapabilityComments}}
		checksOnly := complete()
		checksOnly.desc = reviewer.Description{DisplayName: "CI", Capabilities: []string{reviewer.CapabilityChecks}}
		record, err := collect(t, lab, FindingsOptions{Reviewers: named(commentsOnly, checksOnly), GitHub: forge.GitHub{Run: run}})
		if err != nil {
			t.Fatal(err)
		}
		if e := record.Rounds[0].Reviewers[0]; e.Status != FindingsError || e.Error != "this forge does not serve review comments" || len(commentsOnly.requests) != 0 {
			t.Fatalf("comments reviewer = %+v", e)
		}
		if e := record.Rounds[0].Reviewers[1]; e.Status != FindingsComplete {
			t.Fatalf("checks reviewer = %+v", e)
		}
		got := checksOnly.requests[0].Checks
		want := []reviewer.Check{{Name: "build", Status: "completed", Conclusion: "success", URL: "https://github.com/acme/widgets/runs/1"}, {Name: "ci/legacy", Status: "in_progress"}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("checks = %+v", got)
		}
		if strings.Join(argv, " ") != "gh pr view 7 --repo acme/widgets --json headRefOid,statusCheckRollup" {
			t.Fatalf("argv = %v", argv)
		}

		// A head that moved on serves another commit's checks: refused.
		head = findingsTestOther
		checksOnly.requests = nil
		record, err = collect(t, lab, FindingsOptions{Reviewers: named(checksOnly), GitHub: forge.GitHub{Run: run}})
		if err != nil {
			t.Fatal(err)
		}
		if e := record.Rounds[1].Reviewers[0]; e.Status != FindingsError || !strings.Contains(e.Error, "head moved past") || len(checksOnly.requests) != 0 {
			t.Fatalf("stale = %+v", e)
		}
	})
}

func TestReadFindingsRefusesTampering(t *testing.T) {
	lab, _ := findingsLab(t, githubReceipt())
	empty, err := ReadFindings(lab, findingsTestID)
	if err != nil || empty.MoleculeID != findingsTestID || len(empty.Rounds) != 0 {
		t.Fatalf("an absent record = %+v, %v", empty, err)
	}
	if _, err := collect(t, lab, FindingsOptions{Reviewers: named(complete(finding("low", "src/a.go", "One")))}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(lab, "deliveries", findingsTestID+".findings.json")
	good, _ := os.ReadFile(file)
	for name, edit := range map[string]func(string) string{
		"another Molecule":  func(s string) string { return strings.Replace(s, `"moleculeId":"mol-7"`, `"moleculeId":"mol-8"`, 1) },
		"unknown field":     func(s string) string { return strings.Replace(s, `"rounds":`, `"approved":true,"rounds":`, 1) },
		"bad fingerprint":   func(s string) string { return strings.Replace(s, `"fingerprint":"`, `"fingerprint":"zz`, 1) },
		"traversing path":   func(s string) string { return strings.Replace(s, `"path":"src/a.go"`, `"path":"../a.go"`, 1) },
		"unknown status":    func(s string) string { return strings.Replace(s, `"status":"complete"`, `"status":"approved"`, 1) },
		"trailing data":     func(s string) string { return s + "{}" },
		"oversized":         func(s string) string { return s + strings.Repeat(" ", MaxFindingsRecordBytes) },
		"unknown severity":  func(s string) string { return strings.Replace(s, `"severity":"low"`, `"severity":"critical"`, 1) },
		"bad schemaVersion": func(s string) string { return strings.Replace(s, `"schemaVersion":"v1"`, `"schemaVersion":"v2"`, 1) },
	} {
		tampered := edit(string(good))
		if tampered == string(good) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if err := os.WriteFile(file, []byte(tampered), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFindings(lab, findingsTestID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// 21 rounds are refused on read.
	var record FindingsRecord
	if err := json.Unmarshal(good, &record); err != nil {
		t.Fatal(err)
	}
	for len(record.Rounds) <= MaxFindingsRounds {
		record.Rounds = append(record.Rounds, record.Rounds[0])
	}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFindings(lab, findingsTestID); err == nil {
		t.Fatal("21 rounds were accepted")
	}
}
