package readiness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// CollectOptions opts a collection in to evidence a reviewed Formula cites. The
// zero value collects exactly what Collect always has.
type CollectOptions struct {
	// GitLabCI appends the committed root .gitlab-ci.yml after the tree walk.
	GitLabCI bool
}

// gitlabCIFile is the root GitLab CI file and gitlabCIMaxBytes its own read
// bound, the same bound the GitLab baseline parser reads.
const (
	gitlabCIFile     = ".gitlab-ci.yml"
	gitlabCIMaxBytes = 256 << 10
)

// Collect reads only selected committed regular files. An index of other paths
// supports analysis without copying arbitrary files, ignored data or credentials.
func Collect(ctx context.Context, dir, ref string) (RepositorySnapshot, error) {
	return CollectFor(ctx, dir, ref, CollectOptions{})
}

// CollectFor is Collect with options. Evidence an option adds is appended after
// the tree walk, so no evidence id the zero value assigns ever shifts.
func CollectFor(ctx context.Context, dir, ref string, opts CollectOptions) (RepositorySnapshot, error) {
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
	if opts.GitLabCI {
		if err := appendRootEvidence(ctx, dir, &s, total, gitlabCIFile, gitlabCIMaxBytes); err != nil {
			return s, err
		}
	}
	s.Unknowns = []string{"Remote branch/review rules have not been verified; this does not block local intake.", "Issue comments are not read. Put clarifications in the Issue body.", "Workflow and manifest content is evidence, not proof that commands pass or policies are enforced."}
	return s, nil
}

// appendRootEvidence appends the committed file at the exact path file as the
// last evidence entry, under the same mode, denied-path, UTF-8, count and 1 MB
// total rules as the tree walk and its own per-file bound of limit bytes. An
// absent file adds nothing; any other refusal is an error, never a silent skip.
// The size is read before the content, so an oversized blob is never read.
func appendRootEvidence(ctx context.Context, dir string, s *RepositorySnapshot, total int, file string, limit int) error {
	var entry *TreeFile
	for i := range s.Files {
		if s.Files[i].Path == file {
			entry = &s.Files[i]
			break
		}
	}
	if entry == nil {
		return nil
	}
	for _, e := range s.Evidence {
		if e.Path == file {
			return nil
		}
	}
	if d := deniedText(file); d != "" {
		return fmt.Errorf("%s: refused as evidence (%s)", file, d)
	}
	if entry.Mode != "100644" && entry.Mode != "100755" {
		return fmt.Errorf("evidence must be a regular committed file: %s", file)
	}
	if len(s.Evidence) >= 256 {
		return fmt.Errorf("too many evidence files; explicit scoping required")
	}
	size, err := repositoryGit(ctx, dir, 256, "cat-file", "-s", entry.Object)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	var n int
	if _, err := fmt.Sscan(size, &n); err != nil || n < 0 {
		return fmt.Errorf("%s: committed blob size unavailable", file)
	}
	if n > limit {
		return fmt.Errorf("%s is %d bytes, more than the %d bytes read as evidence; explicit scoping required", file, n, limit)
	}
	if total+n > 1_000_000 {
		return fmt.Errorf("repository evidence exceeds 1 MB")
	}
	content, err := repositoryGit(ctx, dir, limit+4096, "cat-file", "blob", entry.Object)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if len(content) != n {
		return fmt.Errorf("%s: committed blob size changed while it was read", file)
	}
	if !textContent(content) {
		return fmt.Errorf("%s: binary or invalid UTF-8 committed evidence", file)
	}
	s.Evidence = append(s.Evidence, FileEvidence{Evidence: Evidence{ID: fmt.Sprintf("file-%d", len(s.Evidence)+1), Path: file, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content})
	return nil
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
	return CollectForTicketFor(ctx, dir, ref, ticket, CollectOptions{})
}

// CollectForTicketFor is CollectForTicket over CollectFor with options.
func CollectForTicketFor(ctx context.Context, dir, ref, ticket string, opts CollectOptions) (RepositorySnapshot, error) {
	s, err := CollectFor(ctx, dir, ref, opts)
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

// MaxSupplementalPasses bounds the read-only supplemental evidence passes of one intake.
const MaxSupplementalPasses = 2

// Excerpt and outline bounds. A range or outline reads at most
// maxRangeSourceBytes of the committed blob.
const (
	maxExcerptBytes     = 64000
	maxExcerptLines     = 1500
	maxRangeSourceBytes = 4_000_000
)

// LineRangeHint is the reason suffix of an oversized whole-file request.
func LineRangeHint(size int) string {
	return fmt.Sprintf("file is %d bytes; request line ranges (path#Lstart-end) for content", size)
}

// SupplementalPasses counts the supplemental passes already collected.
func (s RepositorySnapshot) SupplementalPasses() int {
	n := 0
	for _, pass := range s.Collection {
		if pass.Supplemental {
			n++
		}
	}
	return n
}

// UnmetRequests describes each request with the latest collection outcome for its
// path, for the message that stops intake once the supplemental passes are spent.
// A request whose path an earlier pass already collected is not unmet; it is named
// separately as already collected. Paths compare in normalized form, so a.go#L5
// and a.go#L5-5 are the same request.
func (s RepositorySnapshot) UnmetRequests(requests []EvidenceRequest) string {
	latest := map[string]string{}
	for _, pass := range s.Collection {
		for _, o := range pass.Outcomes {
			latest[NormalizeEvidenceRequest(o.Path)] = o.Reason
		}
	}
	unmet, collected := []string{}, []string{}
	for _, r := range requests {
		reason, ok := latest[NormalizeEvidenceRequest(r.Path)]
		if ok && (collectedOutcome(reason) || reason == "already collected") {
			collected = append(collected, r.Path)
			continue
		}
		if !ok {
			reason = "not collected: no supplemental pass remains"
		}
		unmet = append(unmet, fmt.Sprintf("%s (%s)", r.Path, reason))
	}
	out := "none"
	if len(unmet) > 0 {
		out = strings.Join(unmet, "; ")
	}
	if len(collected) > 0 {
		out += "; already collected: " + strings.Join(collected, ", ")
	}
	return out
}

// CollectSupplemental performs one bounded pass against the original commit, at
// most MaxSupplementalPasses times per snapshot.
func CollectSupplemental(ctx context.Context, dir string, s RepositorySnapshot, requests []EvidenceRequest) (RepositorySnapshot, error) {
	if s.SupplementalPasses() >= MaxSupplementalPasses || len(requests) == 0 {
		return s, fmt.Errorf("supplemental evidence passes exhausted")
	}
	if err := ValidateEvidenceRequests(requests, s.References()); err != nil {
		return s, err
	}
	s = collectRequests(ctx, dir, s, requests)
	s.Collection[len(s.Collection)-1].Supplemental = true
	return s, nil
}

// collectedOutcome reports whether an outcome added evidence content, which counts
// toward the 16 named-file bound.
func collectedOutcome(reason string) bool {
	return reason == "collected" || strings.HasPrefix(reason, "collected;") || strings.HasPrefix(reason, "outline collected;")
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
			if collectedOutcome(o.Reason) {
				named++
			}
		}
	}
	for _, r := range requests {
		reason := ""
		file, start, end, ranged, rangeErr := ParseEvidenceRange(r.Path)
		f, exists := files[file]
		if e := ValidatePath(r.Path); e != nil {
			reason = e.Error()
		} else if rangeErr != nil {
			reason = rangeErr.Error()
		} else if e := ValidatePath(file); e != nil {
			reason = e.Error()
		} else if d := deniedText(file); d != "" {
			reason = d
		} else if !exists {
			reason = "absent from frozen commit"
			for ancestor := path.Dir(file); ancestor != "."; ancestor = path.Dir(ancestor) {
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
			} else if _, e = fmt.Sscan(size, &n); e != nil {
				reason = "committed blob size unavailable"
			} else if !ranged && n <= maxExcerptBytes && total+n > 1000000 {
				reason = "1000000-byte total bound exceeded"
			} else if !ranged && n > maxExcerptBytes && path.Ext(file) != ".go" {
				reason = "64000-byte per-file bound exceeded; " + LineRangeHint(n)
			} else if (ranged || n > maxExcerptBytes) && n > maxRangeSourceBytes {
				reason = fmt.Sprintf("%d-byte range and outline source bound exceeded; %s", maxRangeSourceBytes, LineRangeHint(n))
			} else {
				limit := maxExcerptBytes
				if ranged || n > maxExcerptBytes {
					limit = maxRangeSourceBytes
				}
				blob, e := repositoryGit(ctx, dir, limit, "cat-file", "blob", f.Object)
				evidencePath, content := r.Path, blob
				if e != nil {
					reason = "committed blob read failed"
				} else if !textContent(blob) {
					reason = "binary or invalid UTF-8 content"
				} else if ranged {
					evidencePath, content, reason = lineExcerpt(file, blob, start, end)
				} else if n > maxExcerptBytes {
					evidencePath, content, reason = file+"#outline", goOutline(file, blob), "outline collected; "+LineRangeHint(n)
				} else {
					reason = "collected"
				}
				if collectedOutcome(reason) {
					if seen[evidencePath] {
						reason = "already collected"
					} else if total+len(content) > 1000000 {
						reason = "1000000-byte total bound exceeded"
					} else {
						s.Evidence = append(s.Evidence, FileEvidence{Evidence: Evidence{ID: fmt.Sprintf("file-%d", len(s.Evidence)+1), Path: evidencePath, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content})
						seen[evidencePath] = true
						total += len(content)
						named++
					}
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

// lineExcerpt returns the exact bytes of lines start..end (1-based, inclusive) of
// blob, its evidence path and the outcome reason. The range is clamped to the end
// of the file and to the per-excerpt line and byte bounds; each clamp is recorded
// in the reason and in the evidence path, which names the lines actually returned.
func lineExcerpt(file, blob string, start, end int) (string, string, string) {
	lines := strings.SplitAfter(blob, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if start > len(lines) {
		return "", "", fmt.Sprintf("line range starts past the end of the file (%d lines)", len(lines))
	}
	clamps := []string{}
	if end > len(lines) {
		end = len(lines)
		clamps = append(clamps, fmt.Sprintf("range clamped to the end of the file at line %d", end))
	}
	if end-start+1 > maxExcerptLines {
		end = start + maxExcerptLines - 1
		clamps = append(clamps, fmt.Sprintf("range clamped to the %d-line excerpt bound at line %d", maxExcerptLines, end))
	}
	size := 0
	for i := start; i <= end; i++ {
		if size+len(lines[i-1]) > maxExcerptBytes {
			if i == start {
				return "", "", fmt.Sprintf("line %d exceeds the %d-byte excerpt bound", start, maxExcerptBytes)
			}
			end = i - 1
			clamps = append(clamps, fmt.Sprintf("range clamped to the %d-byte excerpt bound at line %d", maxExcerptBytes, end))
			break
		}
		size += len(lines[i-1])
	}
	reason := "collected"
	if len(clamps) > 0 {
		reason += "; " + strings.Join(clamps, "; ")
	}
	return fmt.Sprintf("%s#L%d-%d", file, start, end), strings.Join(lines[start-1:end], ""), reason
}

// goOutline lists the top-level func, type, const and var declarations of a Go
// file with their line ranges, bounded to maxExcerptBytes: a func by its signature
// alone, and a type, const or var by its names, never by its body or value. A file
// that does not parse still lists the declarations parsed before the error; a
// malformed func signature is listed by name alone.
func goOutline(file, blob string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Outline of %s: %s.\n", file, LineRangeHint(len(blob)))
	entries := []string{}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, blob, parser.SkipObjectResolution)
	lineOf := func(p token.Pos) int { return fset.Position(p).Line }
	// span prints the line range of from..to. A malformed declaration can end at
	// NoPos or before it starts; its range then ends on its first line.
	span := func(from, to token.Pos) string {
		start, end := lineOf(from), lineOf(to)
		if !to.IsValid() || end < start {
			end = start
		}
		return fmt.Sprintf("L%d-%d", start, end)
	}
	// signature prints the source of a func from its keyword to the end of its
	// type on one line, so an outline names it without carrying a body or literal.
	// go/parser can return a malformed signature that ends at NoPos or before it
	// starts; that func is named alone rather than sliced from the blob.
	signature := func(decl *ast.FuncDecl) string {
		from, to := decl.Pos(), decl.Type.End()
		lo, hi := fset.Position(from).Offset, fset.Position(to).Offset
		if !from.IsValid() || !to.IsValid() || hi <= lo || hi > len(blob) {
			name := "_"
			if decl.Name != nil {
				name = decl.Name.Name
			}
			return "func " + name + " (signature did not parse)"
		}
		text := strings.Join(strings.Fields(blob[lo:hi]), " ")
		if len(text) > 200 {
			cut := 200
			for !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + "..."
		}
		return text
	}
	if parsed != nil {
		for _, d := range parsed.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				entries = append(entries, span(decl.Pos(), decl.End())+" "+signature(decl))
			case *ast.GenDecl:
				if decl.Tok != token.TYPE && decl.Tok != token.CONST && decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					from, to := spec.Pos(), spec.End()
					if !decl.Lparen.IsValid() {
						from, to = decl.Pos(), decl.End()
					}
					names := []string{}
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						names = append(names, sp.Name.Name)
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							names = append(names, n.Name)
						}
					}
					entries = append(entries, fmt.Sprintf("%s %s %s", span(from, to), decl.Tok, strings.Join(names, ", ")))
				}
			}
		}
	}
	if err != nil {
		entries = append(entries, "// The file did not parse completely; later declarations may be missing.")
	}
	for i, entry := range entries {
		if b.Len()+len(entry)+1 > maxExcerptBytes-128 {
			fmt.Fprintf(&b, "// Outline truncated at the %d-byte bound; %d further entries omitted.\n", maxExcerptBytes, len(entries)-i)
			break
		}
		b.WriteString(entry + "\n")
	}
	return b.String()
}

// Reject binary control bytes as well as invalid UTF-8; ordinary text whitespace is allowed.
func textContent(content string) bool {
	return utf8.ValidString(content) && strings.IndexFunc(content, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) < 0
}
