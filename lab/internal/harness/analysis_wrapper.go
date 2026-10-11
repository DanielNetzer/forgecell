package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// AnalysisWrapperProtocol names the analysis-only stdin/stdout exchange: one JSON
// envelope in, one bounded readiness Analysis JSON document out.
const AnalysisWrapperProtocol = "forgecell-analysis-wrapper/v1"

const (
	MaxAnalysisWrapperManifestBytes = 64 * 1024
	maxWrapperListEntries           = 32
	maxWrapperText                  = 1024

	// wrapperPathEnv is the only PATH a wrapper ever receives.
	wrapperPathEnv = "/usr/bin:/bin"
	// WrapperHomeEnv names the private directory that holds the verified copies:
	// exec/, config/, policy/, dependencies/ and scratch/.
	WrapperHomeEnv = "FORGECELL_WRAPPER_HOME"
)

// Test seams between revalidation, copying and execution.
var testHookAfterRevalidate, testHookAfterCopy func()

// WrapperClaimNotice is stored with every decision so a reader cannot mistake
// declared controls for Forgecell-verified containment.
const WrapperClaimNotice = "Declared controls are wrapper claims. Forgecell verified only pin integrity, approval freshness, process outcome and bounded output. It did not verify external authenticity or containment, and it is not an OS sandbox."

var (
	hex64          = regexp.MustCompile(`^[a-f0-9]{64}$`)
	credentialName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// PinnedFile is one content-addressed identity.
type PinnedFile = pinned.File

// WrapperDependency is a reviewed enforcement-critical dependency. A mutable
// dependency nobody explained is declared with state "unknown" and is never
// treated as verified.
type WrapperDependency struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Note   string `json:"note,omitempty"`
}

// WrapperControls are the wrapper's own claims about its boundaries. A missing,
// empty or "unknown" value is uncertain; a value outside the read-only
// vocabulary is denied. Both refuse the run.
type WrapperControls struct {
	Inputs        []string `json:"inputs"`
	ScratchWrites string   `json:"scratchWrites"`
	Processes     string   `json:"processes"`
	Tools         string   `json:"tools"`
	Network       string   `json:"network"`
	Credentials   []string `json:"credentials"`
}

// WrapperEvidence is a bounded reference to an external review of the
// enforcement: a pinned file whose SHA-256 Forgecell recomputes at every
// verification. Forgecell records it; it does not authenticate its author or
// judge what it says.
type WrapperEvidence struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Origin      string `json:"origin"`
	Limitations string `json:"limitations"`
	ReviewBasis string `json:"reviewBasis"`
}

// AnalysisWrapperManifest is the content-addressed contract a human approves
// through the exact Formula bytes that pin its digest.
type AnalysisWrapperManifest struct {
	SchemaVersion string              `json:"schemaVersion"`
	Kind          string              `json:"kind"`
	Executable    PinnedFile          `json:"executable"`
	Args          []string            `json:"args"`
	Configuration []PinnedFile        `json:"configuration"`
	Interpreter   *PinnedFile         `json:"interpreter,omitempty"`
	Policy        []PinnedFile        `json:"policy"`
	Dependencies  []WrapperDependency `json:"dependencies"`
	Controls      WrapperControls     `json:"controls"`
	Enforcement   []WrapperEvidence   `json:"enforcement"`
}

// VerifiedIdentity is a digest Forgecell computed from the file itself at
// verification time, never one copied from the manifest.
type VerifiedIdentity = pinned.Identity

// AnalysisWrapperDecision is the immutable evidence of one approval attempt.
type AnalysisWrapperDecision struct {
	SchemaVersion  string                   `json:"schemaVersion"`
	Status         string                   `json:"status"`
	Reason         string                   `json:"reason,omitempty"`
	Findings       []string                 `json:"findings,omitempty"`
	ManifestPath   string                   `json:"manifestPath"`
	ManifestSHA256 string                   `json:"manifestSha256"`
	Manifest       *AnalysisWrapperManifest `json:"manifest,omitempty"`
	Identities     []VerifiedIdentity       `json:"identities,omitempty"`
	Relaxations    []string                 `json:"relaxations,omitempty"`
	Notice         string                   `json:"notice"`
	Digest         string                   `json:"digest"`
}

// Err is nil only for an approved decision.
func (d AnalysisWrapperDecision) Err() error {
	if d.Status == "approved" && d.Manifest != nil {
		return nil
	}
	return fmt.Errorf("analysis wrapper refused: %s; no analysis process was started, no coding harness was invoked and no fallback was used", d.Reason)
}

func decisionDigest(d AnalysisWrapperDecision) string {
	d.Digest = ""
	raw, _ := json.Marshal(d)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DecodeAnalysisWrapperManifest is strict: bounded size, one JSON object, no
// duplicate keys and no unknown fields.
func DecodeAnalysisWrapperManifest(raw []byte) (AnalysisWrapperManifest, error) {
	var m AnalysisWrapperManifest
	if len(raw) > MaxAnalysisWrapperManifestBytes {
		return m, fmt.Errorf("analysis wrapper manifest exceeds %d bytes", MaxAnalysisWrapperManifestBytes)
	}
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		var dup *pinned.DuplicateKeyError
		if errors.As(err, &dup) {
			err = fmt.Errorf("analysis wrapper manifest has a duplicate key %q", dup.Key)
		}
		return m, fmt.Errorf("invalid analysis wrapper manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return AnalysisWrapperManifest{}, fmt.Errorf("invalid analysis wrapper manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return AnalysisWrapperManifest{}, fmt.Errorf("analysis wrapper manifest must be one JSON object")
	}
	if err := m.validate(); err != nil {
		return AnalysisWrapperManifest{}, err
	}
	return m, nil
}

func validWrapperPath(p string) bool {
	return p != "" && len(p) <= 4096 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsRune(p, 0)
}

func (m AnalysisWrapperManifest) validate() error {
	if m.SchemaVersion != "v1" || m.Kind != "analysis-wrapper" {
		return fmt.Errorf("analysis wrapper manifest must be schemaVersion v1, kind analysis-wrapper")
	}
	checkPin := func(label string, f PinnedFile) error {
		if !validWrapperPath(f.Path) || !hex64.MatchString(f.SHA256) {
			return fmt.Errorf("%s needs a clean absolute path and a lowercase SHA-256", label)
		}
		return nil
	}
	if err := checkPin("executable", m.Executable); err != nil {
		return err
	}
	if len(m.Args) > maxWrapperListEntries {
		return fmt.Errorf("analysis wrapper argv exceeds %d entries", maxWrapperListEntries)
	}
	for _, a := range m.Args {
		if a == "" || len(a) > 4096 || strings.ContainsRune(a, 0) {
			return fmt.Errorf("analysis wrapper argv must contain bounded non-empty strings")
		}
	}
	if len(m.Configuration) > maxWrapperListEntries || len(m.Policy) > maxWrapperListEntries || len(m.Dependencies) > maxWrapperListEntries || len(m.Enforcement) > maxWrapperListEntries {
		return fmt.Errorf("analysis wrapper lists exceed %d entries", maxWrapperListEntries)
	}
	if m.Interpreter != nil {
		if err := checkPin("interpreter", *m.Interpreter); err != nil {
			return err
		}
	}
	for _, f := range m.Configuration {
		if err := checkPin("configuration entry", f); err != nil {
			return err
		}
	}
	for _, f := range m.Policy {
		if err := checkPin("policy entry", f); err != nil {
			return err
		}
	}
	for _, d := range m.Dependencies {
		if !validWrapperPath(d.Path) || strings.TrimSpace(d.Role) == "" || len(d.Role) > maxWrapperText || len(d.Note) > maxWrapperText {
			return fmt.Errorf("dependency needs a clean absolute path and a role")
		}
		if d.State == "pinned" && !hex64.MatchString(d.SHA256) {
			return fmt.Errorf("a pinned dependency needs a lowercase SHA-256")
		}
		if d.State == "unknown" && d.SHA256 != "" {
			return fmt.Errorf("an unknown dependency cannot carry a digest")
		}
	}
	if len(m.Controls.Inputs) > maxWrapperListEntries || len(m.Controls.Credentials) > maxWrapperListEntries {
		return fmt.Errorf("analysis wrapper control lists exceed %d entries", maxWrapperListEntries)
	}
	for _, e := range m.Enforcement {
		if len(e.ID) > maxWrapperText || len(e.Path) > 4096 || len(e.Origin) > maxWrapperText || len(e.Limitations) > maxWrapperText || len(e.ReviewBasis) > maxWrapperText {
			return fmt.Errorf("enforcement evidence fields exceed %d bytes", maxWrapperText)
		}
	}
	// Verified copies keep their basename inside one directory per role.
	unique := func(role string, paths []string) error {
		seen := map[string]bool{}
		for _, p := range paths {
			if seen[filepath.Base(p)] {
				return fmt.Errorf("analysis wrapper %s files must have distinct names, %q repeats", role, filepath.Base(p))
			}
			seen[filepath.Base(p)] = true
		}
		return nil
	}
	var config, policy, deps []string
	for _, f := range m.Configuration {
		config = append(config, f.Path)
	}
	for _, f := range m.Policy {
		policy = append(policy, f.Path)
	}
	for _, d := range m.Dependencies {
		if d.State == "pinned" {
			deps = append(deps, d.Path)
		}
	}
	for role, paths := range map[string][]string{"configuration": config, "policy": policy, "dependency": deps} {
		if err := unique(role, paths); err != nil {
			return err
		}
	}
	return nil
}

// deniedCredentialName reports names that would replace the fixed wrapper
// environment, inject loaders or expose an agent socket.
func deniedCredentialName(name string) bool {
	switch name {
	case "HOME", "PATH", "TMPDIR", "TMP", "TEMP", "SHELL", "PWD", "IFS", "ENV", "BASH_ENV", "USER", "LOGNAME":
		return true
	}
	for _, prefix := range []string{"LD_", "DYLD_", "SSH_", "GPG_", "FORGECELL_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return strings.HasSuffix(name, "_SOCK") || strings.HasSuffix(name, "_SOCKET")
}

// relaxations names the declared controls that are weaker than strict
// read-only analysis, so the decision never reads as stronger than it is.
func (m AnalysisWrapperManifest) relaxations() []string {
	var out []string
	if m.Controls.ScratchWrites == "temporary-directory" {
		out = append(out, "scratchWrites=temporary-directory is a relaxation: the wrapper claims writes only inside its private temporary directory; Forgecell removes that directory afterwards and does not confine the writes.")
	}
	if m.Controls.Processes == "wrapper-managed" {
		out = append(out, "processes=wrapper-managed is a relaxation: the wrapper claims to manage any helper processes it starts; Forgecell reconciles the process group after the run and does not prevent or inspect them.")
	}
	return out
}

// Assess classifies the declared controls and evidence without touching the
// filesystem. Every finding refuses the run: denied (outside the read-only
// vocabulary), unknown (uncertain), missing-evidence or self-attested.
func (m AnalysisWrapperManifest) Assess() []string {
	var findings []string
	control := func(name, value string, allowed ...string) {
		switch v := strings.TrimSpace(value); {
		case v == "" || v == "unknown":
			findings = append(findings, "unknown: control "+name+" is not declared")
		case !slices.Contains(allowed, v):
			findings = append(findings, fmt.Sprintf("denied: control %s=%q is outside the read-only vocabulary", name, v))
		}
	}
	switch {
	case m.Controls.Inputs == nil:
		findings = append(findings, "unknown: control inputs is not declared")
	case !slices.Equal(m.Controls.Inputs, []string{"stdin"}):
		findings = append(findings, fmt.Sprintf("denied: control inputs=%v must be exactly [stdin]", m.Controls.Inputs))
	}
	control("scratchWrites", m.Controls.ScratchWrites, "none", "temporary-directory")
	control("processes", m.Controls.Processes, "none", "wrapper-managed")
	control("tools", m.Controls.Tools, "none")
	control("network", m.Controls.Network, "none")
	if m.Controls.Credentials == nil {
		findings = append(findings, "unknown: control credentials is not declared")
	}
	for _, name := range m.Controls.Credentials {
		switch {
		case !credentialName.MatchString(name):
			findings = append(findings, fmt.Sprintf("denied: credential %q is not an exact environment variable name", name))
		case deniedCredentialName(name):
			findings = append(findings, fmt.Sprintf("denied: credential %q would override the fixed wrapper environment, inject a loader or expose an agent socket", name))
		}
	}
	if len(m.Controls.Credentials) > 0 && strings.TrimSpace(m.Controls.Network) == "none" {
		findings = append(findings, "denied: credentials are declared while network is none; the claims are contradictory and the wrapper is never given credentials")
	}
	if m.Configuration == nil || m.Policy == nil || m.Dependencies == nil {
		findings = append(findings, "unknown: configuration, policy and dependency identities must be declared, even when empty")
	}
	for _, d := range m.Dependencies {
		if d.State != "pinned" {
			findings = append(findings, fmt.Sprintf("unknown: dependency %s is mutable or unexplained (state %q)", d.Path, d.State))
		}
	}
	if len(m.Enforcement) == 0 {
		findings = append(findings, "missing-evidence: no externally reviewed enforcement evidence is referenced")
	}
	for _, e := range m.Enforcement {
		switch {
		case e.Origin == "self-attested" || e.Origin == "wrapper" || e.Origin == "self":
			findings = append(findings, fmt.Sprintf("self-attested: enforcement evidence %q originates from the wrapper itself", e.ID))
		case e.Origin != "external-review":
			findings = append(findings, fmt.Sprintf("unknown: enforcement evidence %q has unrecognized origin %q", e.ID, e.Origin))
		case strings.TrimSpace(e.ID) == "" || !validWrapperPath(e.Path) || !hex64.MatchString(e.SHA256) || strings.TrimSpace(e.Limitations) == "" || strings.TrimSpace(e.ReviewBasis) == "":
			findings = append(findings, fmt.Sprintf("missing-evidence: enforcement evidence %q needs an id, a pinned file path and SHA-256, limitations and review basis", e.ID))
		}
	}
	return findings
}

// verifyIdentities hashes every pinned file now and returns the digests it
// computed. A script's shebang interpreter must be declared and pinned; a
// PATH-resolved one is unknown. Evidence that points at nothing is refused.
func (m AnalysisWrapperManifest) verifyIdentities() ([]string, []VerifiedIdentity) {
	var findings []string
	var identities []VerifiedIdentity
	check := func(role string, f PinnedFile, unavailable string) {
		if id, finding := pinned.Verify(role, f, unavailable); finding != "" {
			findings = append(findings, finding)
		} else {
			identities = append(identities, id)
		}
	}
	check("executable", m.Executable, "changed")
	if info, err := os.Stat(m.Executable.Path); err == nil && info.Mode().Perm()&0o100 == 0 {
		findings = append(findings, "changed: executable "+m.Executable.Path+" is not executable by its owner")
	}
	if m.Interpreter != nil {
		check("interpreter", *m.Interpreter, "changed")
	}
	for _, f := range m.Configuration {
		check("configuration", f, "changed")
	}
	for _, f := range m.Policy {
		check("policy", f, "changed")
	}
	for _, d := range m.Dependencies {
		if d.State == "pinned" {
			check("dependency", PinnedFile{Path: d.Path, SHA256: d.SHA256}, "changed")
		}
	}
	for _, e := range m.Enforcement {
		if validWrapperPath(e.Path) && hex64.MatchString(e.SHA256) {
			check("enforcement evidence "+e.ID, PinnedFile{Path: e.Path, SHA256: e.SHA256}, "missing-evidence")
		}
	}
	head := make([]byte, 256)
	if file, err := os.Open(m.Executable.Path); err == nil {
		n, _ := io.ReadFull(file, head)
		file.Close()
		head = head[:n]
		if bytes.HasPrefix(head, []byte("#!")) {
			line, _, _ := strings.Cut(string(head[2:]), "\n")
			fields := strings.Fields(line)
			switch {
			case len(fields) == 0:
				findings = append(findings, "unknown: script interpreter line is empty")
			case filepath.Base(fields[0]) == "env":
				findings = append(findings, "unknown: script interpreter is resolved through PATH by env")
			case m.Interpreter == nil:
				findings = append(findings, "unknown: script interpreter "+fields[0]+" is not declared and pinned")
			case m.Interpreter.Path != fields[0]:
				findings = append(findings, fmt.Sprintf("denied: script interpreter %s differs from the declared %s", fields[0], m.Interpreter.Path))
			}
		}
	}
	return findings, identities
}

// ApproveAnalysisWrapper reads the Lab-relative manifest, checks it against the
// digest pinned by the approved Formula, assesses the declared controls and
// re-hashes every pinned identity. It never starts a process. The decision is
// always returned so a refusal can be persisted as evidence.
func ApproveAnalysisWrapper(labDir, manifestPath, pinnedSHA256 string) AnalysisWrapperDecision {
	d := AnalysisWrapperDecision{SchemaVersion: "v1", Status: "refused", ManifestPath: manifestPath, ManifestSHA256: pinnedSHA256, Notice: WrapperClaimNotice}
	refuse := func(reason string, findings ...string) AnalysisWrapperDecision {
		d.Reason = reason
		d.Findings = append(d.Findings, findings...)
		d.Digest = decisionDigest(d)
		return d
	}
	if !filepath.IsLocal(filepath.FromSlash(manifestPath)) || !hex64.MatchString(pinnedSHA256) {
		return refuse("manifest reference is not a Lab-relative path with a pinned SHA-256", "missing-evidence: manifest reference is incomplete")
	}
	file := filepath.Join(labDir, filepath.FromSlash(manifestPath))
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxAnalysisWrapperManifestBytes {
		return refuse("manifest is missing or is not a bounded regular file", "missing-evidence: manifest "+manifestPath+" is unavailable")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return refuse("manifest is unreadable", "missing-evidence: "+err.Error())
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != pinnedSHA256 {
		return refuse("manifest changed since the Formula approval pinned it", fmt.Sprintf("changed: manifest SHA-256 %s differs from the pinned %s", got, pinnedSHA256))
	}
	m, err := DecodeAnalysisWrapperManifest(raw)
	if err != nil {
		return refuse(err.Error(), "malformed: "+err.Error())
	}
	d.Manifest = &m
	d.Relaxations = m.relaxations()
	identityFindings, identities := m.verifyIdentities()
	d.Identities = identities
	findings := append(m.Assess(), identityFindings...)
	if len(findings) > 0 {
		return refuse("declared controls, evidence or pinned identities are not acceptable: "+findings[0], findings...)
	}
	d.Status = "approved"
	d.Digest = decisionDigest(d)
	return d
}

type wrapperEnvelope struct {
	Protocol     string          `json:"protocol"`
	Instruction  string          `json:"instruction"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Request      map[string]any  `json:"request"`
}

// materializeWrapper copies the verified executable, configuration, policy and
// pinned dependencies into root and returns the copied executable. Each copy is
// made from the handle that was hash-checked and is hashed again after writing,
// so the process runs the bytes that were approved, not whatever the original
// path holds by then. The interpreter is the one identity that cannot be
// copied; it is verified in place under the same placement rules.
func materializeWrapper(root string, m AnalysisWrapperManifest) (string, error) {
	for _, sub := range []string{"exec", "config", "policy", "dependencies", "scratch"} {
		if err := os.Mkdir(filepath.Join(root, sub), 0o700); err != nil {
			return "", err
		}
	}
	copyTo := func(sub string, f PinnedFile) (string, error) {
		dst := filepath.Join(root, sub, filepath.Base(f.Path))
		return dst, pinned.Copy(f, dst)
	}
	executable, err := copyTo("exec", m.Executable)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(executable); err != nil || info.Mode().Perm() != 0o500 {
		return "", fmt.Errorf("the private copy of %s is not owner-executable", m.Executable.Path)
	}
	for _, group := range []struct {
		sub   string
		files []PinnedFile
	}{{"config", m.Configuration}, {"policy", m.Policy}} {
		for _, f := range group.files {
			if _, err = copyTo(group.sub, f); err != nil {
				return "", err
			}
		}
	}
	for _, dep := range m.Dependencies {
		if dep.State == "pinned" {
			if _, err = copyTo("dependencies", PinnedFile{Path: dep.Path, SHA256: dep.SHA256}); err != nil {
				return "", err
			}
		}
	}
	return executable, nil
}

// InvokeAnalysisWrapper revalidates the decision against the files as they are
// now, copies the verified bytes into a private temporary directory and runs
// only the copied executable with the approved argv. The environment is fixed
// to a minimal PATH, TMPDIR and WrapperHomeEnv; no ambient or declared
// credential is forwarded. Output is bounded and decoded exactly like a native
// analysis, wrapper stderr is never returned, and any failure leaves OK false so
// intake stops.
func InvokeAnalysisWrapper(ctx context.Context, labDir string, d AnalysisWrapperDecision, request map[string]any, timeout time.Duration) process.Result {
	fail := func(message string) process.Result { return process.Result{Code: -1, Reconciled: true, Error: message} }
	if err := d.Err(); err != nil {
		return fail(err.Error())
	}
	current := ApproveAnalysisWrapper(labDir, d.ManifestPath, d.ManifestSHA256)
	if current.Status != "approved" || current.Digest != d.Digest {
		return fail("analysis wrapper changed after its approval decision: " + current.Reason)
	}
	if testHookAfterRevalidate != nil {
		testHookAfterRevalidate()
	}
	m := *d.Manifest
	if request["kind"] != "ticket-analysis" {
		return fail("an analysis wrapper only accepts ticket-analysis requests")
	}
	root, err := os.MkdirTemp("", "forgecell-analysis-wrapper-")
	if err != nil {
		return fail(err.Error())
	}
	defer os.RemoveAll(root)
	executable, err := materializeWrapper(root, m)
	if err != nil {
		return fail("analysis wrapper could not be materialized from its verified bytes: " + err.Error())
	}
	if testHookAfterCopy != nil {
		testHookAfterCopy()
	}
	scratch := filepath.Join(root, "scratch")
	env := []string{"PATH=" + wrapperPathEnv, "TMPDIR=" + scratch, WrapperHomeEnv + "=" + root}
	input, err := json.Marshal(wrapperEnvelope{Protocol: AnalysisWrapperProtocol, Instruction: analysisInstruction, OutputSchema: json.RawMessage(readiness.AnalysisSchema()), Request: request})
	if err != nil {
		return fail(err.Error())
	}
	opts := process.Options{Argv: append([]string{executable}, m.Args...), Dir: scratch, Env: env, Stdin: append(input, '\n'), Timeout: timeout, MaxOutputBytes: readiness.MaxAnalysisBytes}
	var result process.Result
	// A just-written executable can be briefly "text file busy" while a
	// concurrent fork elsewhere in this process still holds its write handle.
	for attempt := 0; attempt < 5; attempt++ {
		if result = process.Run(ctx, opts); result.Code != -1 || !strings.Contains(result.Error, "text file busy") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(result.Stderr); n > 0 {
		result.Stderr = fmt.Sprintf("[wrapper stderr omitted: %d bytes]", n)
	}
	if !result.OK {
		return result
	}
	if _, err = readiness.DecodeAnalysis([]byte(result.Stdout)); err != nil {
		result.OK = false
		result.Error = InvalidAnalysisPrefix + err.Error()
		if !readiness.IsPolicyViolation(err) {
			result.Stdout, result.RawStdout = "", ""
		}
	}
	return result
}
