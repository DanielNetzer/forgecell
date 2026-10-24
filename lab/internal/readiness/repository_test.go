package readiness

import (
	"context"
	"crypto/sha256"
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
	second, e := CollectSupplemental(context.Background(), dir, augmented, []EvidenceRequest{{Path: "docs/config.json", Reason: "Inspect configuration again", Evidence: []string{"issue"}}})
	if e != nil {
		t.Fatalf("second supplemental pass refused: %v", e)
	}
	if _, e = CollectSupplemental(context.Background(), dir, second, requests); e == nil {
		t.Fatal("third supplemental pass allowed")
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

func sliceBFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := repoFixture(t)
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0700)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "slice b fixtures"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	return dir
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func sliceBRequests(paths ...string) []EvidenceRequest {
	requests := []EvidenceRequest{}
	for _, p := range paths {
		requests = append(requests, EvidenceRequest{Path: p, Reason: "Inspect", Evidence: []string{"issue"}})
	}
	return requests
}

// sliceBSupplement runs one supplemental pass and returns its outcome reasons by path.
func sliceBSupplement(t *testing.T, dir string, s RepositorySnapshot, paths ...string) (RepositorySnapshot, map[string]string) {
	t.Helper()
	s, err := CollectSupplemental(context.Background(), dir, s, sliceBRequests(paths...))
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, o := range s.Collection[len(s.Collection)-1].Outcomes {
		reasons[o.Path] = o.Reason
	}
	return s, reasons
}

func sliceBCollect(t *testing.T, dir string, paths ...string) (RepositorySnapshot, map[string]string) {
	t.Helper()
	s, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sliceBSupplement(t, dir, s, paths...)
}

func evidenceAt(s RepositorySnapshot, p string) (FileEvidence, bool) {
	for _, e := range s.Evidence {
		if e.Path == p {
			return e, true
		}
	}
	return FileEvidence{}, false
}

func TestSliceBRangeReturnsExactLinesWithMatchingHash(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/big.txt": numberedLines(3000), "docs/small.txt": "one\ntwo\nthree"})
	s, reasons := sliceBCollect(t, dir, "docs/big.txt#L10-12", "docs/small.txt#L2-3")
	if reasons["docs/big.txt#L10-12"] != "collected" {
		t.Fatalf("range outcome %q", reasons["docs/big.txt#L10-12"])
	}
	e, ok := evidenceAt(s, "docs/big.txt#L10-12")
	if !ok || e.Content != "line 10\nline 11\nline 12\n" {
		t.Fatalf("excerpt %q (found %t)", e.Content, ok)
	}
	if e.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(e.Content))) || !strings.HasPrefix(e.ID, "file-") {
		t.Fatalf("excerpt identity %+v", e.Evidence)
	}
	if e, ok := evidenceAt(s, "docs/small.txt#L2-3"); !ok || e.Content != "two\nthree" {
		t.Fatalf("final unterminated line excerpt %q", e.Content)
	}
	if _, again := sliceBSupplement(t, dir, s, "docs/big.txt#L10-12"); again["docs/big.txt#L10-12"] != "already collected" {
		t.Fatalf("repeated range: %q", again["docs/big.txt#L10-12"])
	}
}

func TestSliceBRangePastEOFIsClampedAndRecorded(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/five.txt": numberedLines(5), "docs/long.txt": numberedLines(4000)})
	s, reasons := sliceBCollect(t, dir, "docs/five.txt#L4-90", "docs/five.txt#L9-10", "docs/long.txt#L1-4000")
	if r := reasons["docs/five.txt#L4-90"]; r != "collected; range clamped to the end of the file at line 5" {
		t.Fatalf("clamp not recorded: %q", r)
	}
	if e, ok := evidenceAt(s, "docs/five.txt#L4-5"); !ok || e.Content != "line 4\nline 5\n" {
		t.Fatalf("clamped excerpt %q (found %t)", e.Content, ok)
	}
	if r := reasons["docs/five.txt#L9-10"]; collectedOutcome(r) || !strings.Contains(r, "past the end of the file") {
		t.Fatalf("range wholly past EOF: %q", r)
	}
	if r := reasons["docs/long.txt#L1-4000"]; r != "collected; range clamped to the 1500-line excerpt bound at line 1500" {
		t.Fatalf("line bound clamp: %q", r)
	}
	if e, ok := evidenceAt(s, "docs/long.txt#L1-1500"); !ok || strings.Count(e.Content, "\n") != 1500 {
		t.Fatal("1500-line excerpt missing")
	}
}

func TestSliceBRangeOnDeniedPathIsRefused(t *testing.T) {
	const marker = "do-not-collect-range"
	dir := sliceBFixture(t, map[string]string{"private/notes.md": marker + "\n", "docs/blob.png": marker + "\n", "dist/out.js": marker + "\n"})
	s, reasons := sliceBCollect(t, dir, "private/notes.md#L1-1", "docs/blob.png#L1-1", "dist/out.js#L1-1", ".env#L1-1")
	for _, file := range []string{"private/notes.md", "docs/blob.png", "dist/out.js", ".env"} {
		want := deniedText(file)
		if got := reasons[file+"#L1-1"]; want == "" || got != want {
			t.Fatalf("%s: reason %q, want %q", file, got, want)
		}
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), marker) || strings.Contains(string(raw), "do-not-collect\\n") {
		t.Fatal("denied range content collected")
	}
}

func TestSliceBOversizedGoFileReturnsOutline(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\nimport \"fmt\"\n\nconst Limit = 3\n\nvar (\n\tA = 1\n\tB, C = 2, 3\n)\n\ntype Thing struct{ N int }\n\nfunc (t Thing) String() string {\n\treturn fmt.Sprint(t.N)\n}\n\n")
	for i := 0; b.Len() <= 70000; i++ {
		fmt.Fprintf(&b, "func Helper%d() int {\n\treturn %d\n}\n\n", i, i)
	}
	src := b.String()
	dir := sliceBFixture(t, map[string]string{"lab/big.go": src, "docs/big.md": strings.Repeat("x\n", 40000)})
	s, reasons := sliceBCollect(t, dir, "lab/big.go", "docs/big.md")
	hint := fmt.Sprintf("file is %d bytes; request line ranges (path#Lstart-end) for content", len(src))
	if reasons["lab/big.go"] != "outline collected; "+hint {
		t.Fatalf("outline reason %q", reasons["lab/big.go"])
	}
	e, ok := evidenceAt(s, "lab/big.go#outline")
	if !ok || len(e.Content) > 64000 || e.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(e.Content))) {
		t.Fatalf("outline evidence missing or unbounded: %d bytes", len(e.Content))
	}
	for _, want := range []string{hint, "\nL5-5 const Limit\n", "\nL8-8 var A\n", "\nL9-9 var B, C\n", "\nL12-12 type Thing\n", "\nL14-16 func (t Thing) String() string\n", "\nL18-20 func Helper0() int\n"} {
		if !strings.Contains(e.Content, want) {
			t.Fatalf("outline lacks %q:\n%s", want, e.Content[:500])
		}
	}
	if strings.Contains(e.Content, "return 0") || strings.Contains(e.Content, "import") {
		t.Fatal("outline carries bodies or imports")
	}
	if _, whole := evidenceAt(s, "lab/big.go"); whole {
		t.Fatal("outline recorded under the whole-file path")
	}
	if r := reasons["docs/big.md"]; r != "64000-byte per-file bound exceeded; file is 80000 bytes; request line ranges (path#Lstart-end) for content" {
		t.Fatalf("non-Go oversized reason %q", r)
	}
	if _, ok := evidenceAt(s, "docs/big.md"); ok {
		t.Fatal("non-Go oversized file collected")
	}
	s, reasons = sliceBSupplement(t, dir, s, "lab/big.go#L14-16")
	if e, ok := evidenceAt(s, "lab/big.go#L14-16"); !ok || reasons["lab/big.go#L14-16"] != "collected" || e.Content != "func (t Thing) String() string {\n\treturn fmt.Sprint(t.N)\n}\n" {
		t.Fatalf("range from outline: %q %q", reasons["lab/big.go#L14-16"], e.Content)
	}
}

func TestSliceBExcerptsCountTowardTotalAndNamedBounds(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/big.txt": numberedLines(3000)})
	s, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	used := 0
	for _, e := range s.Evidence {
		used += len(e.Content)
	}
	// "line 1\n" fits in the remaining 10 bytes; three lines do not.
	padded := s
	padded.Evidence = append(append([]FileEvidence{}, s.Evidence...), FileEvidence{Content: strings.Repeat("x", 1000000-used-10)})
	_, reasons := sliceBSupplement(t, dir, padded, "docs/big.txt#L1-1", "docs/big.txt#L1-3")
	if reasons["docs/big.txt#L1-1"] != "collected" || reasons["docs/big.txt#L1-3"] != "1000000-byte total bound exceeded" {
		t.Fatalf("total bound must count excerpt size: %v", reasons)
	}
	for i := 0; i < 15; i++ {
		s.Collection = append(s.Collection, CollectionPass{Outcomes: []CollectionOutcome{{Path: fmt.Sprintf("old/%d.md", i), Reason: "collected"}}})
	}
	_, reasons = sliceBSupplement(t, dir, s, "docs/big.txt#L1-1", "docs/big.txt#L5-6")
	if reasons["docs/big.txt#L1-1"] != "collected" || reasons["docs/big.txt#L5-6"] != "16 named-file bound reached" {
		t.Fatalf("excerpts must count toward the 16 named-file bound: %v", reasons)
	}
}

func TestSliceBTwoSupplementalPassesThenExhausted(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/a.md": "a\n", "docs/b.md": "b\n", "docs/c.md": "c\n"})
	s, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	s, _ = sliceBSupplement(t, dir, s, "docs/a.md")
	s, _ = sliceBSupplement(t, dir, s, "docs/b.md#L1-1")
	if s.SupplementalPasses() != 2 || MaxSupplementalPasses != 2 {
		t.Fatalf("passes %d", s.SupplementalPasses())
	}
	third := sliceBRequests("docs/c.md", "docs/b.md#L1-1")
	if _, err = CollectSupplemental(context.Background(), dir, s, third); err == nil || err.Error() != "supplemental evidence passes exhausted" {
		t.Fatalf("third pass: %v", err)
	}
	if got := s.UnmetRequests(third); got != "docs/c.md (not collected: no supplemental pass remains); already collected: docs/b.md#L1-1" {
		t.Fatalf("unmet list %q", got)
	}
}

func TestSliceBNonRangeHashSuffixIsAWholeFile(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/issue#12.md": "twelve\n", "docs/a.md#Lx-5": "odd\n"})
	s, reasons := sliceBCollect(t, dir, "docs/issue#12.md", "docs/a.md#Lx-5")
	for file, content := range map[string]string{"docs/issue#12.md": "twelve\n", "docs/a.md#Lx-5": "odd\n"} {
		if e, ok := evidenceAt(s, file); reasons[file] != "collected" || !ok || e.Content != content {
			t.Fatalf("%s: reason %q content %q", file, reasons[file], e.Content)
		}
	}
	if got := s.UnmetRequests(sliceBRequests("docs/issue#12.md")); got != "none; already collected: docs/issue#12.md" {
		t.Fatalf("unmet list %q", got)
	}
}

func TestSliceBGoOutlinePrintsSignaturesWithoutBodies(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\nfunc key() string { return \"secret\" }\n\nvar token = \"hunter2\"\n\ntype pair struct{ A, B string }\n\nfunc join(\n\ta string,\n\tb string,\n) (string, error) { return a + b, nil }\n\n")
	for b.Len() <= 70000 {
		b.WriteString("// filler\n")
	}
	dir := sliceBFixture(t, map[string]string{"lab/big.go": b.String()})
	s, _ := sliceBCollect(t, dir, "lab/big.go")
	e, ok := evidenceAt(s, "lab/big.go#outline")
	if !ok {
		t.Fatal("outline missing")
	}
	for _, want := range []string{"\nL3-3 func key() string\n", "\nL5-5 var token\n", "\nL7-7 type pair\n", "\nL9-12 func join( a string, b string, ) (string, error)\n"} {
		if !strings.Contains(e.Content, want) {
			t.Fatalf("outline lacks %q:\n%s", want, e.Content)
		}
	}
	for _, leaked := range []string{"secret", "hunter2", "return", "A, B"} {
		if strings.Contains(e.Content, leaked) {
			t.Fatalf("outline leaks %q:\n%s", leaked, e.Content)
		}
	}
}

func TestSliceBGoOutlineSurvivesMalformedSignatures(t *testing.T) {
	const valid = "\nfunc Good() int {\n\treturn 1\n}\n"
	for name, src := range map[string]string{
		"unclosed params":    "package big\n\nfunc F(a int {\n\treturn\n}\n" + valid,
		"unclosed results":   "package big\n\nfunc F(a int) ( {\n}\n" + valid,
		"params then var":    "package big\n\nfunc F(\nvar x = 1\n" + valid,
		"truncated at EOF":   "package big\n\nfunc F(",
		"one-line body leak": "package big\n\nfunc F(a int { return \"secret\" }\n" + valid,
	} {
		t.Run(name, func(t *testing.T) {
			var out string
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("outline panicked: %v", r)
					}
				}()
				out = goOutline("lab/big.go", src)
			}()
			if !strings.HasPrefix(out, "// Outline of lab/big.go: ") || !strings.Contains(out, "// The file did not parse completely; later declarations may be missing.\n") {
				t.Fatalf("outline lacks the header or the parse fallback:\n%s", out)
			}
			for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
				var start, end int
				if _, err := fmt.Sscanf(line, "L%d-%d", &start, &end); err == nil && (start < 1 || end < start) {
					t.Fatalf("invalid range in %q", line)
				}
			}
			if strings.Contains(out, "secret") || strings.Contains(out, "return") {
				t.Fatalf("outline leaks a body:\n%s", out)
			}
		})
	}
	// Each case also collects through the real path without failing intake.
	dir := sliceBFixture(t, map[string]string{"lab/big.go": "package big\n\nfunc F(a int {\n" + strings.Repeat("// filler\n", 8000)})
	if _, reasons := sliceBCollect(t, dir, "lab/big.go"); !strings.HasPrefix(reasons["lab/big.go"], "outline collected; ") {
		t.Fatalf("malformed Go outline reason %q", reasons["lab/big.go"])
	}
}

func TestSliceBUnmetRequestsNormalizeSingleLineRanges(t *testing.T) {
	dir := sliceBFixture(t, map[string]string{"docs/a.txt": numberedLines(10)})
	s, reasons := sliceBCollect(t, dir, "docs/a.txt#L5")
	if reasons["docs/a.txt#L5"] != "collected" {
		t.Fatalf("single-line range outcome %q", reasons["docs/a.txt#L5"])
	}
	if got := s.UnmetRequests(sliceBRequests("docs/a.txt#L5-5", "docs/a.txt#L6")); got != "docs/a.txt#L6 (not collected: no supplemental pass remains); already collected: docs/a.txt#L5-5" {
		t.Fatalf("unmet list %q", got)
	}
}
