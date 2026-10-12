package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func extensionRecordFixture() molecule.Record {
	return molecule.Record{ID: "mol-1", Extensions: []molecule.ExtensionRecord{
		{Name: "graphify", Kind: "mcp-provider", ManifestSHA256: strings.Repeat("ab", 32), Network: "none", Roles: []string{"analysis", "coding"},
			Tools: map[string][]molecule.ExtensionTool{
				"analysis": {{Name: "query_graph", Access: "read-only"}},
				"coding":   {{Name: "query_graph", Access: "read-only"}, {Name: "annotate", Access: "read-write"}}},
			Cache: molecule.ExtensionCache{Key: "k", Outcome: "built", Code: 0, ElapsedMS: 12}},
		{Name: "broken", Kind: "mcp-provider", Network: "declared", Roles: []string{}, Skipped: map[string]string{"coding": "the cache is unavailable: prepare exited with code 1"},
			Cache: molecule.ExtensionCache{Key: "k2", Outcome: "unavailable", Code: 1, Reason: "prepare exited with code 1"}},
	}}
}

func TestScopeGateShowsExtensions(t *testing.T) {
	r := extensionRecordFixture()
	r.Readiness = &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{}, Digest: "d"}}}
	var out bytes.Buffer
	presentRun(&out, r)
	for _, want := range []string{
		"Extension graphify (mcp-provider; manifest SHA-256 " + strings.Repeat("ab", 32) + "): attached to analysis, coding",
		"analysis tools: query_graph (read-only)",
		"coding tools: query_graph (read-only), annotate (read-write)",
		"cache: built",
		"network claim (declared by the manifest, not enforced by Forgecell): none",
		"Extension broken",
		"coding not attached: the cache is unavailable: prepare exited with code 1",
		"cache: unavailable (prepare exit 1",
		molecule.AnalysisWithoutSafeModeDisclosure,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// A server passed only to coding makes no claim about analysis.
	r.Extensions = r.Extensions[1:]
	r.Extensions[0].Roles = []string{"coding"}
	out.Reset()
	presentRun(&out, r)
	if strings.Contains(out.String(), "without safe mode") {
		t.Fatalf("the disclosure appears although no server reached analysis:\n%s", out.String())
	}
	// The same gate for a blocked intake with no plan still shows the extensions.
	r.Readiness = nil
	out.Reset()
	presentRun(&out, r)
	if !strings.Contains(out.String(), "Extension broken") {
		t.Fatalf("missing extension for a record without a plan:\n%s", out.String())
	}
}

func TestExtensionFreeGateOutputIsUnchanged(t *testing.T) {
	r := molecule.Record{ID: "mol-1", Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{}, Digest: "d"}}}}
	var out bytes.Buffer
	presentRun(&out, r)
	if strings.Contains(out.String(), "Extension") || strings.Contains(out.String(), "safe mode") {
		t.Fatalf("an extension-free gate gained text:\n%s", out.String())
	}
}

const doctorRecipe = "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nEXTENSIONS" +
	"harness:\n  binding: custom\n  command: [\"/bin/harness\"]\n  extensions: [fake]\n" +
	"harnesses:\n  analysis: {binding: claude-code, command: [\"/x/forgecell\", \"__adapter\", \"claude-code\", \"/x/claude\"], extensions: [fake]}\n" +
	"atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"

type doctorEnv struct {
	lab, marker, digest string
	manifestRaw         []byte
}

func newDoctorEnv(t *testing.T) *doctorEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &doctorEnv{lab: filepath.Join(root, "lab"), marker: filepath.Join(root, "ran")}
	if err = os.MkdirAll(filepath.Join(e.lab, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	pin := func(name string) pinned.File {
		path := filepath.Join(root, name, "file")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// Running either script would leave the marker.
		body := "#!/bin/sh\necho ran > " + e.marker + "\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		return pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	m := extension.Manifest{SchemaVersion: "v1", Kind: "mcp-provider", Name: "fake",
		Server:       extension.Command{Executable: pin("server"), Args: []string{}},
		Prepare:      &extension.Prepare{Command: extension.Command{Executable: pin("prepare"), Args: []string{"{{CACHE}}"}}, TimeoutMS: 1000, Network: "none"},
		Tools:        []extension.Tool{{Name: "query", Access: "read-only"}},
		Roles:        []string{"analysis", "coding"},
		Dependencies: []extension.Dependency{},
		Limitations:  "test"}
	if e.manifestRaw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.lab, "manifests", "fake.json"), e.manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(e.manifestRaw)
	e.digest = hex.EncodeToString(sum[:])
	return e
}

func (e *doctorEnv) formula(t *testing.T, digest string) formula.Formula {
	t.Helper()
	f, err := formula.Parse([]byte(strings.Replace(doctorRecipe, "EXTENSIONS", "extensions:\n  fake: {manifest: manifests/fake.json, sha256: "+digest+", required: true}\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDoctorReportsExtensionProblems(t *testing.T) {
	e := newDoctorEnv(t)
	good := inspectExtensions(e.lab, e.formula(t, e.digest))
	if len(good) != 1 || good[0].Status != "verified" || good[0].Detail != "" || good[0].FixCommand != "" {
		t.Fatalf("%+v", good)
	}
	if len(good[0].Roles) != 2 || good[0].Roles[0].Role != "analysis" || good[0].Roles[0].Adapter.State != "supported" || good[0].Roles[1].Role != "coding" || good[0].Roles[1].Adapter.State == "supported" {
		t.Fatalf("adapter support per binding: %+v", good[0].Roles)
	}
	if _, err := os.Stat(e.marker); !os.IsNotExist(err) {
		t.Fatal("doctor ran prepare or the server")
	}
	if _, err := os.Stat(filepath.Join(e.lab, "extensions")); !os.IsNotExist(err) {
		t.Fatal("doctor created the extensions cache directory")
	}

	bad := inspectExtensions(e.lab, e.formula(t, strings.Repeat("f", 64)))
	if len(bad) != 1 || bad[0].Status != "problem" || !strings.Contains(bad[0].Detail, "changed") || !strings.HasPrefix(bad[0].FixCommand, "shasum -a 256 ") || !strings.Contains(bad[0].FixCommand, "manifests/fake.json") {
		t.Fatalf("a digest mismatch needs one fix command: %+v", bad)
	}

	var out bytes.Buffer
	renderDoctor(&out, doctorView{Lab: e.lab, Note: "note", Extensions: append(good, bad...)})
	for _, want := range []string{"Extensions (", "fake (manifests/fake.json, required): verified", "fake (manifests/fake.json, required): problem", "Fix: shasum -a 256 ", "analysis role (claude-code binding): adapter support supported", "coding role (custom binding): adapter support unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderDoctor(&out, doctorView{Lab: e.lab, Note: "note"})
	if strings.Contains(out.String(), "Extensions") {
		t.Fatalf("a Formula without extensions gained a section:\n%s", out.String())
	}
	if empty := inspectExtensions(e.lab, formula.Formula{}); len(empty) != 0 {
		t.Fatalf("%+v", empty)
	}
}

func TestExtensionDoctorAnalysisRoleMakesNoSafeModeClaim(t *testing.T) {
	e := newDoctorEnv(t)
	f := e.formula(t, e.digest)
	usual := "Ticket analysis can be attempted with verified controls; model access and end-to-end workflow remain unverified."
	role := onboarding.RoleReadiness{Role: "analysis", Capability: harness.Capability{State: "conditional", Reason: usual}}
	describeAnalysisWithExtensions(&role, f)
	if !strings.Contains(role.Capability.Reason, "without safe mode") || strings.Contains(role.Capability.Reason, "verified controls") {
		t.Fatalf("%q", role.Capability.Reason)
	}
	for name, unchanged := range map[string]onboarding.RoleReadiness{
		"coding role":     {Role: "coding", Capability: harness.Capability{State: "conditional", Reason: usual}},
		"blocked":         {Role: "analysis", Capability: harness.Capability{State: "blocked", Reason: "not logged in"}},
		"no extensions":   {Role: "analysis", Capability: harness.Capability{State: "conditional", Reason: usual}},
		"wrapped binding": {Role: "analysis", Capability: harness.Capability{State: "conditional", Reason: usual}},
	} {
		g := f
		switch name {
		case "no extensions":
			g.Harnesses = map[string]formula.Binding{"analysis": {Binding: "claude-code", Command: f.Harnesses["analysis"].Command}}
		case "wrapped binding":
			b := f.Harnesses["analysis"]
			b.AnalysisWrapper = &formula.AnalysisWrapperRef{Manifest: "w.json", SHA256: strings.Repeat("a", 64)}
			g.Harnesses = map[string]formula.Binding{"analysis": b}
		}
		before := unchanged.Capability
		describeAnalysisWithExtensions(&unchanged, g)
		if unchanged.Capability != before {
			t.Fatalf("%s: description changed to %q", name, unchanged.Capability.Reason)
		}
	}
}
