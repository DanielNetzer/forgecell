package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Bounds shared by every ledger directory scan. Memory is bounded by streaming
// and work by the entry, file and byte caps. A Molecule's companion files (.md
// reports, .lock approval locks and .loop-lock advisory locks) count only toward
// MaxScanRawEntries, so they never crowd ledgers out of MaxScanEntries.
const (
	MaxScanEntries    = 4096
	MaxScanRawEntries = 65536
	MaxScanFiles      = 4096
	MaxScanBytes      = 1_000_000_000
	MaxHeaderLedger   = 5_000_000
)

// Header is the small, bounded part of a ledger that inventories need. A molecule
// Record marshals these fields first, so ReadHeader stops reading as soon as it
// has them; a ledger saved in the older order is streamed to its end instead.
type Header struct {
	SchemaVersion string
	Kind          string
	ID            string
	FormulaID     string
	Status        string
	Mode          string
	FinishedAt    string
	LabDir        string
	QueueProjection
}

// ProjectionError reports a ledger whose identity is valid but whose optional
// queue projection (issue, workspace, startedAt, readiness, mode, finishedAt or
// labDir) has the wrong JSON type. The returned Header still carries its identity.
type ProjectionError struct {
	Field string
	Err   error
}

func (e *ProjectionError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *ProjectionError) Unwrap() error { return e.Err }

const (
	fieldSchemaVersion = iota
	fieldKind
	fieldID
	fieldFormulaID
	fieldStatus
	fieldMode
	fieldStartedAt
	fieldFinishedAt
	fieldLabDir
	fieldIssue
	fieldWorkspace
	fieldReadiness
)

// headerNames is indexed by the field constants above.
var headerNames = []string{"schemaVersion", "kind", "id", "formulaId", "status", "mode", "startedAt", "finishedAt", "labDir", "issue", "workspace", "readiness"}

// Every header field except readiness, which is optional and comes last.
const requiredFields = 1<<fieldReadiness - 1

func headerField(key string) int {
	// encoding/json matches keys case-insensitively; so does Decode.
	for i, name := range headerNames {
		if strings.EqualFold(key, name) {
			return i
		}
	}
	return -1
}

// ReadHeader decodes a ledger's header from a stream with the same identity rules
// as Decode. Other values are skipped token by token, never buffered whole. Once
// every header field but readiness has been seen, the first readiness phase or
// the first non-header key ends the decode; the rest of the stream, at most
// MaxHeaderLedger bytes, is then only checked to complete one JSON object, so a
// truncated, corrupt or trailing-garbage ledger is still refused as Decode
// refuses it. A ledger that lacks some header fields is decoded to its end.
func ReadHeader(r io.Reader) (Header, error) {
	h, _, err := readHeader(r)
	return h, err
}

// Prefixes that make the unread rest of a ledger a standalone JSON document
// again, for each place the header decode can stop.
const (
	restAfterKey       = `{""`
	restAfterValue     = `{"":0`
	restAfterReadiness = `{"":{"":0`
)

// readHeader also returns how many bytes the header decode consumed before the
// rest of the stream was only validated.
func readHeader(r io.Reader) (Header, int64, error) {
	var h Header
	dec := json.NewDecoder(r)
	dec.UseNumber()
	// finish validates the header and then the rest of the stream after prefix.
	finish := func(prefix string, projection error) (Header, int64, error) {
		offset := dec.InputOffset()
		got, err := h.validate(projection)
		if err != nil && !errors.As(err, new(*ProjectionError)) {
			return Header{}, offset, err
		}
		if e := validRest(prefix, dec.Buffered(), r); e != nil {
			return Header{}, offset, e
		}
		return got, offset, err
	}
	tok, err := dec.Token()
	if err != nil {
		return Header{}, dec.InputOffset(), fmt.Errorf("ledger must be a JSON object: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return Header{}, dec.InputOffset(), fmt.Errorf("ledger must be a JSON object")
	}
	seen := 0
	var projection error
	project := func(field int, err error) error {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return err
		}
		if projection == nil {
			projection = &ProjectionError{Field: headerNames[field], Err: err}
		}
		return nil
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Header{}, dec.InputOffset(), err
		}
		key, _ := tok.(string)
		field := headerField(key)
		if field < 0 {
			if seen&requiredFields == requiredFields {
				return finish(restAfterKey, projection)
			}
			if err := skipValue(dec); err != nil {
				return Header{}, dec.InputOffset(), err
			}
			continue
		}
		var target any
		switch field {
		case fieldSchemaVersion:
			target = &h.SchemaVersion
		case fieldKind:
			target = &h.Kind
		case fieldID:
			target = &h.ID
		case fieldFormulaID:
			target = &h.FormulaID
		case fieldStatus:
			target = &h.Status
		case fieldMode:
			target = &h.Mode
		case fieldStartedAt:
			target = &h.StartedAt
		case fieldFinishedAt:
			target = &h.FinishedAt
		case fieldLabDir:
			target = &h.LabDir
		case fieldIssue:
			target = &h.Issue
		case fieldWorkspace:
			target = &h.Workspace
		}
		if field == fieldReadiness {
			phase, inside, err := readReadiness(dec, &h, seen&requiredFields == requiredFields)
			if err = project(field, err); err != nil {
				return Header{}, dec.InputOffset(), err
			}
			seen |= 1 << field
			if phase && seen&requiredFields == requiredFields {
				if inside {
					return finish(restAfterReadiness, projection)
				}
				return finish(restAfterValue, projection)
			}
			continue
		}
		if err := dec.Decode(target); err != nil {
			if field <= fieldStatus {
				return Header{}, dec.InputOffset(), err
			}
			if err = project(field, err); err != nil {
				return Header{}, dec.InputOffset(), err
			}
		}
		seen |= 1 << field
		if seen == 1<<len(headerNames)-1 {
			return finish(restAfterValue, projection)
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return Header{}, dec.InputOffset(), fmt.Errorf("ledger must be a JSON object: %v", err)
	}
	offset := dec.InputOffset()
	if _, err := dec.Token(); err != io.EOF {
		return Header{}, offset, fmt.Errorf("ledger must be a single JSON object")
	}
	h, err = h.validate(projection)
	return h, offset, err
}

// restPool holds the buffers that validate the rest of a ledger after its header,
// so a scan reuses one buffer of at most MaxHeaderLedger bytes.
var restPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// validRest checks that prefix followed by the unread rest of a ledger (what the
// decoder buffered, then r) is exactly one valid JSON document.
func validRest(prefix string, buffered, r io.Reader) error {
	b := restPool.Get().(*bytes.Buffer)
	defer restPool.Put(b)
	b.Reset()
	b.WriteString(prefix)
	n, err := b.ReadFrom(io.LimitReader(io.MultiReader(buffered, r), MaxHeaderLedger+1))
	if err != nil {
		return err
	}
	if n > MaxHeaderLedger {
		return fmt.Errorf("ledger exceeds %d bytes", MaxHeaderLedger)
	}
	if !json.Valid(b.Bytes()) {
		return fmt.Errorf("ledger is not a single valid JSON object after its header")
	}
	return nil
}

// readReadiness streams the readiness object, keeping only its phase. With stop
// set it returns as soon as the phase is read, leaving the stream mid-object and
// reporting inside.
func readReadiness(dec *json.Decoder, h *Header, stop bool) (phase, inside bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return false, false, err
	}
	if tok == nil {
		h.Readiness = nil
		return false, false, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		if ok {
			if err := skipRest(dec, 1); err != nil {
				return false, false, err
			}
		}
		return false, false, &json.UnmarshalTypeError{Value: "non-object", Field: "readiness"}
	}
	if h.Readiness == nil {
		h.Readiness = &ReadinessProjection{}
	}
	var typeErr error
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false, false, err
		}
		if key, _ := tok.(string); !strings.EqualFold(key, "phase") {
			if err := skipValue(dec); err != nil {
				return false, false, err
			}
			continue
		}
		if err := dec.Decode(&h.Readiness.Phase); err != nil {
			var te *json.UnmarshalTypeError
			if !errors.As(err, &te) {
				return false, false, err
			}
			typeErr = err
		}
		phase = true
		if stop && typeErr == nil {
			return true, true, nil
		}
	}
	if _, err := dec.Token(); err != nil {
		return false, false, err
	}
	return phase, false, typeErr
}

// skipValue discards one value token by token, so a large array or object costs
// no more memory than its largest scalar.
func skipValue(dec *json.Decoder) error { return skipRest(dec, 0) }

func skipRest(dec *json.Decoder, depth int) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func (h Header) validate(projection error) (Header, error) {
	if h.SchemaVersion != "v0" || h.Kind != "molecule" {
		return Header{}, fmt.Errorf("unsupported ledger schema or kind")
	}
	if !idPattern.MatchString(h.ID) || h.FormulaID == "" || h.Status == "" {
		return Header{}, fmt.Errorf("ledger identity, Formula and status are required")
	}
	if projection != nil {
		return h, projection
	}
	return h, nil
}

// ProblemKind classifies why a ledger in a scan has no usable header.
type ProblemKind int

const (
	ProblemNone ProblemKind = iota
	// ProblemUnreadable: the file could not be listed, opened or read.
	ProblemUnreadable
	// ProblemUnsafe: a symlink, a non-regular file or a file replaced while opened.
	ProblemUnsafe
	// ProblemOversized: the file exceeds, or grew beyond, MaxHeaderLedger bytes.
	ProblemOversized
	// ProblemMalformed: not a JSON object, or an unsupported or invalid header.
	ProblemMalformed
	// ProblemIdentity: the ledger id does not match its file name.
	ProblemIdentity
	// ProblemProjection: a valid identity with a malformed queue projection; Header
	// carries the identity.
	ProblemProjection
)

// ScanFile is one ledger JSON file in a scan. Problem names the file.
// HeaderBytes is how far the header decode read before the rest of the file was
// only validated as JSON: a few KB for a ledger saved header first.
type ScanFile struct {
	Name        string
	Header      Header
	Kind        ProblemKind
	Problem     string
	HeaderBytes int64
}

// Scan is a ledger directory's headers in file-name order. A non-empty Problem is
// a directory-level problem after which no further file was read; Truncated
// marks it as a scan bound rather than an unsafe or unreadable directory.
type Scan struct {
	Missing   bool
	Problem   string
	Truncated bool
	Files     []ScanFile
}

type ScanOptions struct {
	// MaxFiles bounds the JSON files read; zero means MaxScanFiles.
	MaxFiles int
	// MaxBytes bounds the summed size of the ledgers scanned; zero means
	// MaxScanBytes.
	MaxBytes int64
	// Cache, when set, reuses headers of files whose identity is unchanged.
	Cache *HeaderCache
	// Wrap, when set, wraps each opened ledger's reader (tests count bytes read).
	Wrap func(name string, r io.Reader) io.Reader
}

// companion reports a Molecule's non-ledger files in ledgers/: its Markdown
// report, its approval lock and the loop's advisory locks. They count only toward
// MaxScanRawEntries.
func companion(name string) bool {
	return strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".loop-lock")
}

// HeaderCache keeps scan results keyed by a file's device, inode, modification
// time and size, so repeated scans in one process, such as the loop's re-read
// under an Issue lock, do not re-read unchanged ledgers. A file modified within
// racyWindow of being read is never cached, since a coarse clock could give a
// later write the same modification time. It keeps only the files its latest
// scan of each directory saw.
type HeaderCache struct {
	mu   sync.Mutex
	dirs map[string]map[cacheKey]ScanFile
}

type cacheKey struct {
	name        string
	dev, ino    uint64
	mtime, size int64
}

const (
	racyWindow     = 2 * time.Second
	maxCachedDirs  = 64
	cacheableKinds = 1<<ProblemNone | 1<<ProblemMalformed | 1<<ProblemIdentity | 1<<ProblemProjection
)

func NewHeaderCache() *HeaderCache { return &HeaderCache{dirs: map[string]map[cacheKey]ScanFile{}} }

func (c *HeaderCache) get(dir string, key cacheKey) (ScanFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.dirs[dir][key]
	return f, ok
}

func (c *HeaderCache) replace(dir string, entries map[cacheKey]ScanFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.dirs[dir]; !ok && len(c.dirs) >= maxCachedDirs {
		c.dirs = map[string]map[cacheKey]ScanFile{}
	}
	c.dirs[dir] = entries
}

func fileKey(name string, info os.FileInfo) (cacheKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return cacheKey{}, false
	}
	return cacheKey{name: name, dev: uint64(st.Dev), ino: st.Ino, mtime: info.ModTime().UnixNano(), size: info.Size()}, true
}

// scanState carries one ScanDir's byte budget and cache.
type scanState struct {
	opts    ScanOptions
	dir     string
	budget  int64
	used    int64
	entries map[cacheKey]ScanFile
}

// ScanDir reads the header of every ledger JSON file in dir. It never writes. It
// refuses a symlinked or replaced directory, symlinks, non-regular files and
// files replaced while opened, and reports each such file by name.
func ScanDir(dir string, opts ScanOptions) Scan {
	var s Scan
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = MaxScanFiles
	}
	before, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		s.Missing = true
		return s
	}
	if err != nil {
		s.Problem = fmt.Sprintf("local evidence unavailable: %v", err)
		return s
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		s.Problem = "ledger directory is not a regular directory"
		return s
	}
	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		s.Problem = fmt.Sprintf("local evidence unavailable: %v", err)
		return s
	}
	defer d.Close()
	opened, err := d.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		s.Problem = "ledger directory identity changed"
		return s
	}
	names, raw, err := readNames(d)
	if err != nil {
		s.Problem = fmt.Sprintf("ledger directory unreadable: %v", err)
		return s
	}
	if raw > MaxScanRawEntries {
		s.Problem, s.Truncated = fmt.Sprintf("directory scan truncated at %d raw entries", MaxScanRawEntries), true
		return s
	}
	if len(names) > MaxScanEntries {
		s.Problem, s.Truncated = fmt.Sprintf("directory scan truncated at %d entries", MaxScanEntries), true
		return s
	}
	st := &scanState{opts: opts, dir: dir, budget: opts.MaxBytes, entries: map[cacheKey]ScanFile{}}
	if st.budget <= 0 {
		st.budget = MaxScanBytes
	}
	sort.Strings(names)
	files := 0
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if files >= maxFiles {
			s.Problem, s.Truncated = fmt.Sprintf("ledger scan truncated at %d JSON files", maxFiles), true
			break
		}
		files++
		if current, e := os.Lstat(dir); e != nil || !os.SameFile(opened, current) {
			s.Problem = "ledger directory identity changed during scan"
			break
		}
		f, ok := st.scanFile(filepath.Join(dir, name), name)
		if !ok {
			s.Problem, s.Truncated = fmt.Sprintf("ledger scan truncated at %d bytes", st.budget), true
			break
		}
		s.Files = append(s.Files, f)
	}
	if opts.Cache != nil {
		opts.Cache.replace(dir, st.entries)
	}
	return s
}

// readNames lists the directory without the companion files. It stops once more
// than MaxScanEntries other entries or MaxScanRawEntries entries in all have
// been seen; ScanDir applies the caps.
func readNames(dir *os.File) (names []string, raw int, err error) {
	for {
		batch, e := dir.Readdirnames(512)
		raw += len(batch)
		for _, name := range batch {
			if !companion(name) {
				names = append(names, name)
			}
		}
		if e == io.EOF {
			return names, raw, nil
		}
		if e != nil {
			return nil, 0, e
		}
		if len(names) > MaxScanEntries || raw > MaxScanRawEntries {
			return names, raw, nil
		}
	}
}

// countingReader counts bytes and records the underlying read error, so read
// failures and growth are told apart from malformed JSON.
type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

// scanFile reads one ledger's header. It reports false, reading nothing, when the
// file would take the scan past its byte budget.
func (st *scanState) scanFile(path, name string) (ScanFile, bool) {
	result := func(kind ProblemKind, problem string) (ScanFile, bool) {
		return ScanFile{Name: name, Kind: kind, Problem: name + ": " + problem}, true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return result(ProblemUnreadable, fmt.Sprintf("unreadable: %v", err))
	}
	if !info.Mode().IsRegular() {
		return result(ProblemUnsafe, "refused nonregular file or symlink")
	}
	if info.Size() > MaxHeaderLedger {
		return result(ProblemOversized, fmt.Sprintf("file exceeds %d bytes", MaxHeaderLedger))
	}
	if st.used+info.Size() > st.budget {
		return ScanFile{}, false
	}
	st.used += info.Size()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result(ProblemUnreadable, fmt.Sprintf("unreadable: %v", err))
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || !os.SameFile(info, stat) {
		return result(ProblemUnsafe, "opened file identity mismatch")
	}
	key, keyed := fileKey(name, stat)
	keyed = keyed && st.opts.Cache != nil
	if keyed {
		if cached, ok := st.opts.Cache.get(st.dir, key); ok {
			st.entries[key] = cached
			return cached, true
		}
	}
	var r io.Reader = f
	if st.opts.Wrap != nil {
		r = st.opts.Wrap(name, r)
	}
	// The extra byte detects growth beyond the inclusive limit after Stat.
	counted := &countingReader{r: io.LimitReader(r, MaxHeaderLedger+1)}
	header, headerBytes, err := readHeader(counted)
	if counted.n > MaxHeaderLedger {
		return result(ProblemOversized, fmt.Sprintf("file grew beyond %d bytes", MaxHeaderLedger))
	}
	if counted.err != nil {
		return result(ProblemUnreadable, fmt.Sprintf("unreadable: %v", counted.err))
	}
	var out ScanFile
	var projection *ProjectionError
	switch {
	case err != nil && !errors.As(err, &projection):
		out, _ = result(ProblemMalformed, fmt.Sprintf("malformed or unsupported ledger: %v", err))
	case header.ID+".json" != name:
		out, _ = result(ProblemIdentity, "ledger identity mismatch")
	case projection != nil:
		out, _ = result(ProblemProjection, fmt.Sprintf("malformed historical projection: %v", projection))
		out.Header = header
	default:
		out = ScanFile{Name: name, Header: header}
	}
	out.HeaderBytes = headerBytes
	if keyed && cacheableKinds&(1<<out.Kind) != 0 && time.Since(stat.ModTime()) > racyWindow {
		st.entries[key] = out
	}
	return out, true
}
