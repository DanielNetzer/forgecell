package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

type wrapperLab struct {
	o        Options
	repo     string
	script   string
	config   string
	dep      string
	evidence string
	stdin    string
	cwd      string
	ran      string
	yaml     string
	path     string
}

type wrapperOptions struct {
	body      string
	timeoutMS int
	mutate    func(*harness.AnalysisWrapperManifest)
}

func wrapperSum(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// newWrapperLab approves a Formula that keeps the fixture's custom coding
// command and adds a separately pinned analysis wrapper. Only local scripts
// under t.TempDir() are used; no provider is called.
func newWrapperLab(t *testing.T, opts wrapperOptions) *wrapperLab {
	t.Helper()
	o, repo := fixture(t)
	analyze := o.Analyze
	o.Analyze = nil
	// Pinned paths must not resolve through symlinks, and macOS temporary
	// directories do, so the wrapper files live under the resolved root.
	root, err := filepath.EvalSymlinks(filepath.Dir(o.LabDir))
	if err != nil {
		t.Fatal(err)
	}
	interp, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skip("no system shell to use as the pinned interpreter")
	}
	dir := filepath.Join(root, "wrapper")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &wrapperLab{o: o, repo: repo, script: filepath.Join(dir, "wrapper.sh"), config: filepath.Join(dir, "config.json"), dep: filepath.Join(dir, "dependency.bin"), stdin: filepath.Join(dir, "stdin.json"), cwd: filepath.Join(dir, "cwd"), ran: filepath.Join(dir, "ran")}
	policy := filepath.Join(dir, "policy.json")
	analysisFile := filepath.Join(dir, "analysis.json")
	base := testGit(t, repo, "rev-parse", "HEAD")
	snapshot, err := readiness.CollectForTicket(context.Background(), repo, base, "Change code\nWrite changed")
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := analyze(context.Background(), formula.Binding{}, Issue{}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(analysis)
	w.evidence = filepath.Join(dir, "review.txt")
	for path, content := range map[string]string{analysisFile: string(raw), w.config: `{"mode":"analysis"}`, policy: `{"deny":["write"]}`, w.dep: "reviewed dependency", w.evidence: "External review of the wrapper enforcement."} {
		if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := opts.body
	if body == "" {
		body = fmt.Sprintf("cat > %q\npwd > %q\ntouch %q\ncat %q\n", w.stdin, w.cwd, w.ran, analysisFile)
	} else {
		body = fmt.Sprintf("touch %q\n", w.ran) + body
	}
	if err = os.WriteFile(w.script, []byte("#!"+interp+"\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	m := harness.AnalysisWrapperManifest{SchemaVersion: "v1", Kind: "analysis-wrapper", Args: []string{"--analysis-only"}}
	m.Executable = harness.PinnedFile{Path: w.script, SHA256: wrapperSum(t, w.script)}
	m.Configuration = []harness.PinnedFile{{Path: w.config, SHA256: wrapperSum(t, w.config)}}
	m.Interpreter = &harness.PinnedFile{Path: interp, SHA256: wrapperSum(t, interp)}
	m.Policy = []harness.PinnedFile{{Path: policy, SHA256: wrapperSum(t, policy)}}
	m.Dependencies = []harness.WrapperDependency{{Path: w.dep, SHA256: wrapperSum(t, w.dep), Role: "enforcement-critical", State: "pinned"}}
	m.Controls = harness.WrapperControls{Inputs: []string{"stdin"}, ScratchWrites: "temporary-directory", Processes: "wrapper-managed", Tools: "none", Network: "none", Credentials: []string{}}
	m.Enforcement = []harness.WrapperEvidence{{ID: "review-1", Path: w.evidence, SHA256: wrapperSum(t, w.evidence), Origin: "external-review", Limitations: "Reviewer inspected the wrapper source only.", ReviewBasis: "Manual source review."}}
	if opts.mutate != nil {
		opts.mutate(&m)
	}
	manifest, _ := json.Marshal(m)
	w.path = filepath.Join(o.LabDir, "analysis-wrappers", "wrapper.json")
	if err = os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(w.path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	current, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	insert := "  analysisWrapper: {manifest: analysis-wrappers/wrapper.json, sha256: " + hex.EncodeToString(digest[:]) + "}\n"
	if opts.timeoutMS > 0 {
		insert += fmt.Sprintf("  timeoutMs: %d\n", opts.timeoutMS)
	}
	w.yaml = strings.Replace(string(current), "atoms:\n", insert+"atoms:\n", 1)
	approveFixture(t, o.LabDir, w.yaml, "wrapper")
	return w
}

func wrapperDecision(t *testing.T, o Options, r Record, ordinal int) harness.AnalysisWrapperDecision {
	t.Helper()
	want := fmt.Sprintf("analysis-wrapper-%d-decision.json", ordinal)
	for _, a := range r.Atoms {
		if a.Type != "intake" || a.Provenance == nil {
			continue
		}
		for _, action := range a.Provenance.Actions {
			if action.Observed == nil {
				continue
			}
			for _, e := range action.Observed.Evidence {
				if !strings.HasSuffix(e.Path, want) {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(o.LabDir, e.Path))
				if err != nil {
					t.Fatal(err)
				}
				if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != e.SHA256 {
					t.Fatalf("decision evidence digest does not match %s", e.Path)
				}
				var d harness.AnalysisWrapperDecision
				if err = json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				return d
			}
		}
	}
	t.Fatalf("intake provenance does not cite %s", want)
	return harness.AnalysisWrapperDecision{}
}

func TestAnalysisWrapperApprovedCustomAnalysisPreservesCodingBinding(t *testing.T) {
	w := newWrapperLab(t, wrapperOptions{})
	r, err := Run(context.Background(), w.o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || r.Readiness == nil || len(r.Readiness.Plans) != 1 || len(r.HarnessAttempts) != 0 {
		t.Fatalf("intake must wait at the unchanged human scope gate with no coding yet: %+v", r)
	}
	if len(r.AnalysisAttempts) != 1 || r.AnalysisAttempts[0].Status != "completed" {
		t.Fatalf("%+v", r.AnalysisAttempts)
	}
	d := wrapperDecision(t, w.o, r, 1)
	if d.Status != "approved" || d.Manifest == nil || d.Digest == "" || !strings.Contains(d.Notice, "wrapper claims") || d.Manifest.Controls.Network != "none" {
		t.Fatalf("%+v", d)
	}
	verified := false
	for _, id := range d.Identities {
		verified = verified || (strings.HasPrefix(id.Role, "enforcement evidence") && id.Path == w.evidence && id.SHA256 == wrapperSum(t, w.evidence))
	}
	if !verified || len(d.Relaxations) != 2 {
		t.Fatalf("the decision must record the verified evidence digest and the relaxed controls: %+v", d)
	}
	sourced := false
	for _, a := range r.Atoms {
		if a.Type == "intake" && a.Provenance != nil {
			for _, action := range a.Provenance.Actions {
				sourced = sourced || action.ResolvedSource == "analysis-wrapper:custom@sha256:"+wrapperSum(t, w.path)
			}
		}
	}
	if !sourced {
		t.Fatal("the intake provenance must record the wrapper identity as the analysis source")
	}
	noted := false
	for _, n := range r.Notes {
		noted = noted || (strings.Contains(n, "Analysis wrapper decision") && strings.Contains(n, "wrapper claims"))
	}
	if !noted {
		t.Fatalf("the ledger must record the capability decision as wrapper claims: %v", r.Notes)
	}
	var envelope struct {
		Protocol string         `json:"protocol"`
		Request  map[string]any `json:"request"`
	}
	raw, err := os.ReadFile(w.stdin)
	if err != nil || json.Unmarshal(raw, &envelope) != nil {
		t.Fatalf("wrapper stdin: %v", err)
	}
	contract, _ := envelope.Request["formulaCheckContract"].([]any)
	if envelope.Protocol != harness.AnalysisWrapperProtocol || envelope.Request["kind"] != "ticket-analysis" || envelope.Request["issue"] == nil || envelope.Request["repository"] == nil || envelope.Request["humanVerification"] == nil || len(contract) != 1 {
		t.Fatalf("the wrapper must receive frozen issue, repository evidence and the check contract: %s", raw)
	}
	cwd, _ := os.ReadFile(w.cwd)
	if dir := strings.TrimSpace(string(cwd)); strings.HasPrefix(dir, w.repo) || strings.HasPrefix(dir, w.o.LabDir) || !strings.Contains(dir, "forgecell-analysis-wrapper-") {
		t.Fatalf("the wrapper must run in a temporary directory, ran in %q", dir)
	}
	if r.Readiness.Plans[0].Plan.Inputs.BindingSHA256 == "" {
		t.Fatal("the plan must still bind the Formula binding")
	}

	w.o.Approve = r.Readiness.Plans[0].Digest
	coded, err := Run(context.Background(), w.o)
	if err != nil {
		t.Fatal(err)
	}
	if coded.Status != "waiting" || len(coded.HarnessAttempts) != 1 || coded.HarnessAttempts[0].Coding == nil || coded.HarnessAttempts[0].Coding.Outcome != "completed" {
		t.Fatalf("the unchanged custom coding binding must run after the human approval: %+v", coded.HarnessAttempts)
	}
	if got, _ := os.ReadFile(filepath.Join(coded.Workspace.Path, "code.txt")); string(got) != "changed" {
		t.Fatal("coding did not run through the preserved binding")
	}
}

func TestAnalysisWrapperFailuresStopIntakeWithZeroCodingOrFallback(t *testing.T) {
	appendTo := func(path, text string) func(*wrapperLab) {
		return func(w *wrapperLab) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.WriteString(text)
		}
	}
	cases := []struct {
		name    string
		opts    wrapperOptions
		after   func(w *wrapperLab)
		failure string
		started bool
		status  string
	}{
		{"missing evidence", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) { m.Enforcement = nil }}, nil, "missing-evidence", false, "refused"},
		{"self attested evidence", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) { m.Enforcement[0].Origin = "self-attested" }}, nil, "self-attested", false, "refused"},
		{"denied control", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) { m.Controls.Network = "unrestricted" }}, nil, "denied", false, "refused"},
		{"unknown control", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) { m.Controls.Tools = "" }}, nil, "unknown", false, "refused"},
		{"unknown dependency", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) {
			m.Dependencies = append(m.Dependencies, harness.WrapperDependency{Path: "/usr/lib/mutable.so", Role: "enforcement-critical", State: "unknown"})
		}}, nil, "unknown", false, "refused"},
		{"changed executable", wrapperOptions{}, func(w *wrapperLab) { appendTo(w.script, "\n# tampered\n")(w) }, "changed", false, "refused"},
		{"changed configuration", wrapperOptions{}, func(w *wrapperLab) { appendTo(w.config, " ")(w) }, "changed", false, "refused"},
		{"changed dependency", wrapperOptions{}, func(w *wrapperLab) { appendTo(w.dep, " patched")(w) }, "changed", false, "refused"},
		{"changed manifest", wrapperOptions{}, func(w *wrapperLab) { appendTo(w.path, " ")(w) }, "changed", false, "refused"},
		{"changed evidence", wrapperOptions{}, func(w *wrapperLab) { appendTo(w.evidence, " amended")(w) }, "changed", false, "refused"},
		{"evidence points at nothing", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) {
			m.Enforcement[0].Path = filepath.Join(filepath.Dir(m.Enforcement[0].Path), "absent-review.txt")
			m.Enforcement[0].SHA256 = strings.Repeat("a", 64)
		}}, nil, "missing-evidence", false, "refused"},
		{"symlinked executable", wrapperOptions{}, func(w *wrapperLab) {
			if err := os.Rename(w.script, w.script+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(w.script+".real", w.script); err != nil {
				t.Fatal(err)
			}
		}, "changed", false, "refused"},
		{"credential overriding the environment", wrapperOptions{mutate: func(m *harness.AnalysisWrapperManifest) { m.Controls.Credentials = []string{"LD_PRELOAD"} }}, nil, "denied", false, "refused"},
		{"process failure with stderr", wrapperOptions{body: "cat >/dev/null\necho secret-stderr-detail >&2\nexit 3\n"}, nil, "analysis failed", true, "approved"},
		{"process failure", wrapperOptions{body: "cat >/dev/null\nexit 3\n"}, nil, "analysis failed", true, "approved"},
		{"timeout", wrapperOptions{body: "cat >/dev/null\nsleep 60 &\nwait\n", timeoutMS: stallTimeoutMS}, nil, "deadline", true, "approved"},
		{"output overflow", wrapperOptions{body: "cat >/dev/null\nhead -c 300000 /dev/zero | tr '\\0' a\n"}, nil, "byte limit", true, "approved"},
		{"malformed analysis", wrapperOptions{body: "cat >/dev/null\nprintf 'not json'\n"}, nil, "invalid readiness analysis", true, "approved"},
		{"unknown analysis field", wrapperOptions{body: "cat >/dev/null\nprintf '%s' '{\"summary\":\"x\",\"approved\":true}'\n"}, nil, "invalid readiness analysis", true, "approved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWrapperLab(t, tc.opts)
			if tc.after != nil {
				tc.after(w)
			}
			r, err := Run(context.Background(), w.o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 || r.CheckoutStatus != "absent" || r.Workspace.Path != "" {
				t.Fatalf("a wrapper failure must stop intake before any coding checkout or harness: %+v", r)
			}
			if len(r.AnalysisAttempts) != 1 || r.AnalysisAttempts[0].Status != "failed" || !strings.Contains(r.AnalysisAttempts[0].Error, tc.failure) {
				t.Fatalf("expected one failed attempt mentioning %q, got %+v", tc.failure, r.AnalysisAttempts)
			}
			if strings.Contains(r.AnalysisAttempts[0].Error, "secret-stderr-detail") {
				t.Fatalf("wrapper stderr must not be persisted in the ledger: %q", r.AnalysisAttempts[0].Error)
			}
			d := wrapperDecision(t, w.o, r, 1)
			if d.Status != tc.status || !strings.Contains(d.Notice, "wrapper claims") {
				t.Fatalf("the persisted decision must record the capability evidence: %+v", d)
			}
			if tc.status == "refused" && len(d.Findings) == 0 {
				t.Fatalf("a refusal must persist its findings: %+v", d)
			}
			_, statErr := os.Stat(w.ran)
			if started := statErr == nil; started != tc.started {
				t.Fatalf("wrapper process started=%v, want %v", started, tc.started)
			}
		})
	}
}

func TestAnalysisWrapperStaleFormulaApprovalStartsNothing(t *testing.T) {
	w := newWrapperLab(t, wrapperOptions{})
	f, err := os.OpenFile(filepath.Join(w.o.LabDir, "formulas", "sample.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("# edited after approval\n")
	f.Close()
	if _, err = Run(context.Background(), w.o); err == nil {
		t.Fatal("an edited Formula must not run")
	}
	if _, statErr := os.Stat(w.ran); statErr == nil {
		t.Fatal("wrapper process started under a stale Formula approval")
	}
}

func TestAnalysisWrapperHistoricalCustomBindingGainsNoAnalysisCapability(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = nil
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 || len(r.AnalysisAttempts) != 1 || !strings.Contains(r.AnalysisAttempts[0].Error, "no verified read-only ticket-analysis protocol") {
		t.Fatalf("a custom binding without an approved wrapper must still stop intake: %+v", r)
	}
}
