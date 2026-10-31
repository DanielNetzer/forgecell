package mcpserver

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Reason is one fixed tool failure a handler may name. The engine's client
// maps any other reason to failed, so the kit does the same.
type Reason string

const (
	AuthMissing Reason = "auth-missing"
	RateLimited Reason = "rate-limited"
	Network     Reason = "network"
	Failed      Reason = "failed"
)

const (
	// maxMessageRunes bounds the message a Fail error carries back to the
	// client.
	maxMessageRunes = 200
	// minSecretLength is the shortest env value treated as a secret, as the
	// engine's launcher does; shorter values would redact ordinary words.
	minSecretLength = 4
	// maxScanBytes bounds the text sanitize scans for secrets.
	maxScanBytes = 64 << 10
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Error is a handler failure with a fixed reason and a raw message capped at
// the scan bound. The message is redacted and cut only when it is sent or
// logged, so a secret is always found whole before any cut.
type Error struct {
	reason  Reason
	message string
}

// Error returns the reason and the message with its tokens and authorization
// headers redacted, cut to a bounded length. It does not know the secret env
// names a Server declares, so the Server never logs or sends it.
func (e *Error) Error() string {
	msg := sanitize(e.message, maxMessageRunes)
	if msg == "" {
		return string(e.reason)
	}
	return string(e.reason) + ": " + msg
}

// Reason returns the fixed reason.
func (e *Error) Reason() Reason { return e.reason }

// Fail builds a handler error with a fixed reason. A reason other than
// auth-missing, rate-limited or network becomes failed. Only the raw message is
// kept, capped at the scan bound; the Server redacts every token, authorization
// header and declared secret env value in it, strips control and format
// characters and only then cuts it to a bounded length.
func Fail(reason Reason, msg string) error {
	switch reason {
	case AuthMissing, RateLimited, Network:
	default:
		reason = Failed
	}
	return &Error{reason: reason, message: capScan(msg)}
}

// reasonOf returns the fixed reason of a handler error and its raw message.
// Any error that is not a Fail error is failed with no message, so arbitrary
// error text never reaches the client.
func reasonOf(err error) (Reason, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.reason, e.message
	}
	return Failed, ""
}

// capScan cuts s to at most maxScanBytes, backed off to a rune start.
func capScan(s string) string {
	if len(s) <= maxScanBytes {
		return s
	}
	n := maxScanBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// sanitize bounds s to limit runes of one line with every secret redacted. It
// cleans s, does one redaction over the whole cleaned text and only then cuts,
// so a secret straddling the cut is replaced whole, never partly served. Only
// the first maxScanBytes are scanned, so hostile input costs bounded work. A
// secret may straddle that scan cap; text cut there is served only when at
// least half the scanned bytes survive cleaning, which keeps the cap so far past
// the served limit that no secret reaches from the cap into it. Otherwise, for
// text padded with characters cleaning drops, the message is dropped.
func sanitize(s string, limit int, secretEnv ...string) string {
	capped := len(s) >= maxScanBytes
	s = safetext.Text(capScan(s), maxScanBytes)
	if capped && utf8.RuneCountInString(s) < maxScanBytes/2 {
		return ""
	}
	return safetext.Text(Redact(s, secretEnv...), limit)
}

// Redact replaces every token or authorization header in s, and every value of
// the named env variables (the ones a plugin declares secret), with
// "[redacted]". A value is replaced both as set and as cleaned display text, so
// it is found in cleaned messages too. Values shorter than four bytes are left
// alone.
func Redact(s string, secretEnv ...string) string {
	for _, name := range secretEnv {
		value, ok := Env(name)
		if !ok {
			continue
		}
		for _, v := range []string{value, safetext.Text(value, len(value))} {
			if len(v) >= minSecretLength {
				s = strings.ReplaceAll(s, v, safetext.SecretMark)
			}
		}
	}
	return safetext.RedactSecrets(s)
}

// Env reads only the named environment variable, for a plugin's needs.env. An
// invalid name is never looked up.
func Env(name string) (string, bool) {
	if !envName.MatchString(name) {
		return "", false
	}
	return os.LookupEnv(name)
}
