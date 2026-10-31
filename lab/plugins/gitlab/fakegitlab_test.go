package main

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const (
	fakeToken  = "glpat-faketoken0123456789abcdef"
	headCommit = "0123456789abcdef0123456789abcdef01234567"
	otherSHA   = "89abcdef0123456789abcdef0123456789abcdef"
	mergeSHA   = "fedcba9876543210fedcba9876543210fedcba98"
	// projectKey is how the canned project appears in an escaped request path.
	projectKey = "example%2Fproject"
	projectID  = "42"
)

// seenRequest is one request the fake answered.
type seenRequest struct {
	Method, Path, Query string
	Token               string
	Authorization       string
	Body                map[string]any
}

// fakeGitLab is an in-process GitLab REST v4 over TLS with canned JSON for
// every endpoint the plugin calls, plus failure switches.
type fakeGitLab struct {
	srv *httptest.Server

	mu sync.Mutex
	// Canned data, editable by tests before a call.
	project   map[string]any
	protected map[string]bool
	mrs       []map[string]any
	pipelines map[string][]map[string]any // by iid
	jobs      map[string][]map[string]any // by pipeline id
	notes     map[string][]map[string]any // by iid
	created   map[string]any              // the POST answer; nil derives one

	// Failure switches. status answers every request (or only paths with
	// statusPath as a suffix) with that status and a body naming the token.
	status     int
	statusPath string
	oversized  bool
	redirect   bool
	endless    bool
	badJSON    bool
	duplicate  bool
	wrongType  bool
	hang       bool
	bigPage    bool
	// postFault creates the merge request a POST asks for, then loses the
	// answer: "hang" never answers, "malformed" and "oversized" send a body
	// the plugin refuses.
	postFault   string
	release     chan struct{}
	requests    []seenRequest
	pageSizeCap int
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{release: make(chan struct{})}
	f.reset()
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// Registered after Close, so it runs first and frees a hung handler.
	t.Cleanup(func() { close(f.release) })
	return f
}

// reset restores the canned data.
func (f *fakeGitLab) reset() {
	f.project = map[string]any{
		"id": 42, "path_with_namespace": "example/project", "default_branch": "main",
		"web_url": "https://gitlab.example/example/project", "only_allow_merge_if_pipeline_succeeds": true,
		"approvals_before_merge": 2, "name": "project", "visibility": "private",
	}
	f.protected = map[string]bool{"main": true}
	f.mrs = []map[string]any{
		{"iid": 7, "state": "opened", "draft": true, "sha": headCommit, "web_url": "https://gitlab.example/example/project/-/merge_requests/7", "source_branch": "forgecell/x", "source_project_id": 42, "target_project_id": 42, "merged_at": nil, "merge_commit_sha": nil},
		{"iid": 8, "state": "opened", "draft": false, "sha": otherSHA, "web_url": "https://gitlab.example/example/project/-/merge_requests/8", "source_branch": "forgecell/x", "source_project_id": 99, "target_project_id": 42},
		{"iid": 9, "state": "merged", "draft": false, "sha": headCommit, "web_url": "https://gitlab.example/example/project/-/merge_requests/9", "source_branch": "done", "source_project_id": 42, "target_project_id": 42, "merged_at": "2026-10-30T12:34:56.789Z", "merge_commit_sha": mergeSHA},
	}
	f.pipelines = map[string][]map[string]any{
		"7": {
			{"id": 101, "sha": otherSHA, "status": "success", "web_url": "https://gitlab.example/example/project/-/pipelines/101"},
			{"id": 103, "sha": headCommit, "status": "failed", "web_url": "https://gitlab.example/example/project/-/pipelines/103"},
			{"id": 102, "sha": headCommit, "status": "success", "web_url": "https://gitlab.example/example/project/-/pipelines/102"},
		},
	}
	f.jobs = map[string][]map[string]any{
		"103": {
			{"id": 3, "name": "test", "status": "failed", "web_url": "https://gitlab.example/example/project/-/jobs/3"},
			{"id": 1, "name": "build", "status": "success", "web_url": "https://gitlab.example/example/project/-/jobs/1"},
			{"id": 4, "name": "deploy", "status": "manual"},
		},
	}
	f.notes = map[string][]map[string]any{
		"7": {
			{"id": 1, "body": "assigned to @bot", "system": true, "created_at": "2026-10-30T10:00:00Z", "author": map[string]any{"username": "alice", "bot": false}},
			{"id": 2, "body": "Looks good", "system": false, "created_at": "2026-10-30T11:00:00.000Z", "author": map[string]any{"username": "alice", "bot": false}},
			{"id": 3, "body": "Nit here", "system": false, "created_at": "2026-10-30T12:00:00Z", "author": map[string]any{"username": "review-bot", "bot": true}, "position": map[string]any{"new_path": "src/main.go", "new_line": 12, "old_line": nil}},
		},
	}
	f.created = nil
	f.status, f.statusPath = 0, ""
	f.oversized, f.redirect, f.endless, f.badJSON, f.duplicate, f.wrongType, f.hang, f.bigPage = false, false, false, false, false, false, false, false
	f.postFault = ""
	f.requests = nil
	f.pageSizeCap = 0
}

// createMR records the merge request a POST body asks for as an open merge
// request of the project and returns it.
func (f *fakeGitLab) createMR(in map[string]any) map[string]any {
	title, _ := in["title"].(string)
	iid := 12
	for _, mr := range f.mrs {
		iid = max(iid, toInt(mr["iid"])+1)
	}
	mr := map[string]any{
		"iid": iid, "state": "opened", "draft": draftPrefix.MatchString(title), "sha": headCommit,
		"web_url": "https://gitlab.example/example/project/-/merge_requests/" + strconv.Itoa(iid), "title": title,
		"source_branch": in["source_branch"], "target_branch": in["target_branch"], "source_project_id": 42, "target_project_id": 42,
	}
	f.mrs = append(f.mrs, mr)
	return mr
}

// set changes the fake under its lock.
func (f *fakeGitLab) set(change func(f *fakeGitLab)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeGitLab) seen() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.requests...)
}

// caFile writes the fake's TLS certificate as a PEM bundle in a fresh
// temporary directory and returns its path.
func (f *fakeGitLab) caFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(plugintest.TempDir(t), "gitlab-ca.pem")
	raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env is a needs.env lookup that points the plugin at the fake.
func (f *fakeGitLab) env(t *testing.T) map[string]string {
	return map[string]string{tokenEnv: fakeToken, urlEnv: f.srv.URL, caFileEnv: f.caFile(t)}
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	path := r.URL.EscapedPath()
	seen := seenRequest{Method: r.Method, Path: path, Query: r.URL.RawQuery, Token: r.Header.Get("PRIVATE-TOKEN"), Authorization: r.Header.Get("Authorization")}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(raw) > 0 {
			json.Unmarshal(raw, &seen.Body)
		}
	}
	f.requests = append(f.requests, seen)
	hang := f.hang
	fault := ""
	if r.Method == http.MethodPost && f.postFault != "" {
		fault = f.postFault
		f.createMR(seen.Body)
	}
	f.mu.Unlock()
	switch fault {
	case "hang":
		hang = true
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"iid": 12,`)
		return
	case "oversized":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"pad":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
		return
	}
	if hang {
		select {
		case <-r.Context().Done():
		case <-f.release:
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.status != 0 && (f.statusPath == "" || strings.HasSuffix(path, f.statusPath)):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, `{"message":"denied for token `+fakeToken+`"}`)
		return
	case f.redirect:
		http.Redirect(w, r, "https://elsewhere.example/api/v4"+path, http.StatusFound)
		return
	case f.oversized:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"pad":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
		return
	case f.badJSON:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id": 42,`)
		return
	case f.duplicate:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id": 42, "id": 43}`)
		return
	case f.wrongType:
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html></html>`)
		return
	}
	body, status := f.route(r, path)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if list, ok := body.([]map[string]any); ok {
		body = f.paginate(w, r, list)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// paginate serves one page of list and sets X-Next-Page when more remain, or
// always under the endless switch.
func (f *fakeGitLab) paginate(w http.ResponseWriter, r *http.Request, list []map[string]any) []map[string]any {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if f.pageSizeCap > 0 && size > f.pageSizeCap {
		size = f.pageSizeCap
	}
	if f.bigPage {
		out := []map[string]any{}
		for len(out) <= size {
			out = append(out, list...)
		}
		return out
	}
	start := min((page-1)*size, len(list))
	end := min(start+size, len(list))
	if f.endless || end < len(list) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	return append([]map[string]any{}, list[start:end]...)
}

var notFound = map[string]any{"message": "404 Not Found"}

// route answers one request from the canned data.
func (f *fakeGitLab) route(r *http.Request, path string) (any, int) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/v4/projects/"), "/")
	if !strings.HasPrefix(path, "/api/v4/projects/") || (parts[0] != projectKey && parts[0] != projectID) {
		return notFound, http.StatusNotFound
	}
	rest := parts[1:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		return f.project, http.StatusOK
	case len(rest) == 2 && rest[0] == "protected_branches":
		if f.protected[unescape(rest[1])] {
			return map[string]any{"name": unescape(rest[1]), "allow_force_push": false}, http.StatusOK
		}
		return notFound, http.StatusNotFound
	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodGet:
		branch, state := r.URL.Query().Get("source_branch"), r.URL.Query().Get("state")
		out := []map[string]any{}
		for _, mr := range f.mrs {
			if mr["source_branch"] == branch && (state == "" || mr["state"] == state) {
				out = append(out, mr)
			}
		}
		return out, http.StatusOK
	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodPost:
		if f.created != nil {
			return f.created, http.StatusCreated
		}
		var in map[string]any
		for _, s := range f.requests {
			in = s.Body
		}
		return f.createMR(in), http.StatusCreated
	case len(rest) >= 2 && rest[0] == "merge_requests":
		iid := rest[1]
		if len(rest) == 2 {
			for _, mr := range f.mrs {
				if strconv.Itoa(toInt(mr["iid"])) == iid {
					return mr, http.StatusOK
				}
			}
			return notFound, http.StatusNotFound
		}
		switch rest[2] {
		case "pipelines":
			return orEmpty(f.pipelines[iid]), http.StatusOK
		case "notes":
			return orEmpty(f.notes[iid]), http.StatusOK
		}
	case len(rest) == 3 && rest[0] == "pipelines" && rest[2] == "jobs":
		return orEmpty(f.jobs[rest[1]]), http.StatusOK
	}
	return notFound, http.StatusNotFound
}

func orEmpty(list []map[string]any) []map[string]any {
	if list == nil {
		return []map[string]any{}
	}
	return list
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func unescape(s string) string {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return out
}
