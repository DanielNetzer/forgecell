package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	// maxTextChunk is the most file text one evidence or ledger response carries.
	maxTextChunk = 256 << 10
	// secretOverlap is read past the chunk so a secret straddling the cut is
	// found whole and never partly served.
	secretOverlap = 4 << 10
	// maxEvidenceHash bounds the bytes hashed for one evidence request; a larger
	// file is refused, never served as matching.
	maxEvidenceHash = 64 << 20
	// maxEvidencePath bounds the requested path.
	maxEvidencePath = 1024
)

// Refusal categories; the handler maps each to a status and a message that
// names no local path.
var (
	errUnsafeFile    = errors.New("unsafe or unreadable file")
	errSHAMismatch   = errors.New("file does not match its recorded SHA-256")
	errEvidenceLarge = errors.New("file exceeds the hash bound")
)

// TextView is GET /api/molecules/{id}/evidence and /ledger: a bounded, redacted
// chunk of one Lab file, served as JSON text and never rendered as markup.
type TextView struct {
	// Path is Lab-relative.
	Path string `json:"path"`
	// SHA256 is set for evidence, whose full content matched it.
	SHA256 string `json:"sha256,omitempty"`
	// Size is the file's size in bytes; Truncated says Text is only its start.
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text"`
}

// validEvidencePath accepts a clean, Lab-relative, slash-separated path only, so
// a requested path never traverses and is compared with the ledger exactly.
func validEvidencePath(p string) bool {
	return p != "" && len(p) <= maxEvidencePath && utf8.ValidString(p) && !strings.ContainsAny(p, "\x00\\") && !filepath.IsAbs(p) && filepath.IsLocal(p) && filepath.ToSlash(filepath.Clean(p)) == p
}

func ownedByUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// openLabFile opens rel below lab for reading. Every component below the Lab is
// checked with Lstat: it must not be a symlink, must be owned by the current
// user, and must be a directory or, last, a regular file. The open is confined
// to the Lab, does not follow a final symlink or block on a FIFO, and must yield
// the file that was checked; an ancestor replaced meanwhile is refused too.
func openLabFile(lab, rel string) (*os.File, os.FileInfo, error) {
	if !validEvidencePath(rel) {
		return nil, nil, errUnsafeFile
	}
	parts := strings.Split(rel, "/")
	dirs := make([]os.FileInfo, 0, len(parts)-1)
	var want os.FileInfo
	at := lab
	for i, part := range parts {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fs.ErrNotExist
		}
		last := i == len(parts)-1
		if err != nil || info.Mode()&fs.ModeSymlink != 0 || !ownedByUser(info) || (!last && !info.IsDir()) || (last && !info.Mode().IsRegular()) {
			return nil, nil, errUnsafeFile
		}
		if last {
			want = info
		} else {
			dirs = append(dirs, info)
		}
	}
	root, err := os.OpenRoot(lab)
	if err != nil {
		return nil, nil, errUnsafeFile
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errUnsafeFile
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || !os.SameFile(want, stat) || !ownedByUser(stat) {
		f.Close()
		return nil, nil, errUnsafeFile
	}
	at = lab
	for i, part := range parts[:len(parts)-1] {
		at = filepath.Join(at, part)
		if info, err := os.Lstat(at); err != nil || info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(dirs[i], info) {
			f.Close()
			return nil, nil, errUnsafeFile
		}
	}
	return f, stat, nil
}

// headWriter keeps the first limit bytes written to it and discards the rest.
type headWriter struct {
	buf   []byte
	limit int
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// readEvidence serves rel only when its full content, hashed within
// maxEvidenceHash, matches one of the recorded digests; the text is the
// redacted start of the file.
func readEvidence(lab, rel string, sums map[string]bool, x redactor) (TextView, error) {
	f, info, err := openLabFile(lab, rel)
	if err != nil {
		return TextView{}, err
	}
	defer f.Close()
	if info.Size() > maxEvidenceHash {
		return TextView{}, errEvidenceLarge
	}
	h := sha256.New()
	head := &headWriter{limit: maxTextChunk + secretOverlap}
	n, err := io.Copy(io.MultiWriter(h, head), io.LimitReader(f, maxEvidenceHash+1))
	if err != nil {
		return TextView{}, errUnsafeFile
	}
	if n > maxEvidenceHash {
		return TextView{}, errEvidenceLarge
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !sums[sum] {
		return TextView{}, errSHAMismatch
	}
	return chunk(rel, sum, n, head.buf, x), nil
}

// readLedgerText serves the start of the Molecule's ledger .md, read with the
// same checks as evidence. The .md is not hashed in the ledger, so only its
// first chunk is read.
func readLedgerText(lab, id string, x redactor) (TextView, error) {
	rel := "ledgers/" + id + ".md"
	f, info, err := openLabFile(lab, rel)
	if err != nil {
		return TextView{}, err
	}
	defer f.Close()
	head := &headWriter{limit: maxTextChunk + secretOverlap}
	n, err := io.Copy(head, io.LimitReader(f, maxTextChunk+secretOverlap))
	if err != nil {
		return TextView{}, errUnsafeFile
	}
	return chunk(rel, "", max(info.Size(), n), head.buf, x), nil
}

// chunk cuts head to maxTextChunk after finding secrets in all of it, then
// replaces local paths. Truncated is set whenever the file is larger than the
// chunk, so a cut is never silent.
func chunk(rel, sum string, size int64, head []byte, x redactor) TextView {
	text := cutRedacted(string(head), maxTextChunk)
	return TextView{Path: rel, SHA256: sum, Size: size, Truncated: size > maxTextChunk || len(head) > maxTextChunk, Text: strings.ToValidUTF8(x.text(text), "�")}
}
