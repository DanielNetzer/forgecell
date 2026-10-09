package readiness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

type TreeFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
}
type FileEvidence struct {
	Evidence
	Content string `json:"content"`
}
type CollectionOutcome struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type CollectionPass struct {
	Supplemental bool                `json:"supplemental,omitempty"`
	Requests     []EvidenceRequest   `json:"requests"`
	Outcomes     []CollectionOutcome `json:"outcomes"`
}
type RepositorySnapshot struct {
	Collection []CollectionPass `json:"collection,omitempty"`
	Commit     string           `json:"commit"`
	Files      []TreeFile       `json:"files"`
	Evidence   []FileEvidence   `json:"evidence"`
	Unknowns   []string         `json:"unknowns"`
}

func committedEvidence(file string) bool {
	if deniedText(file) != "" {
		return false
	}
	base := path.Base(file)
	if strings.Contains("/"+file, "/node_modules/") || strings.Contains("/"+file, "/vendor/") {
		return false
	}
	return base == "go.mod" || base == "package.json" || base == "pyproject.toml" || base == "CODEOWNERS" || base == "CONTRIBUTING.md" || file == "README.md" || strings.HasPrefix(file, ".github/workflows/") && (strings.HasSuffix(file, ".yml") || strings.HasSuffix(file, ".yaml"))
}
func repositoryGit(ctx context.Context, dir string, limit int, args ...string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: dir, Stdin: []byte{}, Timeout: 20 * time.Second, MaxOutputBytes: limit})
	if !r.OK {
		return "", fmt.Errorf("committed evidence: %s %s", r.Error, r.Stderr)
	}
	return r.Stdout, nil
}

// Collect reads only selected committed regular files. An index of other paths
// supports analysis without copying arbitrary files, ignored data or credentials.
func Collect(ctx context.Context, dir, ref string) (RepositorySnapshot, error) {
	var s RepositorySnapshot
	if ref == "" || strings.HasPrefix(ref, "-") {
		return s, fmt.Errorf("invalid evidence revision")
	}
	commit, err := repositoryGit(ctx, dir, 256, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return s, err
	}
	s.Commit = strings.TrimSpace(commit)
	raw, err := repositoryGit(ctx, dir, 2_000_000, "ls-tree", "-r", "-z", s.Commit)
	if err != nil {
		return s, err
	}
	total := 0
	for _, line := range strings.Split(raw, "\x00") {
		if line == "" {
			continue
		}
		header, file, ok := strings.Cut(line, "\t")
		parts := strings.Fields(header)
		if !ok || len(parts) != 3 {
			return s, fmt.Errorf("invalid committed tree entry")
		}
		if err := ValidatePath(file); err != nil {
			return s, fmt.Errorf("repository contains unsupported path: %w", err)
		}
		s.Files = append(s.Files, TreeFile{Path: file, Mode: parts[0], Object: parts[2]})
		if len(s.Files) > 20000 {
			return s, fmt.Errorf("repository exceeds 20000 indexed paths; explicit scoping required")
		}
		if !committedEvidence(file) {
			continue
		}
		if parts[0] != "100644" && parts[0] != "100755" {
			return s, fmt.Errorf("evidence must be a regular committed file: %s", file)
		}
		if len(s.Evidence) >= 256 {
			return s, fmt.Errorf("too many evidence files; explicit scoping required")
		}
		content, err := repositoryGit(ctx, dir, 64000, "cat-file", "blob", parts[2])
		if err != nil {
			return s, fmt.Errorf("%s: %w", file, err)
		}
		if !textContent(content) {
			return s, fmt.Errorf("%s: binary or invalid UTF-8 committed evidence", file)
		}
		total += len(content)
		if total > 1_000_000 {
			return s, fmt.Errorf("repository evidence exceeds 1 MB")
		}
		s.Evidence = append(s.Evidence, FileEvidence{Evidence: Evidence{ID: fmt.Sprintf("file-%d", len(s.Evidence)+1), Path: file, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content})
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	s.Unknowns = []string{"Remote branch/review rules have not been verified; this does not block local intake.", "Issue comments are not read. Put clarifications in the Issue body.", "Workflow and manifest content is evidence, not proof that commands pass or policies are enforced."}
	return s, nil
}
func (s RepositorySnapshot) References() []Evidence {
	out := make([]Evidence, len(s.Evidence))
	for i, e := range s.Evidence {
		out[i] = e.Evidence
	}
	return out
}
func (s RepositorySnapshot) Digest() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
func (s RepositorySnapshot) ValidateScope(scope []ScopedPath) error {
	files := map[string]TreeFile{}
	for _, f := range s.Files {
		files[f.Path] = f
	}
	refs := map[string]FileEvidence{}
	for _, e := range s.Evidence {
		refs[e.ID] = e
	}
	seen := map[string]bool{}
	for _, item := range scope {
		file := item.Path
		if err := ValidatePath(file); err != nil {
			return err
		}
		folded := strings.ToLower(file)
		if seen[folded] {
			return fmt.Errorf("case-ambiguous or duplicate scope: %s", file)
		}
		seen[folded] = true
		for ancestor := file; ancestor != "."; ancestor = path.Dir(ancestor) {
			if f, ok := files[ancestor]; ok && (ancestor != file || (f.Mode != "100644" && f.Mode != "100755")) {
				return fmt.Errorf("scope traverses a file, symlink or submodule: %s", ancestor)
			}
		}
		for existing := range files {
			if strings.HasPrefix(existing, file+"/") {
				return fmt.Errorf("scope is a directory, not an exact file: %s", file)
			}
		}
		if _, exists := files[file]; exists {
			continue
		}
		supported := false
		for _, id := range item.Evidence {
			e, ok := refs[id]
			if !ok {
				continue
			}
			base := path.Base(e.Path)
			if base != "go.mod" && base != "package.json" && base != "pyproject.toml" {
				continue
			}
			root := path.Dir(e.Path)
			if root == "." || strings.HasPrefix(file, root+"/") {
				supported = true
			}
		}
		if !supported {
			return fmt.Errorf("new path %s needs evidence for its parent component", file)
		}
	}
	return nil
}

// CollectForTicket selects exact paths named in ticket prose, including absent paths.
func CollectForTicket(ctx context.Context, dir, ref, ticket string) (RepositorySnapshot, error) {
	s, err := Collect(ctx, dir, ref)
	if err != nil {
		return s, err
	}
	names := map[string]bool{}
	for _, match := range regexp.MustCompile("`([^`\\r\\n]+)`").FindAllStringSubmatch(ticket, -1) {
		if !strings.Contains(match[1], " ") {
			names[match[1]] = true
		}
	}
	for _, token := range regexp.MustCompile("[A-Za-z0-9_./-]+\\.[A-Za-z0-9_-]+").FindAllString(ticket, -1) {
		names[strings.TrimRight(token, ".")] = true
	}
	for _, f := range s.Files {
		if regexp.MustCompile(`(^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(f.Path) + `($|[^A-Za-z0-9_./-])`).MatchString(ticket) {
			names[f.Path] = true
		}
	}
	paths := []string{}
	for p := range names {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	requests := []EvidenceRequest{}
	for _, p := range paths {
		requests = append(requests, EvidenceRequest{Path: p, Reason: "Explicitly named in ticket", Evidence: []string{"issue"}})
	}
	return collectRequests(ctx, dir, s, requests), nil
}

func deniedText(file string) string {
	lower := strings.ToLower(file)
	if lower == ".docker/config.json" || strings.HasSuffix(lower, "/.docker/config.json") {
		return "container registry credentials"
	}
	for _, part := range strings.Split(lower, "/") {
		for _, word := range []string{"secret", "credential", "private"} {
			if strings.Contains(part, word) {
				return "sensitive or private path"
			}
		}
		switch part {
		case ".env", ".aws", ".ssh", ".codex", ".agents", ".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials", "id_rsa", "id_ed25519", ".kube", ".terraform", ".venv", "node_modules", "vendor", "dist", "build", "generated", "coverage", ".next", ".cache", "tmp", "logs", "artifacts":
			return "sensitive, private or generated path"
		}
		if strings.HasPrefix(part, ".env.") || part == "auth.json" || part == "service-account.json" {
			return "environment credentials"
		}
	}
	switch path.Ext(lower) {
	case ".pem", ".key", ".p12", ".pfx":
		return "credential file"
	case ".lock":
		return "generated lockfile"
	case ".png", ".jpg", ".jpeg", ".gif", ".pdf", ".zip", ".gz", ".exe", ".wasm", ".sqlite", ".db":
		return "binary file type"
	}
	if strings.HasSuffix(lower, "package-lock.json") {
		return "generated lockfile"
	}
	return ""
}

// CollectSupplemental performs one bounded pass against the original commit.
func CollectSupplemental(ctx context.Context, dir string, s RepositorySnapshot, requests []EvidenceRequest) (RepositorySnapshot, error) {
	for _, pass := range s.Collection {
		if pass.Supplemental {
			return s, fmt.Errorf("supplemental evidence pass exhausted")
		}
	}
	if len(requests) == 0 {
		return s, fmt.Errorf("supplemental evidence pass exhausted")
	}
	if err := ValidateEvidenceRequests(requests, s.References()); err != nil {
		return s, err
	}
	s = collectRequests(ctx, dir, s, requests)
	s.Collection[len(s.Collection)-1].Supplemental = true
	return s, nil
}
func collectRequests(ctx context.Context, dir string, s RepositorySnapshot, requests []EvidenceRequest) RepositorySnapshot {
	pass := CollectionPass{Requests: requests, Outcomes: []CollectionOutcome{}}
	files := map[string]TreeFile{}
	for _, f := range s.Files {
		files[f.Path] = f
	}
	seen := map[string]bool{}
	total := 0
	named := 0
	for _, e := range s.Evidence {
		seen[e.Path] = true
		total += len(e.Content)
	}
	for _, p := range s.Collection {
		for _, o := range p.Outcomes {
			if o.Reason == "collected" {
				named++
			}
		}
	}
	for _, r := range requests {
		reason := ""
		f, exists := files[r.Path]
		if e := ValidatePath(r.Path); e != nil {
			reason = e.Error()
		} else if d := deniedText(r.Path); d != "" {
			reason = d
		} else if !exists {
			reason = "absent from frozen commit"
			for ancestor := path.Dir(r.Path); ancestor != "."; ancestor = path.Dir(ancestor) {
				if entry, ok := files[ancestor]; ok {
					if entry.Mode == "120000" || entry.Mode == "160000" {
						reason = "path traverses a symlink or submodule"
					} else {
						reason = "path traverses a regular file"
					}
					break
				}
			}
		} else if f.Mode != "100644" && f.Mode != "100755" {
			reason = "not a regular committed file (symlink or submodule)"
		} else if seen[r.Path] {
			reason = "already collected"
		} else if named >= 16 {
			reason = "16 named-file bound reached"
		} else if len(s.Evidence) >= 256 {
			reason = "256 evidence-file bound reached"
		}
		if reason == "" {
			size, e := repositoryGit(ctx, dir, 256, "cat-file", "-s", f.Object)
			var n int
			if e != nil {
				reason = "committed blob size unavailable"
			} else if _, e = fmt.Sscan(size, &n); e != nil || n > 64000 {
				reason = "64000-byte per-file bound exceeded"
			} else if total+n > 1000000 {
				reason = "1000000-byte total bound exceeded"
			} else {
				content, e := repositoryGit(ctx, dir, 64000, "cat-file", "blob", f.Object)
				if e != nil {
					reason = "committed blob read failed"
				} else if !textContent(content) {
					reason = "binary or invalid UTF-8 content"
				} else {
					s.Evidence = append(s.Evidence, FileEvidence{Evidence: Evidence{ID: fmt.Sprintf("file-%d", len(s.Evidence)+1), Path: r.Path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content})
					seen[r.Path] = true
					total += len(content)
					named++
					reason = "collected"
				}
			}
		}
		pass.Outcomes = append(pass.Outcomes, CollectionOutcome{Path: r.Path, Reason: reason})
	}
	if len(requests) > 0 {
		s.Collection = append(s.Collection, pass)
	}
	return s
}

// Reject binary control bytes as well as invalid UTF-8; ordinary text whitespace is allowed.
func textContent(content string) bool {
	return utf8.ValidString(content) && strings.IndexFunc(content, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) < 0
}
