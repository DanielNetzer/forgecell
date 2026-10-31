package readiness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// gitlabCommit writes files into the fixture checkout and commits them; a value
// starting with "->" commits a symbolic link to the rest of it instead.
func gitlabCommit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "gitlab fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", args, b, err)
		}
	}
}

const gitlabCIFixture = "stages: [test]\nunit:\n  stage: test\n  script: [go test ./...]\n"

func TestCollectForZeroOptionsIsByteIdenticalAndSkipsAStrayGitLabFile(t *testing.T) {
	dir := repoFixture(t)
	gitlabCommit(t, dir, map[string]string{".gitlab-ci.yml": gitlabCIFixture})
	ctx := context.Background()
	plain, err := Collect(ctx, dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	zero, err := CollectFor(ctx, dir, "HEAD", CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, zero) {
		t.Fatal("the zero CollectOptions changed the snapshot")
	}
	a, _ := plain.Digest()
	b, _ := zero.Digest()
	if a != b {
		t.Fatal("the zero CollectOptions changed the digest")
	}
	for _, e := range plain.Evidence {
		if e.Path == ".gitlab-ci.yml" {
			t.Fatal("a stray .gitlab-ci.yml was collected without the flag")
		}
	}
	ticket, err := CollectForTicket(ctx, dir, "HEAD", "Fix `lab/a.go`.")
	if err != nil {
		t.Fatal(err)
	}
	ticketZero, err := CollectForTicketFor(ctx, dir, "HEAD", "Fix `lab/a.go`.", CollectOptions{})
	if err != nil || !reflect.DeepEqual(ticket, ticketZero) {
		t.Fatalf("CollectForTicketFor with zero options differs: %v", err)
	}
}

func TestCollectForGitLabCIAppendsTheFileLastWithoutShiftingIDs(t *testing.T) {
	dir := repoFixture(t)
	gitlabCommit(t, dir, map[string]string{".gitlab-ci.yml": gitlabCIFixture})
	ctx := context.Background()
	plain, err := Collect(ctx, dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged.Evidence) != len(plain.Evidence)+1 {
		t.Fatalf("want one more evidence entry: %d vs %d", len(flagged.Evidence), len(plain.Evidence))
	}
	if !reflect.DeepEqual(flagged.Evidence[:len(plain.Evidence)], plain.Evidence) {
		t.Fatal("earlier evidence entries changed")
	}
	last := flagged.Evidence[len(flagged.Evidence)-1]
	if last.Path != ".gitlab-ci.yml" || last.Content != gitlabCIFixture || last.ID != "file-"+strconv.Itoa(len(flagged.Evidence)) {
		t.Fatalf("unexpected appended evidence: %+v", last.Evidence)
	}
	// Ticket-named files follow it, and a ticket naming it finds it collected.
	ticket, err := CollectForTicketFor(ctx, dir, "HEAD", "See `.gitlab-ci.yml` and `lab/a.go`.", CollectOptions{GitLabCI: true})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Evidence[len(plain.Evidence)].Path != ".gitlab-ci.yml" {
		t.Fatal("the GitLab file is not ahead of the ticket-named files")
	}
	found := false
	for _, o := range ticket.Collection[0].Outcomes {
		if o.Path == ".gitlab-ci.yml" {
			found = o.Reason == "already collected"
		}
	}
	if !found {
		t.Fatalf("a ticket-named .gitlab-ci.yml should be already collected: %+v", ticket.Collection)
	}
}

func TestCollectForGitLabCIWithoutTheFileAddsNothing(t *testing.T) {
	dir := repoFixture(t)
	ctx := context.Background()
	plain, err := Collect(ctx, dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
	if err != nil || !reflect.DeepEqual(plain, flagged) {
		t.Fatalf("an absent .gitlab-ci.yml changed the snapshot: %v", err)
	}
}

func TestCollectForGitLabCIReadBound(t *testing.T) {
	ctx := context.Background()
	line := "# padding line for the GitLab CI read bound\n"
	big := gitlabCIFixture + strings.Repeat(line, (200<<10)/len(line))
	dir := repoFixture(t)
	gitlabCommit(t, dir, map[string]string{".gitlab-ci.yml": big})
	s, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
	if err != nil {
		t.Fatalf("a %d-byte file should be accepted: %v", len(big), err)
	}
	if last := s.Evidence[len(s.Evidence)-1]; last.Path != ".gitlab-ci.yml" || last.Content != big {
		t.Fatal("the large file was not collected whole")
	}

	exact := strings.Repeat("a", gitlabCIMaxBytes-1) + "\n"
	dir = repoFixture(t)
	gitlabCommit(t, dir, map[string]string{".gitlab-ci.yml": exact})
	if _, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true}); err != nil {
		t.Fatalf("a file at the bound should be accepted: %v", err)
	}

	over := exact + "x"
	dir = repoFixture(t)
	gitlabCommit(t, dir, map[string]string{".gitlab-ci.yml": over})
	_, err = CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
	if err == nil || !strings.Contains(err.Error(), "more than the 262144 bytes") {
		t.Fatalf("an oversized file should be refused plainly: %v", err)
	}
	if _, err := Collect(ctx, dir, "HEAD"); err != nil {
		t.Fatalf("without the flag the oversized file is not read: %v", err)
	}
}

func TestCollectForGitLabCICountsTowardTheTotal(t *testing.T) {
	dir := repoFixture(t)
	ctx := context.Background()
	files := map[string]string{".gitlab-ci.yml": strings.Repeat("b", 200_000) + "\n"}
	// Thirteen 63000-byte READMEs of nested components bring the tree walk to
	// about 820 KB, under the 1 MB total alone but not with the GitLab file.
	for i := range 13 {
		files["c"+strconv.Itoa(i)+"/CONTRIBUTING.md"] = strings.Repeat("c", 63_000)
	}
	gitlabCommit(t, dir, files)
	if _, err := Collect(ctx, dir, "HEAD"); err != nil {
		t.Fatalf("premise: the tree walk alone fits: %v", err)
	}
	_, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
	if err == nil || !strings.Contains(err.Error(), "exceeds 1 MB") {
		t.Fatalf("the GitLab file must count toward the 1 MB total: %v", err)
	}
}

func TestCollectForGitLabCIRefusesUnsafeFiles(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"symlink": {map[string]string{"ci.yml": gitlabCIFixture, ".gitlab-ci.yml": "->ci.yml"}, "regular committed file"},
		"binary":  {map[string]string{".gitlab-ci.yml": "stages: [test]\n\x00\x01"}, "binary or invalid UTF-8"},
		"invalid": {map[string]string{".gitlab-ci.yml": "stages: [\xff]\n"}, "binary or invalid UTF-8"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := repoFixture(t)
			gitlabCommit(t, dir, c.files)
			_, err := CollectFor(ctx, dir, "HEAD", CollectOptions{GitLabCI: true})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestAppendRootEvidenceRefusesADeniedPath(t *testing.T) {
	dir := repoFixture(t)
	ctx := context.Background()
	s, err := Collect(ctx, dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.Evidence)
	err = appendRootEvidence(ctx, dir, &s, 0, ".env", gitlabCIMaxBytes)
	if err == nil || !strings.Contains(err.Error(), "refused as evidence") || strings.Contains(err.Error(), "do-not-collect") {
		t.Fatalf("a denied path should be refused without its content: %v", err)
	}
	if len(s.Evidence) != before {
		t.Fatal("a refused path was appended")
	}
}
