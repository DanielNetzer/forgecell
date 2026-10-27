package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// writeLabFile writes content at rel below lab and returns its SHA-256.
func writeLabFile(t *testing.T, lab, rel string, content []byte) string {
	t.Helper()
	file := filepath.Join(lab, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// listEvidence adds a Lab-relative path to the code Atom's observed evidence;
// absolute is recorded as an absolute path inside the Lab, as the engine does.
func listEvidence(lab string, r *molecule.Record, rel, sum string, absolute bool) {
	path := rel
	if absolute {
		path = filepath.Join(lab, filepath.FromSlash(rel))
	}
	o := r.Atoms[1].Provenance.Actions[0].Observed
	o.Evidence = append(o.Evidence, molecule.ProvenanceEvidence{Path: path, SHA256: sum})
}

func evidencePath(id, rel string) string {
	return "/api/molecules/" + id + "/evidence?path=" + url.QueryEscape(rel)
}

func TestUIEvidenceServesAListedFileWithAMatchingSHA(t *testing.T) {
	lab, r := labFixture(t)
	content := "check output\nAuthorization: Bearer " + fakeBearer + "\nANTHROPIC_API_KEY=" + fakeAnthropic + "\nworkspace " + r.Workspace.Path + " lab " + lab + "\n<script>alert(1)</script>\n"
	atomFile := "verification-history/" + r.ID + "/observation-1.json"
	listEvidence(lab, &r, atomFile, writeLabFile(t, lab, atomFile, []byte(content)), true)
	harnessFile := "verification-history/" + r.ID + "/harness-h1.json"
	r.HarnessAttempts[0].Evidence = &molecule.VerificationFile{Path: harnessFile, SHA256: writeLabFile(t, lab, harnessFile, []byte(`{"ok":true}`))}
	captureFile := "verification-history/" + r.ID + "/capture-h1.json"
	r.HarnessAttempts[0].CaptureEvidence = []molecule.VerificationFile{{Path: captureFile, SHA256: writeLabFile(t, lab, captureFile, []byte(`{"tree":"t"}`))}}
	resultFile := "verification-history/" + r.ID + "/result-1.json"
	observationFile := "verification-history/" + r.ID + "/check-1.json"
	r.VerificationAttempts = []molecule.VerificationAttempt{{Status: "passed", Result: &molecule.VerificationFile{Path: resultFile, SHA256: writeLabFile(t, lab, resultFile, []byte(`{"passed":true}`))}, Observations: []molecule.VerificationFile{{Path: observationFile, SHA256: writeLabFile(t, lab, observationFile, []byte(`{"check":1}`))}}}}
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})

	w := do(t, h, authed(evidencePath(r.ID, atomFile)))
	raw := w.Body.String()
	var v TextView
	decode(t, w, &v)
	if v.Path != atomFile || v.Truncated || v.Size != int64(len(content)) || v.SHA256 == "" {
		t.Fatalf("view %+v", v)
	}
	for _, leak := range []string{fakeBearer, fakeAnthropic, lab, r.Workspace.Path, "<script>"} {
		if strings.Contains(raw, leak) {
			t.Errorf("evidence response carries %q", leak)
		}
	}
	if !strings.Contains(v.Text, secretMark) || !strings.Contains(v.Text, "<workspace>") || !strings.Contains(v.Text, "<lab>") || !strings.Contains(v.Text, "check output") || !strings.Contains(raw, `\`+"u003cscript") {
		t.Fatalf("text %q", v.Text)
	}
	for _, rel := range []string{harnessFile, captureFile, resultFile, observationFile} {
		var other TextView
		decode(t, do(t, h, authed(evidencePath(r.ID, rel))), &other)
		if other.Path != rel || other.Truncated || other.Text == "" {
			t.Errorf("%s: %+v", rel, other)
		}
	}
	var d Detail
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	ev := d.Atoms[1].Provenance.Actions[0].Observed.Evidence
	if !ev[len(ev)-1].Viewable || ev[0].Viewable || ev[2].Viewable || ev[4].Viewable {
		t.Fatalf("viewable flags %+v", ev)
	}
}

func TestUIEvidenceRefusesUnlistedTraversalAndAbsolutePaths(t *testing.T) {
	lab, r := labFixture(t)
	listed := "verification-history/" + r.ID + "/listed.json"
	listEvidence(lab, &r, listed, writeLabFile(t, lab, listed, []byte("listed")), false)
	writeLabFile(t, lab, "verification-history/"+r.ID+"/unlisted.json", []byte("unlisted secret"))
	writeLabFile(t, lab, "evidence/h1.json", []byte("listed with a malformed digest"))
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})
	for path, code := range map[string]int{
		evidencePath(r.ID, "verification-history/"+r.ID+"/unlisted.json"): http.StatusNotFound,
		// The fixture lists this path with the digest "aa", which can never match.
		evidencePath(r.ID, "evidence/h1.json"):                              http.StatusNotFound,
		evidencePath(r.ID, "ledgers/"+r.ID+".json"):                         http.StatusNotFound,
		evidencePath(r.ID, "../outside.json"):                               http.StatusBadRequest,
		evidencePath(r.ID, "verification-history/../ledgers/"+r.ID+".json"): http.StatusBadRequest,
		evidencePath(r.ID, "./"+listed):                                     http.StatusBadRequest,
		evidencePath(r.ID, "/etc/passwd"):                                   http.StatusBadRequest,
		evidencePath(r.ID, filepath.Join(lab, listed)):                      http.StatusBadRequest,
		evidencePath(r.ID, `verification-history\x.json`):                   http.StatusBadRequest,
		evidencePath(r.ID, ""):                                              http.StatusBadRequest,
		evidencePath(r.ID, "/harnessAttempts/0"):                            http.StatusBadRequest,
		"/api/molecules/" + r.ID + "/evidence":                              http.StatusBadRequest,
		evidencePath(r.ID, listed) + "&path=" + url.QueryEscape(listed):     http.StatusBadRequest,
		"/api/molecules/" + r.ID + "/evidence/extra?path=" + listed:         http.StatusNotFound,
		"/api/molecules/" + r.ID + "/logs":                                  http.StatusNotFound,
		evidencePath("absent", listed):                                      http.StatusNotFound,
		"/api/molecules/a.b/evidence?path=" + listed:                        http.StatusBadRequest,
	} {
		w := do(t, h, authed(path))
		if w.Code != code {
			t.Errorf("%s: %d, want %d: %s", path, w.Code, code, w.Body)
		}
		if strings.Contains(w.Body.String(), lab) || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s refusal carries content or a local path: %s", path, w.Body)
		}
	}
	if w := do(t, h, authed(evidencePath(r.ID, listed))); w.Code != http.StatusOK {
		t.Fatalf("listed file: %d %s", w.Code, w.Body)
	}
}

func TestUIEvidenceRefusesASHAMismatch(t *testing.T) {
	lab, r := labFixture(t)
	rel := "verification-history/" + r.ID + "/changed.json"
	listEvidence(lab, &r, rel, writeLabFile(t, lab, rel, []byte("original")), true)
	writeRecord(t, lab, r)
	writeLabFile(t, lab, rel, []byte("rewritten after the ledger recorded it"))
	w := do(t, testHandler(t, Options{LabDir: lab}), authed(evidencePath(r.ID, rel)))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "rewritten") || strings.Contains(w.Body.String(), lab) {
		t.Fatalf("mismatch: %d %s", w.Code, w.Body)
	}
}

func TestUIEvidenceRefusesSymlinksAndNonRegularFiles(t *testing.T) {
	lab, r := labFixture(t)
	dir := "verification-history/" + r.ID
	target := dir + "/target.json"
	sum := writeLabFile(t, lab, target, []byte("target secret"))
	// A symlinked file, inside the Lab, with the target's digest.
	if err := os.Symlink(filepath.Join(lab, filepath.FromSlash(target)), filepath.Join(lab, filepath.FromSlash(dir), "link.json")); err != nil {
		t.Fatal(err)
	}
	listEvidence(lab, &r, dir+"/link.json", sum, false)
	// A symlinked ancestor directory.
	if err := os.Symlink(filepath.Join(lab, "verification-history"), filepath.Join(lab, "linked-history")); err != nil {
		t.Fatal(err)
	}
	listEvidence(lab, &r, "linked-history/"+r.ID+"/target.json", sum, false)
	// A FIFO and a directory.
	fifo := dir + "/fifo.json"
	if err := syscall.Mkfifo(filepath.Join(lab, filepath.FromSlash(fifo)), 0600); err != nil {
		t.Fatal(err)
	}
	listEvidence(lab, &r, fifo, sum, false)
	listEvidence(lab, &r, dir, sum, false)
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})
	for _, rel := range []string{dir + "/link.json", "linked-history/" + r.ID + "/target.json", fifo, dir} {
		w := do(t, h, authed(evidencePath(r.ID, rel)))
		if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), lab) {
			t.Errorf("%s: %d %s", rel, w.Code, w.Body)
		}
	}
	listEvidence(lab, &r, dir+"/missing.json", sum, false)
	writeRecord(t, lab, r)
	if w := do(t, h, authed(evidencePath(r.ID, dir+"/missing.json"))); w.Code != http.StatusNotFound {
		t.Errorf("missing file: %d", w.Code)
	}
}

// An oversized log returns a bounded chunk with truncated=true and the full
// size, and a secret straddling the cut is not partly served.
func TestUIEvidenceOversizedLogReturnsABoundedChunk(t *testing.T) {
	lab, r := labFixture(t)
	rel := "verification-history/" + r.ID + "/big.log"
	// The token starts 9 bytes before the cut and ends after it.
	content := strings.Repeat("x", maxTextChunk-10) + " " + fakeGitHub + "\n" + strings.Repeat("after\n", maxTextChunk/6)
	listEvidence(lab, &r, rel, writeLabFile(t, lab, rel, []byte(content)), false)
	writeRecord(t, lab, r)
	var v TextView
	decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed(evidencePath(r.ID, rel))), &v)
	if !v.Truncated || v.Size != int64(len(content)) || len(v.Text) > maxTextChunk+len(secretMark) || len(v.Text) < maxTextChunk/2 {
		t.Fatalf("truncated %t size %d text %d", v.Truncated, v.Size, len(v.Text))
	}
	if strings.Contains(v.Text, "ghp_") || strings.Contains(v.Text, "after") || !strings.HasSuffix(v.Text, " "+secretMark) {
		t.Fatalf("the chunk carries part of the secret or text past the cut: %q", v.Text[len(v.Text)-80:])
	}

	// A file beyond the hash bound is refused before it is read, never served as
	// matching; a sparse file stands in for a huge log.
	huge := "verification-history/" + r.ID + "/huge.log"
	f, err := os.Create(filepath.Join(lab, filepath.FromSlash(huge)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxEvidenceHash + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	listEvidence(lab, &r, huge, strings.Repeat("0", 64), false)
	writeRecord(t, lab, r)
	if w := do(t, testHandler(t, Options{LabDir: lab}), authed(evidencePath(r.ID, huge))); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge file: %d %s", w.Code, w.Body)
	}
}

func TestUIEvidenceAndLedgerAreReadOnlyAndNeedTheSession(t *testing.T) {
	lab, r := labFixture(t)
	rel := "verification-history/" + r.ID + "/x.json"
	listEvidence(lab, &r, rel, writeLabFile(t, lab, rel, []byte("x")), false)
	writeRecord(t, lab, r)
	writeLabFile(t, lab, "ledgers/"+r.ID+".md", []byte("# Molecule\n"))
	before := snapshot(t, lab)
	h := testHandler(t, Options{LabDir: lab})
	for _, path := range []string{evidencePath(r.ID, rel), "/api/molecules/" + r.ID + "/ledger"} {
		if w := do(t, h, authed(path)); w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		noHeader := authed(path)
		noHeader.header = map[string]string{sessionHeader: ""}
		post := authed(path)
		post.method = http.MethodPost
		for name, req := range map[string]request{"no cookie": {path: path}, "no session header": noHeader} {
			if w := do(t, h, req); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), `"text"`) {
				t.Errorf("%s %s: %d", name, path, w.Code)
			}
		}
		if w := do(t, h, post); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", path, w.Code)
		}
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("the UI changed the Lab:\n%s\n---\n%s", before, after)
	}
	demo := testHandler(t, Options{Demo: true})
	for _, path := range []string{"/api/molecules/demo-101/ledger", evidencePath("demo-101", rel)} {
		if w := do(t, demo, authed(path)); w.Code != http.StatusNotFound {
			t.Errorf("demo %s: %d", path, w.Code)
		}
	}
}

func TestUILedgerTextServesTheMarkdownAsRedactedText(t *testing.T) {
	lab, r := labFixture(t)
	md := "# Molecule " + r.ID + "\n\n- Workspace: " + r.Workspace.Path + "\n\n## Exact Issue snapshot\n\n```json\n" + `{"body": "<img src=x onerror=alert(1)> GH_TOKEN=` + fakeGitHub + ` in ` + lab + `"}` + "\n```\n"
	writeLabFile(t, lab, "ledgers/"+r.ID+".md", []byte(md))
	h := testHandler(t, Options{LabDir: lab})
	w := do(t, h, authed("/api/molecules/"+r.ID+"/ledger"))
	raw := w.Body.String()
	var v TextView
	decode(t, w, &v)
	if v.Path != "ledgers/"+r.ID+".md" || v.Truncated || v.Size != int64(len(md)) || v.SHA256 != "" {
		t.Fatalf("view %+v", v)
	}
	for _, leak := range []string{lab, r.Workspace.Path, fakeGitHub, "<img"} {
		if strings.Contains(raw, leak) {
			t.Errorf("ledger text carries %q", leak)
		}
	}
	if !strings.Contains(v.Text, "<workspace>") || !strings.Contains(v.Text, "<lab>") || !strings.Contains(v.Text, "GH_TOKEN="+secretMark) || !strings.Contains(v.Text, "<img src=x") || !strings.Contains(raw, `\`+"u003cimg") {
		t.Fatalf("text %q", v.Text)
	}

	// A large ledger is cut and says so.
	big := md + strings.Repeat("- note\n", maxTextChunk/7+100)
	writeLabFile(t, lab, "ledgers/"+r.ID+".md", []byte(big))
	v = TextView{}
	decode(t, do(t, h, authed("/api/molecules/"+r.ID+"/ledger")), &v)
	if !v.Truncated || v.Size != int64(len(big)) || len(v.Text) > maxTextChunk+64 {
		t.Fatalf("big ledger: truncated %t size %d text %d", v.Truncated, v.Size, len(v.Text))
	}

	// A symlinked .md is refused; a missing one is reported as missing.
	file := filepath.Join(lab, "ledgers", r.ID+".md")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, authed("/api/molecules/"+r.ID+"/ledger")); w.Code != http.StatusNotFound {
		t.Fatalf("missing ledger text: %d", w.Code)
	}
	writeLabFile(t, lab, "elsewhere.md", []byte("secret elsewhere"))
	if err := os.Symlink(filepath.Join(lab, "elsewhere.md"), file); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, authed("/api/molecules/"+r.ID+"/ledger")); w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "elsewhere") {
		t.Fatalf("symlinked ledger text: %d %s", w.Code, w.Body)
	}
}

// The Atom panel keeps planned capabilities apart from what was observed, and
// actions taken apart from the context sources each action resolved.
func TestUIAtomPanelSeparatesPlannedFromObserved(t *testing.T) {
	lab, r := labFixture(t)
	p := r.Atoms[1].Provenance
	p.DeclaredSource, p.DeclaredWorkflows = "repository", []string{".github/workflows/lab.yml"}
	p.Actions[0].ResolvedSource = "workspace at " + r.Workspace.Path
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})
	var d Detail
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	pv := d.Atoms[1].Provenance
	if pv.DeclaredSource != "repository" || len(pv.DeclaredWorkflows) != 1 || pv.DeclaredBinding != "claude-code" || pv.PlannedAction != "edit" {
		t.Fatalf("planned %+v", pv)
	}
	if len(pv.Actions) != 1 || pv.Actions[0].ResolvedSource != "workspace at <workspace>" || pv.Actions[0].Observed == nil || pv.Actions[0].Observed.Action != "edit" {
		t.Fatalf("actions %+v", pv.Actions)
	}
	// The intake Atom records nothing and is served without inferred values.
	if d.Atoms[0].Provenance != nil {
		t.Fatalf("intake provenance inferred: %+v", d.Atoms[0].Provenance)
	}
	js := servedJS(t)
	for _, want := range []string{
		"'PLANNED CAPABILITIES · DECLARED, NOT OBSERVED'",
		"'ACTIONS TAKEN · OBSERVED'",
		"'CONTEXT SOURCES · AS RECORDED PER ACTION'",
		"'resolved source ' + notRecorded(x.resolvedSource)",
		"'/evidence?path=' + encodeURIComponent(e.path)",
		"'/ledger'",
		"e.viewable && e.path",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, "Logs · Unavailable") {
		t.Error("app.js still shows logs as unavailable")
	}
	page := do(t, h, authed("/")).Body.String()
	if !strings.Contains(page, `<pre id="dialog-body"></pre>`) || !strings.Contains(page, `id="dialog-note"`) {
		t.Fatal("index.html lacks the text dialog")
	}
}
