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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	emailEnv  = "JIRA_EMAIL"
	tokenEnv  = "JIRA_API_TOKEN"
	caFileEnv = "JIRA_CA_FILE"

	// requestTimeout bounds one HTTP request, body included; each tool's
	// contract timeout still bounds the whole call.
	requestTimeout = 20 * time.Second
	// maxResponseBytes bounds one response body; a larger one fails the call.
	maxResponseBytes = 4 << 20
	// maxPages bounds how many nextPageToken links one call follows.
	maxPages = 5
	// maxCAFileBytes bounds the JIRA_CA_FILE bundle.
	maxCAFileBytes = 1 << 20
	// maxTokenBytes and maxEmailBytes bound JIRA_API_TOKEN and JIRA_EMAIL.
	maxTokenBytes = 1024
	maxEmailBytes = 254
	// maxCursorRunes is the contract bound of a cursor.
	maxCursorRunes = 1024
	userAgent      = "forgecell-jira/" + serverVersion
)

// config is the validated environment. Its errors are fixed text that never
// names a value read from the environment.
type config struct {
	// email and token are sent as HTTP Basic auth when both are set.
	email string
	token string
	// roots is a copy of the system pool with any JIRA_CA_FILE certificates
	// added; customCA records that the file was set.
	roots    *x509.CertPool
	customCA bool
	// timeout bounds one request; zero means requestTimeout.
	timeout time.Duration
}

// printable reports whether s holds only printable ASCII with no space.
func printable(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x21 || r > 0x7e })
}

// loadConfig reads JIRA_EMAIL, JIRA_API_TOKEN and JIRA_CA_FILE through lookup.
// The email and the token are set together or not at all; with neither set,
// requests are anonymous. An unset or empty JIRA_CA_FILE means the system pool
// alone.
func loadConfig(lookup func(string) (string, bool)) (config, error) {
	var c config
	email, _ := lookup(emailEnv)
	token, _ := lookup(tokenEnv)
	if len(email) > maxEmailBytes || !printable(email) || (email != "" && !strings.Contains(email, "@")) {
		return config{}, errors.New("JIRA_EMAIL must be an email address of at most 254 printable ASCII characters")
	}
	if len(token) > maxTokenBytes || !printable(token) {
		return config{}, errors.New("JIRA_API_TOKEN must be at most 1024 printable ASCII characters")
	}
	if (email == "") != (token == "") {
		return config{}, errors.New("JIRA_EMAIL and JIRA_API_TOKEN must be set together")
	}
	c.email, c.token = email, token
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
		c.customCA = true
	}
	return c, nil
}

// readCAFile reads a PEM bundle of at most 1 MiB from a clean absolute path that
// passes the pinned placement checks (no symlink, owned by root or the current
// user, not writable by others) through a handle proven to be the checked file.
// Every PEM block must be a certificate that parses, and there must be at least
// one.
func readCAFile(path string) ([]*x509.Certificate, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("JIRA_CA_FILE must be a clean absolute path")
	}
	f, err := pinned.Open(path)
	if err != nil {
		return nil, errors.New("JIRA_CA_FILE is missing, unreadable or unsafely placed")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > maxCAFileBytes {
		return nil, errors.New("JIRA_CA_FILE exceeds 1 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileBytes+1))
	if err != nil {
		return nil, errors.New("JIRA_CA_FILE is unreadable")
	}
	if len(raw) > maxCAFileBytes {
		return nil, errors.New("JIRA_CA_FILE exceeds 1 MiB")
	}
	var certs []*x509.Certificate
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("JIRA_CA_FILE holds a PEM block that is not a certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("JIRA_CA_FILE holds a certificate that does not parse")
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("JIRA_CA_FILE holds no certificate")
	}
	return certs, nil
}

// client is the one shared Jira Cloud REST v3 client. The site comes from each
// call's reviewed config, so the client holds no origin of its own.
type client struct {
	email string
	token string
	http  *http.Client
	// origin, when set, replaces https://<site> as the request origin. Only
	// in-package tests set it, to reach a fake Jira; no env name or config key
	// reaches it.
	origin string
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
		email: c.email,
		token: c.token,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect is never followed: the 3xx itself fails the call, so
			// the credentials are never sent anywhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// statusError is a Jira answer outside 2xx. It carries only the status, never
// the body.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("Jira answered HTTP %d", e.code) }

// fail maps a request error to a tool failure with a fixed reason: 401 is
// auth-missing, 429 is rate-limited and every other status, 403 included, is
// failed. The message names the status only. Other errors are already tool
// failures.
func fail(err error) error {
	var se *statusError
	if !errors.As(err, &se) {
		var fe *mcpserver.Error
		if errors.As(err, &fe) {
			return err
		}
		return mcpserver.Fail(mcpserver.Failed, "the Jira request failed")
	}
	switch se.code {
	case http.StatusUnauthorized:
		return mcpserver.Fail(mcpserver.AuthMissing, se.Error())
	case http.StatusTooManyRequests:
		return mcpserver.Fail(mcpserver.RateLimited, se.Error())
	}
	return mcpserver.Fail(mcpserver.Failed, se.Error())
}

// get sends one GET to path under /rest/api/3 (already escaped) on site and
// decodes a 2xx JSON answer into out. Every failure is a tool failure.
func (c *client) get(ctx context.Context, site, path string, query url.Values, out any) error {
	origin := "https://" + site
	if c.origin != "" {
		origin = c.origin
	}
	target := origin + "/rest/api/3" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil || req.URL.Scheme != "https" {
		return mcpserver.Fail(mcpserver.Failed, "the request cannot be built")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.token != "" {
		req.SetBasicAuth(c.email, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors, TLS refusals and timeouts all name no detail: the
		// error text may carry the URL.
		return mcpserver.Fail(mcpserver.Network, "Jira could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(&statusError{code: resp.StatusCode})
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return mcpserver.Fail(mcpserver.Failed, "Jira answered with a body that is not JSON")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return mcpserver.Fail(mcpserver.Network, "the Jira response could not be read")
	}
	if len(raw) > maxResponseBytes {
		return mcpserver.Fail(mcpserver.Failed, "the Jira response exceeds 4 MiB")
	}
	if err := decodeStrict(raw, out); err != nil {
		return mcpserver.Fail(mcpserver.Failed, "Jira sent a malformed response")
	}
	return nil
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

// searchPage is one /rest/api/3/search/jql answer.
type searchPage struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken *string     `json:"nextPageToken"`
	IsLast        *bool       `json:"isLast"`
}

// validToken accepts a page token the contract's cursor format accepts.
func validToken(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxCursorRunes && strings.TrimSpace(s) == s &&
		!strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 || r > 0x7e })
}

// search follows nextPageToken from cursor for at most maxPages pages, asking
// each page for the items still wanted, and stops once want items are read.
// next is the token to resume from, empty when Jira has no more. A token Jira
// repeats, the request's own cursor included, is refused, so paging never
// loops.
func (c *client) search(ctx context.Context, site, jql, fields string, want int, cursor string) (issues []jiraIssue, next string, err error) {
	seen := map[string]bool{}
	if cursor != "" {
		seen[cursor] = true
	}
	token := cursor
	for n := 0; n < maxPages && len(issues) < want; n++ {
		ask := want - len(issues)
		q := url.Values{"jql": {jql}, "fields": {fields}, "maxResults": {strconv.Itoa(ask)}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var page searchPage
		if err := c.get(ctx, site, "/search/jql", q, &page); err != nil {
			return nil, "", err
		}
		if len(page.Issues) > ask {
			return nil, "", mcpserver.Fail(mcpserver.Failed, "Jira sent a page larger than the page size asked for")
		}
		issues = append(issues, page.Issues...)
		token = ""
		if page.NextPageToken != nil && (page.IsLast == nil || !*page.IsLast) {
			token = *page.NextPageToken
			if !validToken(token) {
				return nil, "", mcpserver.Fail(mcpserver.Failed, "Jira sent a malformed nextPageToken")
			}
			if seen[token] {
				return nil, "", mcpserver.Fail(mcpserver.Failed, "Jira repeated a nextPageToken")
			}
			seen[token] = true
		}
		if token == "" {
			return issues, "", nil
		}
	}
	return issues, token, nil
}
