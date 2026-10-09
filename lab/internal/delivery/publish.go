package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Receipt is a separate durable delivery record. The original execution ledger is immutable.
type LifecycleObservation struct {
	State          string `json:"state"`
	ObservedAt     string `json:"observedAt"`
	Reconciliation string `json:"reconciliation,omitempty"`
}

type Receipt struct {
	// Persisted before create; an absent response never authorizes another attempt.
	CreationAttemptedAt          string                  `json:"creationAttemptedAt,omitempty"`
	Lifecycle                    *LifecycleObservation   `json:"lifecycle,omitempty"`
	DraftAtPublication           bool                    `json:"draftAtPublication,omitempty"`
	Policy                       *readiness.RemotePolicy `json:"publicationPolicy,omitempty"`
	AcceptanceReviewAcknowledged bool                    `json:"acceptanceReviewAcknowledged"`
	Preview                      PreviewResult           `json:"preview"`
	Title                        string                  `json:"title"`
	State                        string                  `json:"state"`
	Commit                       string                  `json:"commit,omitempty"`
	URL                          string                  `json:"url,omitempty"`
}
type PublishOptions struct {
	Options
	Approve, Title   string
	AcceptUnverified bool
	Runner           func(context.Context, process.Options) process.Result // Remote operations only.
}

func digest(p PreviewResult) string {
	p.Digest = ""
	p.Diff = ""
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func writeReceipt(o Options, r Receipt) error {
	dir := filepath.Join(o.LabDir, "deliveries")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".delivery-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, o.MoleculeID+".json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func verifyCommit(ctx context.Context, path string, r Receipt, head string) error {
	parents, err := git(ctx, path, nil, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return err
	}
	if strings.TrimSpace(parents) != head+" "+r.Preview.BaseCommit {
		return fmt.Errorf("delivery commit does not have the reviewed parent")
	}
	tree, err := git(ctx, path, nil, "rev-parse", head+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != r.Preview.Tree {
		return fmt.Errorf("delivery commit differs from reviewed tree; no push performed")
	}
	message, err := git(ctx, path, nil, "show", "-s", "--format=%B", head)
	if err != nil || strings.TrimSpace(message) != r.Title {
		return fmt.Errorf("delivery commit message differs from recorded intent")
	}
	return nil
}
func Publish(ctx context.Context, o PublishOptions) (r Receipt, err error) {
	absolute, err := filepath.Abs(o.LabDir)
	if err != nil {
		return r, err
	}
	o.LabDir = absolute
	if !safeID.MatchString(o.MoleculeID) || len(o.Approve) != 64 || strings.TrimSpace(o.Title) != o.Title || o.Title == "" || strings.ContainsAny(o.Title, "\r\n\x00") {
		return r, fmt.Errorf("provide an exact preview digest and one-line title")
	}
	if err = os.MkdirAll(o.LabDir, 0700); err != nil {
		return r, err
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return r, fmt.Errorf("delivery lock exists; verify no delivery process is active before recovering it: %w", err)
	}
	defer os.Remove(lock)
	file := filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json")
	raw, readErr := os.ReadFile(file)
	if readErr != nil && !os.IsNotExist(readErr) {
		return r, readErr
	}
	if readErr == nil {
		if err = json.Unmarshal(raw, &r); err != nil {
			return r, err
		}
		files := append([]string(nil), o.Files...)
		sort.Strings(files)
		files = unique(files)
		if r.Preview.Digest != o.Approve || digest(r.Preview) != o.Approve || r.Preview.MoleculeID != o.MoleculeID || r.Preview.BaseBranch != o.BaseBranch || !reflect.DeepEqual(files, r.Preview.Files) || r.Title != o.Title {
			return r, fmt.Errorf("resume must match the recorded review content, title and file scope")
		}
		if r.State != "prepared" && r.State != "committed" && r.State != "published" {
			return r, fmt.Errorf("invalid delivery state")
		}
	} else {
		r.Preview, err = Preview(ctx, o.Options)
		if err != nil {
			return r, err
		}
		if r.Preview.Digest != o.Approve {
			return r, fmt.Errorf("approval is stale: reviewed content changed")
		}
		if r.Preview.AcceptanceUnverified && !o.AcceptUnverified {
			return r, fmt.Errorf("independent acceptance is unverified; explicit human review with --accept-unverified is required before publication")
		}
		r.AcceptanceReviewAcknowledged = o.AcceptUnverified
		r.Title = o.Title
		r.State = "prepared"
		if err = writeReceipt(o.Options, r); err != nil {
			return r, err
		}
	}
	if r.Preview.AcceptanceUnverified && !r.AcceptanceReviewAcknowledged {
		return r, fmt.Errorf("delivery receipt lacks explicit acceptance review")
	}
	// Read the workspace from the original ledger; validation below binds it to this receipt.
	ledgerRaw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"))
	if err != nil {
		return r, err
	}
	var location struct {
		Workspace struct {
			Path string `json:"path"`
		} `json:"workspace"`
	}
	if err = json.Unmarshal(ledgerRaw, &location); err != nil {
		return r, err
	}
	path := location.Workspace.Path
	headRaw, err := git(ctx, path, nil, "rev-parse", "HEAD")
	if err != nil {
		return r, err
	}
	head := strings.TrimSpace(headRaw)
	record, err := validatedHead(ctx, o.Options, head)
	if err != nil {
		return r, err
	}
	if record.Workspace.BaseCommit != r.Preview.BaseCommit || record.Workspace.Repo != r.Preview.Repo || record.Workspace.Branch != r.Preview.Branch {
		return r, fmt.Errorf("ledger differs from reviewed intent")
	}
	if r.Commit != "" && head != r.Commit {
		return r, fmt.Errorf("workspace HEAD changed after delivery commit")
	}
	if head == r.Preview.BaseCommit {
		if r.Commit != "" {
			return r, fmt.Errorf("delivery commit disappeared")
		}
		current, err := Preview(ctx, o.Options)
		if err != nil {
			return r, err
		}
		if current.Digest != r.Preview.Digest {
			return r, fmt.Errorf("prepared content changed; review required")
		}
		if _, err = git(ctx, path, nil, "read-tree", r.Preview.Tree); err != nil {
			return r, err
		}
		tree, err := git(ctx, path, nil, "write-tree")
		if err != nil || strings.TrimSpace(tree) != r.Preview.Tree {
			return r, fmt.Errorf("content changed during staging; no commit performed")
		}
		if _, err = git(ctx, path, nil, "commit", "-m", r.Title); err != nil {
			return r, err
		}
		headRaw, err = git(ctx, path, nil, "rev-parse", "HEAD")
		if err != nil {
			return r, err
		}
		head = strings.TrimSpace(headRaw)
	}
	// Covers a crash after commit and before receipt persistence, and detects hook modifications.
	if err = verifyCommit(ctx, path, r, head); err != nil {
		return r, err
	}
	r.Commit = head
	runner := o.Runner
	if runner == nil {
		runner = process.Run
	}
	call := func(argv ...string) (string, error) {
		p := runner(ctx, process.Options{Argv: argv, Dir: path, Stdin: []byte{}, Timeout: 60 * time.Second, MaxOutputBytes: 1_000_000})
		if !p.OK || p.TimedOut || p.Interrupted || p.Overflow || len(p.Stdout)+len(p.Stderr) > 1_000_000 {
			return "", fmt.Errorf("%s failed or uncertain: %s %s", argv[0], p.Error, p.Stderr)
		}
		return strings.TrimSpace(p.Stdout), nil
	}
	reconcile := func(state, reason string) (Receipt, error) {
		r.Lifecycle = &LifecycleObservation{State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Reconciliation: reason}
		if e := writeReceipt(o.Options, r); e != nil {
			return r, e
		}
		return r, fmt.Errorf("PR reconciliation required: %s", reason)
	}
	verify := func() (Receipt, error) {
		raw, e := call("gh", "pr", "view", r.URL, "--repo", r.Preview.Repo, "--json", prFields)
		if e != nil {
			return reconcile("UNKNOWN", e.Error())
		}
		var pr remotePR
		if e = json.Unmarshal([]byte(raw), &pr); e != nil {
			return reconcile("UNKNOWN", "malformed PR observation")
		}
		if strings.TrimSpace(raw) == "null" {
			return reconcile("MISSING", "PR not found")
		}
		state := pr.State
		if state != "OPEN" && state != "CLOSED" && state != "MERGED" {
			return reconcile("UNKNOWN", "unknown or missing PR state")
		}
		if reason := pr.mismatch(r); reason != "" {
			return reconcile(state, reason)
		}
		if state != "OPEN" {
			return reconcile(state, "PR is "+state)
		}
		if pr.Draft == nil || !*pr.Draft {
			return reconcile(state, "PR is not a confirmed draft")
		}
		r.Lifecycle = &LifecycleObservation{State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if r.State != "published" {
			r.State = "published"
			r.DraftAtPublication = true
		}
		return r, writeReceipt(o.Options, r)
	}
	if r.URL != "" {
		if !validURL(r.Preview.Repo, r.URL) || (r.State != "published" && r.State != "committed") {
			return r, fmt.Errorf("invalid recorded PR URL")
		}
		return verify()
	}
	r.State = "committed"
	if err = writeReceipt(o.Options, r); err != nil {
		return r, err
	}
	if _, err = validatedHead(ctx, o.Options, r.Commit); err != nil {
		return r, err
	}
	if _, err = call("git", "fetch", "--no-tags", "origin", "+refs/heads/"+r.Preview.BaseBranch+":refs/remotes/origin/"+r.Preview.BaseBranch); err != nil {
		return r, err
	}
	if err = validateBase(ctx, path, r.Preview.BaseCommit, r.Preview.BaseBranch); err != nil {
		return r, err
	}
	policy := readiness.ReadRemotePolicy(ctx, r.Preview.Repo, r.Preview.BaseBranch, path, runner)
	r.Policy = &policy
	if err = writeReceipt(o.Options, r); err != nil {
		return r, err
	}
	plan := record.Readiness.Plans[len(record.Readiness.Plans)-1].Plan
	if err = policy.ValidateRequirements(plan.Inputs.MandatoryConstraints); err != nil {
		return r, err
	}
	if _, err = call("git", "push", "origin", r.Commit+":refs/heads/"+r.Preview.Branch); err != nil {
		return r, err
	}
	existing, err := call("gh", "pr", "list", "--repo", r.Preview.Repo, "--head", r.Preview.Branch, "--state", "all", "--limit", "2", "--json", prFields)
	if err != nil {
		return reconcile("UNKNOWN", err.Error())
	}
	var prs []remotePR
	if err = json.Unmarshal([]byte(existing), &prs); err != nil || prs == nil {
		return reconcile("UNKNOWN", "malformed PR list")
	}
	if len(prs) > 0 {
		if len(prs) != 1 {
			return reconcile("AMBIGUOUS", "multiple PRs found")
		}
		if !validURL(r.Preview.Repo, prs[0].URL) {
			return reconcile("UNKNOWN", "unexpected PR URL")
		}
		r.URL = prs[0].URL
		if err = writeReceipt(o.Options, r); err != nil {
			return r, err
		}
		if reason := prs[0].mismatch(r); reason != "" {
			return reconcile(prs[0].observedState(), reason)
		}
		if prs[0].State != "OPEN" {
			return reconcile(prs[0].observedState(), "listed PR is not confirmed OPEN")
		}
		if prs[0].Draft == nil || !*prs[0].Draft {
			return reconcile("OPEN", "listed PR is not a confirmed draft")
		}
	} else {
		if r.CreationAttemptedAt != "" {
			return reconcile("MISSING", "no PR found for unresolved creation attempt; replacement requires reconciliation")
		}
		body := fmt.Sprintf("Implements issue #%d.\n\nMolecule: `%s`\nFormula: `%s`\nBase: `%s`\nDelivery: `%s`\n\nAwaiting exact-commit CI and human review. Harness-reported checks are not independent CI evidence. No merge, deployment or issue closure is authorized.\n", record.Issue.Number, record.ID, record.FormulaID, r.Preview.BaseCommit, r.Commit)
		f, err := os.CreateTemp(filepath.Join(o.LabDir, "deliveries"), ".pr-body-")
		if err != nil {
			return r, err
		}
		defer os.Remove(f.Name())
		if _, err = f.WriteString(body); err != nil {
			f.Close()
			return r, err
		}
		if err = f.Close(); err != nil {
			return r, err
		}
		r.CreationAttemptedAt = time.Now().UTC().Format(time.RFC3339Nano)
		r.Lifecycle = &LifecycleObservation{State: "UNKNOWN", ObservedAt: r.CreationAttemptedAt, Reconciliation: "PR creation intent recorded; outcome unresolved"}
		if err = writeReceipt(o.Options, r); err != nil {
			return r, err
		}
		url, createErr := call("gh", "pr", "create", "--draft", "--repo", r.Preview.Repo, "--base", r.Preview.BaseBranch, "--head", r.Preview.Branch, "--title", r.Title, "--body-file", f.Name())
		if createErr != nil {
			return reconcile("UNKNOWN", createErr.Error())
		}
		if !validURL(r.Preview.Repo, url) {
			return reconcile("UNKNOWN", "creation returned a missing or unexpected GitHub PR URL")
		}
		r.URL = url
	}
	if !validURL(r.Preview.Repo, r.URL) {
		return r, fmt.Errorf("unexpected GitHub PR URL")
	}
	// Retain the returned identity before verification so retries never create a replacement.
	if err = writeReceipt(o.Options, r); err != nil {
		return r, err
	}
	return verify()
}
func unique(files []string) []string {
	out := []string{}
	for _, f := range files {
		if len(out) == 0 || out[len(out)-1] != f {
			out = append(out, f)
		}
	}
	return out
}
func validURL(repo, url string) bool {
	prefix := "https://github.com/" + repo + "/pull/"
	if !strings.HasPrefix(url, prefix) {
		return false
	}
	number := strings.TrimPrefix(url, prefix)
	if number == "" || number[0] == '0' {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const prFields = "url,state,baseRefName,headRefName,headRefOid,isDraft,headRepository,headRepositoryOwner"

type remotePR struct {
	URL        string `json:"url"`
	State      string `json:"state"`
	Base       string `json:"baseRefName"`
	Branch     string `json:"headRefName"`
	Head       string `json:"headRefOid"`
	Draft      *bool  `json:"isDraft"`
	Repository *struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	Owner *struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

func (p remotePR) mismatch(r Receipt) string {
	if p.URL != r.URL || !validURL(r.Preview.Repo, p.URL) {
		return "target repository or PR URL mismatch"
	}
	if p.Repository == nil || p.Owner == nil || p.Repository.Name == "" || p.Owner.Login == "" || p.Owner.Login+"/"+p.Repository.Name != r.Preview.Repo {
		return "head repository missing or mismatched"
	}
	if p.Base != r.Preview.BaseBranch || p.Branch != r.Preview.Branch {
		return "base or head branch missing or mismatched"
	}
	if p.Head != r.Commit {
		return "head commit missing or moved"
	}
	return ""
}

func (p remotePR) observedState() string {
	switch p.State {
	case "OPEN", "CLOSED", "MERGED":
		return p.State
	}
	return "UNKNOWN"
}
