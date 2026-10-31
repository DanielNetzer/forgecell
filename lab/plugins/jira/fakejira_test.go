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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const (
	fakeEmail = "bot@example.com"
	fakeToken = "ATATT-faketoken0123456789abcdef"
	fakeSite  = "example.atlassian.net"
)

// seenRequest is one request the fake answered.
type seenRequest struct {
	Path          string
	Query         url.Values
	Authorization string
	User, Pass    string
	HasBasic      bool
}

// fakeJira is an in-process Jira Cloud REST v3 over TLS with canned JSON for
// every endpoint the plugin calls, plus failure switches.
type fakeJira struct {
	srv *httptest.Server

	mu sync.Mutex
	// issues are the canned issues by key, in creation order.
	issues []map[string]any
	// pageSize caps each search page below maxResults when set.
	pageSize int

	// Failure switches. status answers every request with that status and a
	// body naming the token.
	status    int
	oversized bool
	redirect  bool
	badJSON   bool
	duplicate bool
	wrongType bool
	hang      bool
	bigPage   bool
	// endless always names a fresh next page token; repeat names the same one
	// every time; badToken names one with a control character.
	endless  bool
	repeat   bool
	badToken bool
	// movedFrom answers a get of that key with the first issue, as Jira does
	// for a moved issue; extraKeys answers a key in (...) query with every
	// issue.
	movedFrom string
	extraKeys bool
	release   chan struct{}
	requests  []seenRequest
}

func newFakeJira(t *testing.T) *fakeJira {
	t.Helper()
	f := &fakeJira{release: make(chan struct{})}
	f.reset()
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// Registered after Close, so it runs first and frees a hung handler.
	t.Cleanup(func() { close(f.release) })
	return f
}

func issue(key, summary, category, resolution, updated string, labels []string, versions ...string) map[string]any {
	fv := []any{}
	for _, v := range versions {
		fv = append(fv, map[string]any{"id": "1", "name": v, "released": false})
	}
	fields := map[string]any{
		"summary": summary, "labels": labels, "updated": updated, "fixVersions": fv,
		"status":     map[string]any{"name": category + " status", "statusCategory": map[string]any{"key": category, "colorName": "blue"}},
		"resolution": nil, "resolutiondate": nil,
		"description": map[string]any{"type": "doc", "version": 1, "content": []any{
			map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Body of " + key}}},
		}},
	}
	if resolution != "" {
		fields["resolution"] = map[string]any{"name": resolution, "id": "10000"}
		fields["resolutiondate"] = "2026-10-29T08:00:00.000+0200"
	}
	return map[string]any{"id": "100", "self": "https://" + fakeSite + "/rest/api/3/issue/100", "key": key, "fields": fields}
}

// reset restores the canned data.
func (f *fakeJira) reset() {
	f.issues = []map[string]any{
		issue("ENG-1", "First issue", "new", "", "2026-10-30T10:00:00.000+0000", []string{"forgecell", "backend"}, "1.0", "1.1"),
		issue("ENG-2", "Second issue", "indeterminate", "", "2026-10-30T11:30:00.123-0130", []string{"forgecell"}),
		issue("ENG-3", "Third issue", "done", "Done", "2026-10-30T12:00:00.000+0000", []string{}),
		issue("ENG-4", "Fourth issue", "done", "Won't Do", "2026-10-30T13:00:00.000+0000", []string{"forgecell"}),
	}
	f.pageSize = 0
	f.status = 0
	f.oversized, f.redirect, f.badJSON, f.duplicate, f.wrongType, f.hang, f.bigPage = false, false, false, false, false, false, false
	f.endless, f.repeat, f.badToken = false, false, false
	f.movedFrom, f.extraKeys = "", false
	f.requests = nil
}

// set changes the fake under its lock.
func (f *fakeJira) set(change func(f *fakeJira)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeJira) seen() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.requests...)
}

// caFile writes the fake's TLS certificate as a PEM bundle in a fresh
// temporary directory and returns its path.
func (f *fakeJira) caFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(plugintest.TempDir(t), "jira-ca.pem")
	raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env is a needs.env lookup with credentials that trusts the fake.
func (f *fakeJira) env(t *testing.T) map[string]string {
	return map[string]string{emailEnv: fakeEmail, tokenEnv: fakeToken, caFileEnv: f.caFile(t)}
}

func (f *fakeJira) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	user, pass, basic := r.BasicAuth()
	f.requests = append(f.requests, seenRequest{Path: r.URL.EscapedPath(), Query: r.URL.Query(), Authorization: r.Header.Get("Authorization"), User: user, Pass: pass, HasBasic: basic})
	hang := f.hang
	f.mu.Unlock()
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
	case f.status != 0:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, `{"errorMessages":["denied for token `+fakeToken+`"]}`)
		return
	case f.redirect:
		http.Redirect(w, r, "https://elsewhere.example"+r.URL.Path, http.StatusFound)
		return
	case f.oversized:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"pad":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
		return
	case f.badJSON:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"issues": [`)
		return
	case f.duplicate:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"issues": [], "issues": []}`)
		return
	case f.wrongType:
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html></html>`)
		return
	}
	body, status := f.route(r)
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

var (
	notFound = map[string]any{"errorMessages": []string{"Issue does not exist or you do not have permission to see it."}}
	keysJQL  = regexp.MustCompile(`^key in \((.*)\)$`)
	quoted   = regexp.MustCompile(`"([^"]*)"`)
)

// route answers one request from the canned data.
func (f *fakeJira) route(r *http.Request) (any, int) {
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/rest/api/3/issue/"):
		key := strings.TrimPrefix(path, "/rest/api/3/issue/")
		if key == f.movedFrom {
			return f.issues[0], http.StatusOK
		}
		for _, is := range f.issues {
			if is["key"] == key {
				return is, http.StatusOK
			}
		}
		return notFound, http.StatusNotFound
	case path == "/rest/api/3/search/jql":
		return f.search(r), http.StatusOK
	}
	return notFound, http.StatusNotFound
}

// search answers a search/jql request: a key in (...) query lists those keys,
// any other query lists every issue not done. Pages are cut at maxResults and
// pageSize, with an opaque token naming the next offset.
func (f *fakeJira) search(r *http.Request) map[string]any {
	q := r.URL.Query()
	jql := q.Get("jql")
	var matched []map[string]any
	if m := keysJQL.FindStringSubmatch(jql); m != nil {
		want := map[string]bool{}
		for _, k := range quoted.FindAllStringSubmatch(m[1], -1) {
			want[k[1]] = true
		}
		for _, is := range f.issues {
			if key, _ := is["key"].(string); want[key] || f.extraKeys {
				matched = append(matched, is)
			}
		}
	} else {
		for _, is := range f.issues {
			if categoryOf(is) != "done" {
				matched = append(matched, is)
			}
		}
	}
	size, _ := strconv.Atoi(q.Get("maxResults"))
	if size < 1 {
		size = 50
	}
	if f.pageSize > 0 && size > f.pageSize {
		size = f.pageSize
	}
	start := 0
	if token := q.Get("nextPageToken"); token != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(token, "tok-"))
	}
	if f.bigPage {
		out := []map[string]any{}
		for len(out) <= size {
			out = append(out, f.issues...)
		}
		return map[string]any{"issues": out, "isLast": true}
	}
	start = min(start, len(matched))
	end := min(start+size, len(matched))
	out := map[string]any{"issues": append([]map[string]any{}, matched[start:end]...)}
	switch {
	case f.badToken:
		out["nextPageToken"] = "tok\n1"
	case f.repeat:
		out["nextPageToken"] = "tok-same"
	case f.endless:
		out["issues"] = []map[string]any{}
		out["nextPageToken"] = "tok-" + strconv.Itoa(start+1)
	case end < len(matched):
		out["nextPageToken"] = "tok-" + strconv.Itoa(end)
		out["isLast"] = false
	default:
		out["isLast"] = true
	}
	return out
}

// categoryOf reads an issue's status category, or "" when a test removed it.
func categoryOf(is map[string]any) string {
	fields, _ := is["fields"].(map[string]any)
	status, _ := fields["status"].(map[string]any)
	category, _ := status["statusCategory"].(map[string]any)
	key, _ := category["key"].(string)
	return key
}
