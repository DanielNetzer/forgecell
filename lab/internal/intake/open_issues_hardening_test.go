package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// milestoneGH installs a fake gh that serves the milestone listing, serves Issue
// pages only for the milestone number it is told about, and logs every call.
func milestoneGH(t *testing.T, milestones string, number int, pages map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, raw := range pages {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("page-%d.json", n)), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "milestones.json"), []byte(milestones), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	queueGH(t, "echo \"$*\" >> '"+log+"'\n"+
		"for last; do :; done\n"+
		"case \"$last\" in\n"+
		"*/milestones?*) cat '"+dir+"/milestones.json'; exit 0;;\n"+
		"*'&milestone="+fmt.Sprint(number)+"&page='*) ;;\n"+
		"*) exit 9;;\n"+
		"esac\n"+
		"page=${last##*page=}\n"+
		"cat '"+dir+"'/page-$page.json 2>/dev/null || printf '[]'\n")
	return log
}

func TestHardeningLabelSelectionIsPartOfTheQuery(t *testing.T) {
	log := loopGH(t, map[int]string{1: loopPage(1, 3)})
	got, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectLabel, Value: "good first/issue"})
	if err != nil || len(got.Issues) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	calls := loopCalls(t, log)
	want := "api --hostname github.com --method GET repos/o/r/issues?state=open&sort=created&direction=asc&per_page=30&labels=good+first%2Fissue&page=1"
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("calls: %v", calls)
	}

	oldest := loopGH(t, map[int]string{1: loopPage(1, 1)})
	if _, err = ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectOldest}); err != nil {
		t.Fatal(err)
	}
	if calls = loopCalls(t, oldest); len(calls) != 1 || calls[0] != "api --hostname github.com --method GET repos/o/r/issues?state=open&sort=created&direction=asc&per_page=30&page=1" {
		t.Fatalf("the oldest query changed: %v", calls)
	}
}

func TestHardeningLabelWithACommaIsRefusedBeforeAnyRead(t *testing.T) {
	log := loopGH(t, map[int]string{1: loopPage(1, 1)})
	if _, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectLabel, Value: "a,b"}); err == nil {
		t.Fatal("a label that GitHub's filter would split was accepted")
	}
	if calls := loopCalls(t, log); len(calls) != 0 {
		t.Fatalf("gh was called: %v", calls)
	}
}

func TestHardeningMilestoneTitleIsResolvedToItsNumber(t *testing.T) {
	milestones := `[{"number":3,"title":"v1"},{"number":4,"title":"V1"},{"number":5,"title":"Sprint 4"}]`
	log := milestoneGH(t, milestones, 4, map[int]string{1: loopPage(1, 2)})
	got, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectMilestone, Value: "V1"})
	if err != nil || len(got.Issues) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	calls := loopCalls(t, log)
	if len(calls) != 2 || !strings.Contains(calls[0], "repos/o/r/milestones?state=all&per_page=100&page=1") || !strings.HasSuffix(calls[1], "&milestone=4&page=1") {
		t.Fatalf("calls: %v", calls)
	}
}

func TestHardeningUnknownOrAmbiguousMilestoneIsAnErrorNamingIt(t *testing.T) {
	log := milestoneGH(t, `[{"number":3,"title":"v1"},{"number":9,"title":"v1"}]`, 3, map[int]string{1: loopPage(1, 1)})
	for _, title := range []string{"v2", "V1", "v1"} {
		_, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectMilestone, Value: title})
		if err == nil || !strings.Contains(err.Error(), `"`+title+`"`) {
			t.Fatalf("%q: %v", title, err)
		}
	}
	for _, call := range loopCalls(t, log) {
		if strings.Contains(call, "/issues?") {
			t.Fatalf("an Issue query was made for an unresolved milestone: %s", call)
		}
	}
}

func TestHardeningMilestoneListingRefusesMalformedData(t *testing.T) {
	for name, raw := range map[string]string{"null": "null", "no number": `[{"title":"v1"}]`, "no title": `[{"number":1,"title":""}]`} {
		t.Run(name, func(t *testing.T) {
			milestoneGH(t, raw, 1, nil)
			if _, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectMilestone, Value: "v1"}); err == nil {
				t.Fatal("malformed milestone data accepted")
			}
		})
	}
}

func TestHardeningTruncationUnderASelectorIsAnError(t *testing.T) {
	pages := map[int]string{}
	for p := 1; p <= 10; p++ {
		pages[p] = loopPage((p-1)*30+1, 30)
	}
	pages[11] = loopPage(301, 5)

	loopGH(t, pages)
	got, err := ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectLabel, Value: "bug"})
	if err == nil || !strings.Contains(err.Error(), "label:bug") {
		t.Fatalf("a truncated label selection was accepted: %v", err)
	}

	milestoneGH(t, `[{"number":2,"title":"v1"}]`, 2, pages)
	if _, err = ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectMilestone, Value: "v1"}); err == nil || !strings.Contains(err.Error(), "milestone:v1") {
		t.Fatalf("a truncated milestone selection was accepted: %v", err)
	}

	loopGH(t, pages)
	got, err = ReadSelectedOpenIssues(context.Background(), "o/r", Selector{Kind: SelectOldest})
	if err != nil || !got.Truncated || len(got.Issues) != 300 {
		t.Fatalf("the oldest selection keeps its truncation note: truncated=%t issues=%d %v", got.Truncated, len(got.Issues), err)
	}
}

func TestHardeningADuplicateWithinOnePageIsStillRefused(t *testing.T) {
	one := `{"number":1,"title":"t","state":"open","html_url":"https://github.com/o/r/issues/1","created_at":"2026-01-01T00:00:00Z"}`
	loopGH(t, map[int]string{1: "[" + one + "," + one + "]"})
	if got, err := ReadOpenIssues(context.Background(), "o/r"); err == nil {
		t.Fatalf("accepted: %+v", got)
	}
}

func TestHardeningDeduplicationKeepsTheFirstOccurrence(t *testing.T) {
	first := loopPage(1, 30)
	later := strings.Replace(loopPage(30, 2), `"title":"t30"`, `"title":"moved"`, 1)
	loopGH(t, map[int]string{1: first, 2: later})
	got, err := ReadOpenIssues(context.Background(), "o/r")
	if err != nil || len(got.Issues) != 31 {
		t.Fatalf("%d issues, %v", len(got.Issues), err)
	}
	if got.Issues[29].Number != 30 || got.Issues[29].Title != "t30" {
		t.Fatalf("the first occurrence was not kept: %+v", got.Issues[29])
	}
}
