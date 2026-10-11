package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const wrapperOKAnalysis = `{"summary":"Read-only wrapper analysis"}`

type wrapperFixture struct {
	t        *testing.T
	lab      string
	dir      string
	script   string
	config   string
	policy   string
	dep      string
	evidence string
	interp   string
	manifest AnalysisWrapperManifest
}

type manifestCase struct {
	name   string
	mutate func(*AnalysisWrapperManifest)
	class  string
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// newWrapperFixture builds an approved-looking local wrapper from files under
// t.TempDir(). Nothing outside the temporary tree is installed or executed.
func newWrapperFixture(t *testing.T, body string) *wrapperFixture {
	t.Helper()
	// Pinned paths must not resolve through symlinks, and macOS temporary
	// directories do, so the fixture works from the resolved root.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interp, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skip("no system shell to use as the pinned interpreter")
	}
	fx := &wrapperFixture{t: t, lab: filepath.Join(root, "lab"), dir: filepath.Join(root, "wrapper"), interp: interp}
	for _, d := range []string{fx.lab, fx.dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fx.script = filepath.Join(fx.dir, "wrapper.sh")
	fx.config = filepath.Join(fx.dir, "config.json")
	fx.policy = filepath.Join(fx.dir, "policy.json")
	fx.dep = filepath.Join(fx.dir, "dependency.bin")
	fx.evidence = filepath.Join(fx.dir, "review.txt")
	for path, content := range map[string]string{fx.config: `{"mode":"analysis"}`, fx.policy: `{"deny":["write"]}`, fx.dep: "reviewed dependency", fx.evidence: "External review of the wrapper enforcement."} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fx.setScript(body)
	m := AnalysisWrapperManifest{SchemaVersion: "v1", Kind: "analysis-wrapper", Args: []string{"--analysis-only"}}
	m.Executable = PinnedFile{Path: fx.script}
	m.Configuration = []PinnedFile{{Path: fx.config}}
	m.Interpreter = &PinnedFile{Path: fx.interp}
	m.Policy = []PinnedFile{{Path: fx.policy}}
	m.Dependencies = []WrapperDependency{{Path: fx.dep, Role: "enforcement-critical", State: "pinned"}}
	m.Controls = WrapperControls{Inputs: []string{"stdin"}, ScratchWrites: "temporary-directory", Processes: "wrapper-managed", Tools: "none", Network: "none", Credentials: []string{}}
	m.Enforcement = []WrapperEvidence{{ID: "review-1", Path: fx.evidence, Origin: "external-review", Limitations: "Reviewer inspected the wrapper source only.", ReviewBasis: "Manual source review."}}
	fx.manifest = m
	fx.pin()
	return fx
}

func (fx *wrapperFixture) setScript(body string) {
	fx.t.Helper()
	if err := os.WriteFile(fx.script, []byte("#!"+fx.interp+"\n"+body), 0o700); err != nil {
		fx.t.Fatal(err)
	}
}

// pin recomputes every identity from the files as they are now.
func (fx *wrapperFixture) pin() {
	fx.manifest.Executable.SHA256 = fileSum(fx.t, fx.script)
	fx.manifest.Interpreter.SHA256 = fileSum(fx.t, fx.interp)
	fx.manifest.Configuration[0].SHA256 = fileSum(fx.t, fx.config)
	fx.manifest.Policy[0].SHA256 = fileSum(fx.t, fx.policy)
	fx.manifest.Dependencies[0].SHA256 = fileSum(fx.t, fx.dep)
	fx.manifest.Enforcement[0].SHA256 = fileSum(fx.t, fx.evidence)
}

// write stores the manifest and returns its Lab-relative path and digest.
func (fx *wrapperFixture) write() (string, string) {
	fx.t.Helper()
	raw, err := json.Marshal(fx.manifest)
	if err != nil {
		fx.t.Fatal(err)
	}
	rel := "analysis-wrappers/wrapper.json"
	if err = os.MkdirAll(filepath.Join(fx.lab, "analysis-wrappers"), 0o700); err != nil {
		fx.t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(fx.lab, filepath.FromSlash(rel)), raw, 0o600); err != nil {
		fx.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return rel, hex.EncodeToString(sum[:])
}

func (fx *wrapperFixture) approve() AnalysisWrapperDecision {
	rel, sum := fx.write()
	return ApproveAnalysisWrapper(fx.lab, rel, sum)
}

func analysisRequestFixture() map[string]any {
	return map[string]any{"kind": "ticket-analysis", "issue": map[string]any{"number": 2, "title": "BYO analysis"}, "repository": map[string]any{"commit": "abc"}, "humanVerification": map[string]any{"checks": []any{}}}
}

func assertRefused(t *testing.T, d AnalysisWrapperDecision, class string) {
	t.Helper()
	if d.Status != "refused" || d.Err() == nil {
		t.Fatalf("expected a refusal, got %+v", d)
	}
	found := false
	for _, f := range d.Findings {
		found = found || strings.HasPrefix(f, class+":")
	}
	if !found {
		t.Fatalf("expected a %q finding, got %v (reason %q)", class, d.Findings, d.Reason)
	}
}

func runManifestCases(t *testing.T, cases []manifestCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			fx := newWrapperFixture(t, fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", marker, wrapperOKAnalysis))
			tc.mutate(&fx.manifest)
			d := fx.approve()
			assertRefused(t, d, tc.class)
			if result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second); result.OK {
				t.Fatalf("a refused decision must not run: %+v", result)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("wrapper process started despite a refusal")
			}
		})
	}
}

func TestAnalysisWrapperApprovedInvocationIsBoundedAndFiltered(t *testing.T) {
	out := t.TempDir()
	stdin := filepath.Join(out, "stdin.json")
	fx := newWrapperFixture(t, "true\n")
	fx.setScript(fmt.Sprintf("cat > %q\npwd > %q\nenv > %q\nprintf '%%s' '%s'\n", stdin, filepath.Join(out, "cwd"), filepath.Join(out, "env"), wrapperOKAnalysis))
	fx.pin()
	t.Setenv("UNDECLARED_TOKEN", "must-not-leak")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/evil.dylib")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("PATH", "/ambient/bin:"+os.Getenv("PATH"))
	d := fx.approve()
	if err := d.Err(); err != nil || d.Status != "approved" || d.Digest == "" || !strings.Contains(d.Notice, "wrapper claims") || d.Manifest == nil {
		t.Fatalf("%+v %v", d, err)
	}
	if len(d.Relaxations) != 2 || !strings.Contains(d.Relaxations[0], "relaxation") {
		t.Fatalf("relaxed controls must be recorded with matching wording: %v", d.Relaxations)
	}
	want := map[string]string{"executable": fx.script, "interpreter": fx.interp, "configuration": fx.config, "policy": fx.policy, "dependency": fx.dep, "enforcement evidence review-1": fx.evidence}
	if len(d.Identities) != len(want) {
		t.Fatalf("every pinned identity must be recorded as verified: %+v", d.Identities)
	}
	for _, id := range d.Identities {
		if want[id.Role] != id.Path || id.SHA256 != fileSum(t, id.Path) {
			t.Fatalf("recorded digest must be the one computed from the file: %+v", id)
		}
	}
	result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 10*time.Second)
	if !result.OK || result.Stdout != wrapperOKAnalysis {
		t.Fatalf("%+v", result)
	}
	cwdRaw, _ := os.ReadFile(filepath.Join(out, "cwd"))
	cwd := strings.TrimSpace(string(cwdRaw))
	if !strings.Contains(cwd, "forgecell-analysis-wrapper-") || strings.HasPrefix(cwd, fx.lab) || strings.HasPrefix(cwd, fx.dir) {
		t.Fatalf("wrapper must run in a temporary directory, ran in %q", cwd)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatal("temporary working directory was not removed")
	}
	envRaw, _ := os.ReadFile(filepath.Join(out, "env"))
	env := string(envRaw)
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		key, value, _ := strings.Cut(line, "=")
		got[key] = value
	}
	for key := range got {
		if !slices.Contains([]string{"PATH", "TMPDIR", WrapperHomeEnv, "PWD", "OLDPWD", "SHLVL", "_"}, key) {
			t.Fatalf("ambient variable %q must not reach the wrapper: %s", key, env)
		}
	}
	if got["PATH"] != wrapperPathEnv || !strings.Contains(got["TMPDIR"], "forgecell-analysis-wrapper-") || !strings.Contains(got[WrapperHomeEnv], "forgecell-analysis-wrapper-") {
		t.Fatalf("the wrapper gets a fixed minimal environment: %s", env)
	}
	raw, _ := os.ReadFile(stdin)
	var envelope struct {
		Protocol     string          `json:"protocol"`
		Instruction  string          `json:"instruction"`
		OutputSchema json.RawMessage `json:"outputSchema"`
		Request      map[string]any  `json:"request"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Protocol != AnalysisWrapperProtocol || envelope.Instruction != analysisInstruction || string(envelope.OutputSchema) != readiness.AnalysisSchema() || envelope.Request["kind"] != "ticket-analysis" || envelope.Request["humanVerification"] == nil {
		t.Fatalf("unexpected envelope %s (%v)", raw, err)
	}
}

func TestAnalysisWrapperRefusesMissingAndSelfAttestedEvidence(t *testing.T) {
	runManifestCases(t, []manifestCase{
		{"missing", func(m *AnalysisWrapperManifest) { m.Enforcement = nil }, "missing-evidence"},
		{"self-attested", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Origin = "self-attested" }, "self-attested"},
		{"wrapper-origin", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Origin = "wrapper" }, "self-attested"},
		{"no-review-basis", func(m *AnalysisWrapperManifest) { m.Enforcement[0].ReviewBasis = "" }, "missing-evidence"},
		{"no-limitations", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Limitations = "" }, "missing-evidence"},
		{"no-digest", func(m *AnalysisWrapperManifest) { m.Enforcement[0].SHA256 = "" }, "missing-evidence"},
		{"no-path", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Path = "" }, "missing-evidence"},
		{"relative-path", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Path = "review.txt" }, "missing-evidence"},
		{"digest-points-at-nothing", func(m *AnalysisWrapperManifest) {
			m.Enforcement[0].Path = filepath.Join(filepath.Dir(m.Enforcement[0].Path), "absent-review.txt")
			m.Enforcement[0].SHA256 = strings.Repeat("a", 64)
		}, "missing-evidence"},
		{"digest-is-not-the-file", func(m *AnalysisWrapperManifest) { m.Enforcement[0].SHA256 = strings.Repeat("a", 64) }, "changed"},
		{"unrecognized-origin", func(m *AnalysisWrapperManifest) { m.Enforcement[0].Origin = "vendor-blog" }, "unknown"},
	})
}

func TestAnalysisWrapperRefusesDeniedAndUnknownControls(t *testing.T) {
	runManifestCases(t, []manifestCase{
		{"denied-input", func(m *AnalysisWrapperManifest) { m.Controls.Inputs = []string{"stdin", "workspace"} }, "denied"},
		{"denied-scratch", func(m *AnalysisWrapperManifest) { m.Controls.ScratchWrites = "workspace" }, "denied"},
		{"denied-processes", func(m *AnalysisWrapperManifest) { m.Controls.Processes = "unrestricted" }, "denied"},
		{"denied-tools", func(m *AnalysisWrapperManifest) { m.Controls.Tools = "shell" }, "denied"},
		{"denied-network", func(m *AnalysisWrapperManifest) { m.Controls.Network = "unrestricted" }, "denied"},
		{"denied-credential", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"*"} }, "denied"},
		{"denied-declared-endpoints", func(m *AnalysisWrapperManifest) { m.Controls.Network = "declared-endpoints" }, "denied"},
		{"denied-credential-with-network-none", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"WRAPPER_TOKEN"} }, "denied"},
		{"denied-credential-HOME", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"HOME"} }, "denied"},
		{"denied-credential-PATH", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"PATH"} }, "denied"},
		{"denied-credential-TMPDIR", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"TMPDIR"} }, "denied"},
		{"denied-credential-LD_PRELOAD", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"LD_PRELOAD"} }, "denied"},
		{"denied-credential-DYLD", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"DYLD_INSERT_LIBRARIES"} }, "denied"},
		{"denied-credential-SSH", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"SSH_AUTH_SOCK"} }, "denied"},
		{"denied-credential-agent-socket", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = []string{"GPG_AGENT_SOCK"} }, "denied"},
		{"unknown-input", func(m *AnalysisWrapperManifest) { m.Controls.Inputs = nil }, "unknown"},
		{"unknown-scratch", func(m *AnalysisWrapperManifest) { m.Controls.ScratchWrites = "" }, "unknown"},
		{"unknown-tools", func(m *AnalysisWrapperManifest) { m.Controls.Tools = "unknown" }, "unknown"},
		{"unknown-network", func(m *AnalysisWrapperManifest) { m.Controls.Network = "" }, "unknown"},
		{"unknown-credential", func(m *AnalysisWrapperManifest) { m.Controls.Credentials = nil }, "unknown"},
		{"unknown-dependency", func(m *AnalysisWrapperManifest) {
			m.Dependencies = append(m.Dependencies, WrapperDependency{Path: "/usr/lib/mutable.so", Role: "enforcement-critical", State: "unknown"})
		}, "unknown"},
		{"unknown-identities", func(m *AnalysisWrapperManifest) { m.Policy = nil }, "unknown"},
		{"unknown-interpreter", func(m *AnalysisWrapperManifest) { m.Interpreter = nil }, "unknown"},
		{"mismatched-interpreter", func(m *AnalysisWrapperManifest) { m.Interpreter.Path = "/bin/bash" }, "denied"},
	})
	t.Run("path-resolved-interpreter", func(t *testing.T) {
		fx := newWrapperFixture(t, "true\n")
		if err := os.WriteFile(fx.script, []byte("#!/usr/bin/env sh\ntrue\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		fx.pin()
		assertRefused(t, fx.approve(), "unknown")
	})
}

func TestAnalysisWrapperRefusesChangedIdentitiesAndStaleApproval(t *testing.T) {
	changes := []struct {
		name   string
		change func(fx *wrapperFixture)
	}{
		{"executable", func(fx *wrapperFixture) { fx.setScript("printf '%s' '{\"summary\":\"swapped\"}'\n") }},
		{"configuration", func(fx *wrapperFixture) { os.WriteFile(fx.config, []byte(`{"mode":"write"}`), 0o600) }},
		{"policy", func(fx *wrapperFixture) { os.WriteFile(fx.policy, []byte(`{"deny":[]}`), 0o600) }},
		{"dependency", func(fx *wrapperFixture) { os.WriteFile(fx.dep, []byte("patched"), 0o600) }},
		{"evidence", func(fx *wrapperFixture) { os.WriteFile(fx.evidence, []byte("A different review."), 0o600) }},
	}
	for _, c := range changes {
		t.Run(c.name+"-before-approval", func(t *testing.T) {
			fx := newWrapperFixture(t, "printf '%s' '"+wrapperOKAnalysis+"'\n")
			rel, sum := fx.write()
			c.change(fx)
			assertRefused(t, ApproveAnalysisWrapper(fx.lab, rel, sum), "changed")
		})
		t.Run(c.name+"-after-approval", func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			fx := newWrapperFixture(t, fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", marker, wrapperOKAnalysis))
			d := fx.approve()
			if err := d.Err(); err != nil {
				t.Fatal(err)
			}
			c.change(fx)
			result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second)
			if result.OK || !strings.Contains(result.Error, "changed") {
				t.Fatalf("a changed identity must stop revalidation: %+v", result)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("wrapper process started after its identity changed")
			}
		})
	}
	t.Run("manifest", func(t *testing.T) {
		fx := newWrapperFixture(t, "printf '%s' '"+wrapperOKAnalysis+"'\n")
		rel, sum := fx.write()
		fx.manifest.Args = []string{"--write-everything"}
		fx.write()
		assertRefused(t, ApproveAnalysisWrapper(fx.lab, rel, sum), "changed")
	})
	t.Run("missing-and-bad-reference", func(t *testing.T) {
		fx := newWrapperFixture(t, "true\n")
		for _, rel := range []string{"analysis-wrappers/absent.json", "../outside.json", "/etc/passwd", ""} {
			assertRefused(t, ApproveAnalysisWrapper(fx.lab, rel, strings.Repeat("b", 64)), "missing-evidence")
		}
		assertRefused(t, ApproveAnalysisWrapper(fx.lab, "analysis-wrappers/wrapper.json", "not-a-digest"), "missing-evidence")
	})
}

func TestAnalysisWrapperTimeoutReconcilesDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-only")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	fx := newWrapperFixture(t, fmt.Sprintf("cat >/dev/null\nsleep 60 &\necho $! > %q\nwait\n", pidFile))
	d := fx.approve()
	if err := d.Err(); err != nil {
		t.Fatal(err)
	}
	result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 3*time.Second)
	if result.OK || !result.TimedOut {
		t.Fatalf("a timed-out wrapper must fail intake: %+v", result)
	}
	// Reconciled means the managed process group, including the sleeping
	// descendant, was observed gone; otherwise the uncertainty must be recorded.
	if !result.Reconciled && result.Uncertainty == "" {
		t.Fatalf("an unreconciled descendant must be recorded as uncertain: %+v", result)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("the descendant was never started: %v", err)
	}
	var pid int
	if _, err = fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 1 {
		t.Fatalf("unusable descendant pid %q", raw)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); proc.Signal(syscall.Signal(0)) == nil; time.Sleep(25 * time.Millisecond) {
		if time.Now().After(deadline) {
			proc.Kill()
			t.Fatalf("the timed-out descendant %d is still alive", pid)
		}
	}
}

func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink and permission placement checks are Unix-only")
	}
}

func TestAnalysisWrapperRefusesSymlinkedAndWritableIdentities(t *testing.T) {
	unixOnly(t)
	linkInPlace := func(path string) func(*wrapperFixture) {
		return func(fx *wrapperFixture) {
			real := path + ".real"
			if err := os.Rename(path, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name   string
		change func(*wrapperFixture)
		detail string
	}{
		{"symlinked-executable", func(fx *wrapperFixture) { linkInPlace(fx.script)(fx) }, "symlink"},
		{"symlinked-dependency", func(fx *wrapperFixture) { linkInPlace(fx.dep)(fx) }, "symlink"},
		{"symlinked-configuration", func(fx *wrapperFixture) { linkInPlace(fx.config)(fx) }, "symlink"},
		{"symlinked-evidence", func(fx *wrapperFixture) { linkInPlace(fx.evidence)(fx) }, "symlink"},
		{"symlinked-directory", func(fx *wrapperFixture) {
			link := filepath.Join(filepath.Dir(fx.dir), "linked")
			if err := os.Symlink(fx.dir, link); err != nil {
				t.Fatal(err)
			}
			fx.manifest.Executable.Path = filepath.Join(link, "wrapper.sh")
		}, "through a symlink"},
		{"group-writable-dependency", func(fx *wrapperFixture) { os.Chmod(fx.dep, 0o660) }, "group- or world-writable"},
		{"world-writable-executable", func(fx *wrapperFixture) { os.Chmod(fx.script, 0o707) }, "group- or world-writable"},
		{"world-writable-policy", func(fx *wrapperFixture) { os.Chmod(fx.policy, 0o666) }, "group- or world-writable"},
		{"writable-parent", func(fx *wrapperFixture) { os.Chmod(fx.dir, 0o777) }, "writable directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			fx := newWrapperFixture(t, fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", marker, wrapperOKAnalysis))
			tc.change(fx)
			d := fx.approve()
			class := "changed"
			if strings.Contains(tc.name, "evidence") {
				class = "missing-evidence"
			}
			assertRefused(t, d, class)
			if !strings.Contains(strings.Join(d.Findings, "\n"), tc.detail) {
				t.Fatalf("expected a finding mentioning %q: %v", tc.detail, d.Findings)
			}
			if result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second); result.OK {
				t.Fatalf("a refused decision must not run: %+v", result)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("wrapper process started despite a refusal")
			}
		})
	}
}

func TestAnalysisWrapperSwapBetweenApprovalAndExecution(t *testing.T) {
	unixOnly(t)
	t.Cleanup(func() { testHookAfterRevalidate, testHookAfterCopy = nil, nil })
	t.Run("swap-after-revalidation-is-caught-at-copy", func(t *testing.T) {
		out := t.TempDir()
		fx := newWrapperFixture(t, fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", filepath.Join(out, "approved-ran"), wrapperOKAnalysis))
		d := fx.approve()
		if err := d.Err(); err != nil {
			t.Fatal(err)
		}
		testHookAfterRevalidate = func() {
			fx.setScript(fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", filepath.Join(out, "evil-ran"), wrapperOKAnalysis))
		}
		t.Cleanup(func() { testHookAfterRevalidate = nil })
		result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second)
		if result.OK || !strings.Contains(result.Error, "changed after approval") {
			t.Fatalf("a swap between revalidation and copy must stop the run: %+v", result)
		}
		for _, name := range []string{"approved-ran", "evil-ran"} {
			if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
				t.Fatalf("no wrapper may run after a swap, but %s exists", name)
			}
		}
	})
	t.Run("swap-after-copy-cannot-change-what-runs", func(t *testing.T) {
		out := t.TempDir()
		seenConfig := filepath.Join(out, "config-seen")
		fx := newWrapperFixture(t, fmt.Sprintf("cat >/dev/null\ncp \"$%s/config/config.json\" %q\ntouch %q\nprintf '%%s' '%s'\n", WrapperHomeEnv, seenConfig, filepath.Join(out, "approved-ran"), wrapperOKAnalysis))
		d := fx.approve()
		if err := d.Err(); err != nil {
			t.Fatal(err)
		}
		testHookAfterCopy = func() {
			fx.setScript(fmt.Sprintf("touch %q\nprintf '%%s' '%s'\n", filepath.Join(out, "evil-ran"), wrapperOKAnalysis))
			if err := os.WriteFile(fx.config, []byte(`{"mode":"write"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { testHookAfterCopy = nil })
		result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second)
		if !result.OK {
			t.Fatalf("the verified copy must still run: %+v", result)
		}
		if _, err := os.Stat(filepath.Join(out, "approved-ran")); err != nil {
			t.Fatal("the approved bytes did not run")
		}
		if _, err := os.Stat(filepath.Join(out, "evil-ran")); !os.IsNotExist(err) {
			t.Fatal("the swapped original must never run")
		}
		if got, _ := os.ReadFile(seenConfig); string(got) != `{"mode":"analysis"}` {
			t.Fatalf("the wrapper must read the verified copy of its configuration, read %q", got)
		}
	})
}

func TestAnalysisWrapperStderrIsNeverReturnedOrPersisted(t *testing.T) {
	fx := newWrapperFixture(t, "cat >/dev/null\necho 'leaked-detail' >&2\nexit 4\n")
	d := fx.approve()
	result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second)
	if result.OK || result.Code != 4 || strings.Contains(result.Stderr, "leaked-detail") || !strings.Contains(result.Stderr, "stderr omitted") {
		t.Fatalf("stderr must be omitted from the result: %+v", result)
	}
	fx = newWrapperFixture(t, "cat >/dev/null\nhead -c 300000 /dev/zero | tr '\\0' e >&2\n")
	d = fx.approve()
	result = InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 10*time.Second)
	if result.OK || !result.Overflow || len(result.Stderr) > 100 {
		t.Fatalf("stderr overflow must fail the run without returning the stream: %d bytes %+v", len(result.Stderr), result)
	}
}

func TestAnalysisWrapperInterruptionAndProcessFailureFailClosed(t *testing.T) {
	fx := newWrapperFixture(t, "cat >/dev/null\nexit 3\n")
	d := fx.approve()
	if result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 5*time.Second); result.OK || result.Code != 3 {
		t.Fatalf("a failing wrapper must fail intake: %+v", result)
	}
	fx = newWrapperFixture(t, "cat >/dev/null\nsleep 30\n")
	d = fx.approve()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if result := InvokeAnalysisWrapper(ctx, fx.lab, d, analysisRequestFixture(), 20*time.Second); result.OK || !result.Interrupted {
		t.Fatalf("an interrupted wrapper must fail intake: %+v", result)
	}
}

func TestAnalysisWrapperOverflowAndMalformedOutput(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		overflow bool
		policy   bool
	}{
		{"overflow", "cat >/dev/null\nhead -c 300000 /dev/zero | tr '\\0' a\n", true, false},
		{"not-json", "cat >/dev/null\nprintf 'not json'\n", false, false},
		{"unknown-field", "cat >/dev/null\nprintf '%s' '{\"summary\":\"x\",\"approved\":true}'\n", false, false},
		{"duplicate-key", "cat >/dev/null\nprintf '%s' '{\"summary\":\"x\",\"summary\":\"y\"}'\n", false, false},
		{"no-summary", "cat >/dev/null\nprintf '%s' '{}'\n", false, false},
		{"independent-claim", "cat >/dev/null\nprintf '%s' '{\"summary\":\"x\",\"checks\":[{\"id\":\"c\",\"category\":\"independent-acceptance\"}]}'\n", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newWrapperFixture(t, tc.body)
			d := fx.approve()
			if err := d.Err(); err != nil {
				t.Fatal(err)
			}
			result := InvokeAnalysisWrapper(context.Background(), fx.lab, d, analysisRequestFixture(), 10*time.Second)
			if result.OK {
				t.Fatalf("invalid output must fail: %+v", result)
			}
			if tc.overflow {
				if !result.Overflow {
					t.Fatalf("output past the bound must be flagged: %+v", result)
				}
				return
			}
			if !strings.HasPrefix(result.Error, InvalidAnalysisPrefix) {
				t.Fatalf("expected an invalid-analysis failure: %+v", result)
			}
			if (result.Stdout != "") != tc.policy {
				t.Fatalf("only a policy rejection may keep its text for the corrective pass: %+v", result)
			}
		})
	}
}

func TestAnalysisWrapperManifestDecoderIsStrict(t *testing.T) {
	fx := newWrapperFixture(t, "true\n")
	good, _ := json.Marshal(fx.manifest)
	if _, err := DecodeAnalysisWrapperManifest(good); err != nil {
		t.Fatal(err)
	}
	text := string(good)
	bad := [][2]string{
		{"unknown-field", strings.Replace(text, `"kind"`, `"extra":1,"kind"`, 1)},
		{"duplicate-key", strings.Replace(text, `"kind":"analysis-wrapper"`, `"kind":"analysis-wrapper","kind":"analysis-wrapper"`, 1)},
		{"trailing", text + `{}`},
		{"oversize", text + strings.Repeat(" ", MaxAnalysisWrapperManifestBytes)},
		{"wrong-kind", strings.Replace(text, "analysis-wrapper", "coding", 1)},
		{"relative-exec", strings.Replace(text, fx.script, "wrapper.sh", 1)},
		{"short-digest", strings.Replace(text, fx.manifest.Executable.SHA256, "abc", 1)},
		{"empty-arg", strings.Replace(text, `"--analysis-only"`, `""`, 1)},
		{"not-an-object", `[]`},
	}
	repeated := fx.manifest
	repeated.Configuration = append(slices.Clone(repeated.Configuration), PinnedFile{Path: filepath.Join(fx.dir, "other", "config.json"), SHA256: fx.manifest.Configuration[0].SHA256})
	raw, _ := json.Marshal(repeated)
	bad = append(bad, [2]string{"duplicate-basename", string(raw)})
	for _, b := range bad {
		if _, err := DecodeAnalysisWrapperManifest([]byte(b[1])); err == nil {
			t.Errorf("%s: decoder accepted %.120s", b[0], b[1])
		}
	}
}

func TestAnalysisWrapperLeavesCustomCodingInvocationAndFingerprintUntouched(t *testing.T) {
	fx := newWrapperFixture(t, "true\n")
	before, err := Fingerprint([]string{fx.script})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fx.config, []byte(`{"mode":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint([]string{fx.script})
	if err != nil || before != after {
		t.Fatalf("the legacy fingerprint pins only argv0 and must stay unchanged by wrapper configuration: %v %s %s", err, before, after)
	}
	result := InvokeBinding(context.Background(), []string{fx.script}, t.TempDir(), time.Second, map[string]any{"kind": "ticket-analysis"})
	if result.OK || !strings.Contains(result.Error, "no verified read-only ticket-analysis protocol") {
		t.Fatalf("a custom binding without an approved wrapper must still refuse analysis: %+v", result)
	}
}
