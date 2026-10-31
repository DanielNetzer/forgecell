package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	tokenEnv  = "GITLAB_TOKEN"
	urlEnv    = "GITLAB_URL"
	caFileEnv = "GITLAB_CA_FILE"

	defaultBaseURL = "https://gitlab.com"
	// requestTimeout bounds one HTTP request, body included; each tool's
	// contract timeout still bounds the whole call.
	requestTimeout = 20 * time.Second
	// maxResponseBytes bounds one response body; a larger one fails the call.
	maxResponseBytes = 4 << 20
	// maxPages bounds how many X-Next-Page links one listing follows; perPage
	// is the page size it asks for.
	maxPages = 5
	perPage  = 100
	// notePerPage and maxNotePages page notes in smaller pages, so one page
	// stays well under the response cap, while still reading up to 500 notes.
	notePerPage  = 20
	maxNotePages = 25
	// maxCAFileBytes bounds the GITLAB_CA_FILE bundle.
	maxCAFileBytes = 1 << 20
	// maxTokenBytes bounds GITLAB_TOKEN.
	maxTokenBytes = 1024
	userAgent     = "forgecell-gitlab/" + serverVersion
)

// hostPattern is the forge_describe host pattern: a lowercase host with an
// optional port.
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// config is the validated plugin configuration. Its errors are fixed text that
// never names a value read from the environment.
type config struct {
	// base is the GitLab origin, https://host[:port], with no path.
	base string
	host string
	// token is sent as PRIVATE-TOKEN when set.
	token string
	// roots is a copy of the system pool with any GITLAB_CA_FILE certificates
	// added.
	roots *x509.CertPool
	// timeout bounds one request; zero means requestTimeout.
	timeout time.Duration
}

// loadConfig reads GITLAB_TOKEN, GITLAB_URL and GITLAB_CA_FILE through lookup.
// An unset or empty GITLAB_URL means https://gitlab.com and an unset or empty
// GITLAB_CA_FILE means the system pool alone.
func loadConfig(lookup func(string) (string, bool)) (config, error) {
	var c config
	token, _ := lookup(tokenEnv)
	if len(token) > maxTokenBytes || strings.ContainsFunc(token, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return config{}, errors.New("GITLAB_TOKEN must be at most 1024 printable ASCII characters")
	}
	c.token = token
	raw, _ := lookup(urlEnv)
	if raw == "" {
		raw = defaultBaseURL
	}
	base, host, err := parseBaseURL(raw)
	if err != nil {
		return config{}, err
	}
	c.base, c.host = base, host
	system, err := x509.SystemCertPool()
	if err != nil || system == nil {
		return config{}, errors.New("the system certificate pool is unavailable")
	}
	// SystemCertPool returns a copy, so adding to it never changes the pool
	// other callers see; the system roots are kept, never replaced.
	c.roots = system
	if path, _ := lookup(caFileEnv); path != "" {
		certs, err := readCAFile(path)
		if err != nil {
			return config{}, err
		}
		for _, cert := range certs {
			c.roots.AddCert(cert)
		}
	}
	return c, nil
}

// parseBaseURL accepts only an https origin with a lowercase host, an optional
// port, no credentials and no path other than /, query or fragment. Plain http
// is refused for every host, loopback included.
func parseBaseURL(raw string) (base, host string, err error) {
	refuse := errors.New("GITLAB_URL must be an https URL with a lowercase host, an optional port and no credentials, path, query or fragment")
	if len(raw) > 2048 || strings.ContainsAny(raw, "#? \t\r\n\\") {
		return "", "", refuse
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", refuse
	}
	if len(u.Host) > 253 || !hostPattern.MatchString(u.Host) || strings.Contains(u.Host, "..") {
		return "", "", refuse
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", "", refuse
		}
	}
	return "https://" + u.Host, u.Host, nil
}

// readCAFile reads a PEM bundle of at most 1 MiB from a clean absolute path that
// passes the pinned placement checks (no symlink, owned by root or the current
// user, not writable by others) through a handle proven to be the checked file.
// Every PEM block must be a certificate that parses, and there must be at least
// one.
func readCAFile(path string) ([]*x509.Certificate, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("GITLAB_CA_FILE must be a clean absolute path")
	}
	f, err := pinned.Open(path)
	if err != nil {
		return nil, errors.New("GITLAB_CA_FILE is missing, unreadable or unsafely placed")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > maxCAFileBytes {
		return nil, errors.New("GITLAB_CA_FILE exceeds 1 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileBytes+1))
	if err != nil {
		return nil, errors.New("GITLAB_CA_FILE is unreadable")
	}
	if len(raw) > maxCAFileBytes {
		return nil, errors.New("GITLAB_CA_FILE exceeds 1 MiB")
	}
	var certs []*x509.Certificate
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("GITLAB_CA_FILE holds a PEM block that is not a certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("GITLAB_CA_FILE holds a certificate that does not parse")
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("GITLAB_CA_FILE holds no certificate")
	}
	return certs, nil
}

// client is the one shared GitLab REST v4 client.
type client struct {
	api   string
	host  string
	token string
	http  *http.Client
}

func newClient(c config) *client {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	transport := &http.Transport{
		// The plugin environment is scrubbed; no proxy is ever read from it.
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           4,
		IdleConnTimeout:        90 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	return &client{
		api:   c.base + "/api/v4",
		host:  c.host,
		token: c.token,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect is never followed: the 3xx itself fails the call, so
			// the token is never sent anywhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// statusError is a GitLab answer outside 2xx. It carries only the status,
// never the body.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("GitLab answered HTTP %d", e.code) }

// isStatus reports whether err is a GitLab answer with the given status.
func isStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == code
}

// fail maps a request error to a tool failure with a fixed reason: 401, or 403
// with no token, is auth-missing, 429 is rate-limited and every other status is
// failed. The message names the status only. Other errors are already tool
// failures.
func (c *client) fail(err error) error {
	var se *statusError
	if !errors.As(err, &se) {
		var fe *mcpserver.Error
		if errors.As(err, &fe) {
			return err
		}
		return mcpserver.Fail(mcpserver.Failed, "the GitLab request failed")
	}
	switch {
	case se.code == http.StatusUnauthorized, se.code == http.StatusForbidden && c.token == "":
		return mcpserver.Fail(mcpserver.AuthMissing, se.Error())
	case se.code == http.StatusTooManyRequests:
		return mcpserver.Fail(mcpserver.RateLimited, se.Error())
	}
	return mcpserver.Fail(mcpserver.Failed, se.Error())
}

// do sends one request to path under /api/v4 (already escaped) and decodes a
// 2xx JSON answer into out. It returns the answer's headers.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
	target := c.api + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, "the request cannot be encoded")
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil || req.URL.Scheme != "https" {
		return nil, mcpserver.Fail(mcpserver.Failed, "the request cannot be built")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors, TLS refusals and timeouts all name no detail: the
		// error text may carry the URL.
		return nil, mcpserver.Fail(mcpserver.Network, "GitLab could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{code: resp.StatusCode}
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return nil, mcpserver.Fail(mcpserver.Failed, "GitLab answered with a body that is not JSON")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, mcpserver.Fail(mcpserver.Network, "the GitLab response could not be read")
	}
	if len(raw) > maxResponseBytes {
		return nil, mcpserver.Fail(mcpserver.Failed, "the GitLab response exceeds 4 MiB")
	}
	if err := decodeStrict(raw, out); err != nil {
		return nil, mcpserver.Fail(mcpserver.Failed, "GitLab sent a malformed response")
	}
	return resp.Header, nil
}

func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.do(ctx, http.MethodGet, path, query, nil, out)
	return err
}

// decodeStrict decodes exactly one JSON value with no duplicate keys and no
// trailing data into out.
func decodeStrict(raw []byte, out any) error {
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// listPages follows X-Next-Page from the first page for at most maxPages pages
// and returns every item. more is true when GitLab still names a next page
// past the bound, so the caller never mistakes a cut listing for a whole one.
func listPages[T any](ctx context.Context, c *client, path string, query url.Values) (items []T, more bool, err error) {
	return listPagesOf[T](ctx, c, path, query, perPage, maxPages)
}

// listPagesOf is listPages with its own page size and page bound.
func listPagesOf[T any](ctx context.Context, c *client, path string, query url.Values, perPage, maxPages int) (items []T, more bool, err error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(perPage))
	page := 1
	for n := 0; n < maxPages; n++ {
		q.Set("page", strconv.Itoa(page))
		var batch []T
		header, err := c.do(ctx, http.MethodGet, path, q, nil, &batch)
		if err != nil {
			return nil, false, err
		}
		if len(batch) > perPage {
			return nil, false, mcpserver.Fail(mcpserver.Failed, "GitLab sent a page larger than the page size asked for")
		}
		items = append(items, batch...)
		next := strings.TrimSpace(header.Get("X-Next-Page"))
		if next == "" {
			return items, false, nil
		}
		nextPage, err := strconv.Atoi(next)
		if err != nil || nextPage <= page || nextPage > 1_000_000 {
			return nil, false, mcpserver.Fail(mcpserver.Failed, "GitLab sent a malformed X-Next-Page header")
		}
		page = nextPage
	}
	return items, true, nil
}
