package readiness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialPathsRefusedInBothCollectionPasses(t *testing.T) {
	paths := []string{"_netrc", "docs/_netrc", ".docker/config.json", "docs/.docker/config.json", "docs/nested/.docker/config.json", "docs/.netrc", "docs/.npmrc", "docs/.aws/config", "docs/.ssh/id_rsa", "docs/auth.json"}
	const marker = "credential-marker-must-never-reach-provider"
	dir := repoFixture(t)
	for _, p := range append(append([]string{}, paths...), "docs/config.json") {
		file := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		content := marker
		if p == "docs/config.json" {
			content = "{\"enabled\":true}"
		}
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "credential fixtures"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	for _, supplemental := range []bool{false, true} {
		t.Run(fmt.Sprintf("supplemental=%t", supplemental), func(t *testing.T) {
			ticket := "Acceptance: inspect `docs/config.json`."
			if !supplemental {
				for _, p := range paths {
					ticket += " Inspect `" + p + "`."
				}
			}
			s, err := CollectForTicket(context.Background(), dir, "HEAD", ticket)
			if err != nil {
				t.Fatal(err)
			}
			if supplemental {
				requests := []EvidenceRequest{}
				for _, p := range paths {
					requests = append(requests, EvidenceRequest{Path: p, Reason: "Inspect named configuration", Evidence: []string{"issue"}})
				}
				s, err = CollectSupplemental(context.Background(), dir, s, requests)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), marker) {
				t.Error("credential content leaked into provider snapshot")
			}
			outcomes := map[string]string{}
			for _, pass := range s.Collection {
				for _, outcome := range pass.Outcomes {
					outcomes[outcome.Path] = outcome.Reason
				}
			}
			for _, p := range paths {
				if outcomes[p] == "" || outcomes[p] == "collected" || outcomes[p] == "already collected" {
					t.Errorf("credential path %s not refused: %q", p, outcomes[p])
				}
				for _, evidence := range s.Evidence {
					if evidence.Path == p {
						t.Errorf("credential path %s included in evidence", p)
					}
				}
			}
			if outcomes["docs/config.json"] != "collected" {
				t.Fatal("ordinary configuration no longer collected")
			}
		})
	}
}

func repoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", args, b, err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Fixture")
	for p, s := range map[string]string{"lab/go.mod": "module test\n\ngo 1.27.0\n", "lab/a.go": "package test\n", "README.md": "# Test\n", ".github/workflows/test.yml": "name: checks\n", ".env": "SECRET=do-not-collect\n"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0700)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0600)
	}
	if err := os.Symlink("lab", filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "fixture")
	return dir
}
func TestFrozenRepositoryEvidence(t *testing.T) {
	dir := repoFixture(t)
	s, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) < 5 || len(s.Evidence) != 3 {
		t.Fatalf("unexpected evidence: %+v", s)
	}
	for _, e := range s.Evidence {
		if e.Path == ".env" || strings.Contains(e.Content, "do-not-collect") {
			t.Fatal("secret-like input collected")
		}
	}
	before, _ := s.Digest()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("uncommitted change"), 0600)
	again, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := again.Digest()
	if before != after {
		t.Fatal("collector read working files instead of commit")
	}
	p := fixture()
	p.Evidence = s.References()
	p.Inputs.EvidenceSHA256 = before
	var manifest string
	for _, e := range p.Evidence {
		if e.Path == "lab/go.mod" {
			manifest = e.ID
		}
	}
	p.Analysis.Scope = []ScopedPath{{Path: "lab/new.go", Reason: "New parser", Evidence: []string{manifest}}}
	if err := s.ValidateScope(p.Analysis.Scope); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"linked/new.go", "unrelated/new.go", "lab", ".git/config"} {
		scope := []ScopedPath{{Path: file, Reason: "test", Evidence: []string{manifest}}}
		if err := s.ValidateScope(scope); err == nil {
			t.Fatalf("accepted unsafe scope %s", file)
		}
	}
}
func TestCollectorRejectsSymlinkManifest(t *testing.T) {
	dir := repoFixture(t)
	os.Remove(filepath.Join(dir, "lab/go.mod"))
	os.Symlink("../README.md", filepath.Join(dir, "lab/go.mod"))
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "symlink"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	if _, err := Collect(context.Background(), dir, "HEAD"); err == nil {
		t.Fatal("symlink manifest accepted")
	}
}

func TestTicketEvidenceIncludesNamedSourceOnlyFromCommit(t *testing.T) {
	dir := repoFixture(t)
	os.WriteFile(filepath.Join(dir, "lab/a.go"), []byte("uncommitted"), 0600)
	got, err := CollectForTicket(context.Background(), dir, "HEAD", "Fix lab/a.go; inspect .env too")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range got.Evidence {
		if e.Path == ".env" {
			t.Fatal("secret collected")
		}
		if e.Path == "lab/a.go" {
			found = true
			if e.Content != "package test\n" {
				t.Fatal("working data used")
			}
		}
	}
	if !found {
		t.Fatal("missing named committed source")
	}
}

func TestNamedDocumentationRegression22(t *testing.T) {
	dir := repoFixture(t)
	os.MkdirAll(filepath.Join(dir, "docs"), 0700)
	os.WriteFile(filepath.Join(dir, "docs/agent-onboarding.md"), []byte("Go adapter documentation\n"), 0600)
	os.MkdirAll(filepath.Join(dir, "scripts"), 0700)
	os.WriteFile(filepath.Join(dir, "scripts/check-repo-hygiene.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "documentation"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	s, e := CollectForTicket(context.Background(), dir, "HEAD", "Acceptance: remove the sentence in `docs/agent-onboarding.md`.")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, f := range s.Evidence {
		if f.Path == "scripts/check-repo-hygiene.sh" {
			t.Fatal("checker was not named in acceptance prose")
		}
		if f.Path == "docs/agent-onboarding.md" && f.Content == "Go adapter documentation\n" {
			found = true
		}
	}
	if !found {
		t.Fatal("named Markdown content missing")
	}
	augmented, e := CollectSupplemental(context.Background(), dir, s, []EvidenceRequest{{Path: "scripts/check-repo-hygiene.sh", Reason: "Need checker behavior identified by analysis", Evidence: []string{"issue"}}})
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range augmented.Evidence {
		if f.Path == "scripts/check-repo-hygiene.sh" && f.Content == "#!/bin/sh\nexit 0\n" {
			return
		}
	}
	t.Fatal("explicitly requested checker missing")
}

func TestSupplementalEvidenceRefusalsAndDeterminism(t *testing.T) {
	dir := repoFixture(t)
	for p, c := range map[string]string{"docs/config.json": "{\"enabled\":true}", "docs/large.md": strings.Repeat("x", 64001), "docs/binary.txt": "a\x00b", "docs/control.txt": "a\x01b", ".npmrc": "token=do-not-collect", "private/notes.md": "do-not-collect"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0700)
		os.WriteFile(filepath.Join(dir, p), []byte(c), 0600)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "bounded fixtures"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	s, e := CollectForTicket(context.Background(), dir, "HEAD", "Acceptance: docs/config.json .env docs/large.md docs/binary.txt docs/control.txt .npmrc private/notes.md linked missing/file.md")
	if e != nil {
		t.Fatal(e)
	}
	reasons := map[string]string{}
	for _, o := range s.Collection[0].Outcomes {
		reasons[o.Path] = o.Reason
	}
	for _, p := range []string{".env", "docs/large.md", "docs/binary.txt", "docs/control.txt", ".npmrc", "private/notes.md", "linked", "missing/file.md"} {
		if reasons[p] == "" || reasons[p] == "collected" {
			t.Fatalf("missing refusal %s: %v", p, reasons)
		}
	}
	requests := []EvidenceRequest{{Path: "lab/a.go", Reason: "Inspect checker omitted from prose", Evidence: []string{"issue"}}}
	augmented, e := CollectSupplemental(context.Background(), dir, s, requests)
	if e != nil {
		t.Fatal(e)
	}
	if len(augmented.Evidence) != len(s.Evidence)+1 {
		t.Fatal("supplemental content missing")
	}
	before, _ := augmented.Digest()
	os.WriteFile(filepath.Join(dir, "lab/a.go"), []byte("working secret"), 0600)
	again, e := CollectForTicket(context.Background(), dir, s.Commit, "Acceptance: docs/config.json .env docs/large.md docs/binary.txt docs/control.txt .npmrc private/notes.md linked missing/file.md")
	if e != nil {
		t.Fatal(e)
	}
	again, e = CollectSupplemental(context.Background(), dir, again, requests)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := again.Digest()
	if before != after {
		t.Fatal("snapshot changed from working edits")
	}
	if _, e = CollectSupplemental(context.Background(), dir, augmented, requests); e == nil {
		t.Fatal("third pass allowed")
	}
}

func TestNamedEvidenceSharesCumulativeBound(t *testing.T) {
	dir := repoFixture(t)
	s, e := Collect(context.Background(), dir, "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 16; i++ {
		s.Collection = append(s.Collection, CollectionPass{Outcomes: []CollectionOutcome{{Path: fmt.Sprintf("old/%d.md", i), Reason: "collected"}}})
	}
	got, e := CollectSupplemental(context.Background(), dir, s, []EvidenceRequest{{Path: "lab/a.go", Reason: "Required", Evidence: []string{"issue"}}})
	if e != nil {
		t.Fatal(e)
	}
	if got.Collection[len(got.Collection)-1].Outcomes[0].Reason != "16 named-file bound reached" {
		t.Fatal("cumulative bound lost")
	}
}

func TestSupplementalTotalAndFileCountBounds(t *testing.T) {
	for _, kind := range []string{"bytes", "files"} {
		t.Run(kind, func(t *testing.T) {
			dir := repoFixture(t)
			s, e := Collect(context.Background(), dir, "HEAD")
			if e != nil {
				t.Fatal(e)
			}
			expected := "1000000-byte total bound exceeded"
			if kind == "bytes" {
				s.Evidence = append(s.Evidence, FileEvidence{Content: strings.Repeat("x", 1000000)})
			} else {
				expected = "256 evidence-file bound reached"
				for len(s.Evidence) < 256 {
					s.Evidence = append(s.Evidence, FileEvidence{})
				}
			}
			got, e := CollectSupplemental(context.Background(), dir, s, []EvidenceRequest{{Path: "lab/a.go", Reason: "Inspect", Evidence: []string{"issue"}}})
			if e != nil {
				t.Fatal(e)
			}
			if got.Collection[len(got.Collection)-1].Outcomes[0].Reason != expected {
				t.Fatal("bound not enforced")
			}
		})
	}
}
