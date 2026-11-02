package molecule

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
)

// The Lab loop marker (#254). While a loop runs it holds loopMarkerFile, a
// flockLedgerFile lock on one fixed name, so one loop runs per Lab whoever
// started it, and the operating system drops the lock when its holder exits,
// including on a crash. The holder records a random token in the lock file and
// writes a bounded status, loopStatusFile in the Lab root, carrying the same
// token. A status whose lock is not held is stale; a status whose token is not
// the lock holder's is never reported as running. The status holds no paths and
// no environment values, and the holder removes it when it releases the marker.
const (
	loopMarkerFile       = "lab-loop" + loopLockSuffix
	loopStatusFile       = "loop-status.json"
	loopStatusTempPrefix = ".loop-status-"
	loopStatusSchema     = "v1"

	// LoopStarterCLI and LoopStarterUI name who started a loop.
	LoopStarterCLI = "cli"
	LoopStarterUI  = "ui"

	// The states ReadLoopStatus reports.
	LoopMarkerRunning    = "running"
	LoopMarkerNotRunning = "not-running"
	LoopMarkerStale      = "stale"
	LoopMarkerUnreadable = "unreadable"

	// MaxLoopStatusInFlight bounds the in-flight Molecule ids a status lists.
	MaxLoopStatusInFlight = 64
	maxLoopStatusBytes    = 16 << 10
	maxLoopSelectorRunes  = 128
	maxLoopMarkerNote     = 64
	maxLoopConcurrency    = 1 << 16
	maxLoopCount          = 1 << 31
)

// ErrLoopRunning marks a loop refused because another loop holds this Lab's
// loop marker.
var ErrLoopRunning = errors.New("another loop is running in this Lab")

// ErrIssueBusy marks an intake refused because another process holds the
// Issue's start lock.
var ErrIssueBusy = errors.New("another process is starting a Molecule for this Issue")

var errUnsafeLoopFile = errors.New("not a bounded regular file owned by this user")

// loopMarkerRetry rides out a reader's momentary shared-lock probe, which makes
// an exclusive non-blocking lock fail for that instant.
var loopMarkerRetry = []time.Duration{2 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond}

// LoopStatus is the bounded status of the loop that holds a Lab's marker. Token
// is never served: it only lets the process that started a loop recognise it.
type LoopStatus struct {
	Token                string    `json:"-"`
	Starter              string    `json:"starter"`
	StartedAt            time.Time `json:"startedAt"`
	Selector             string    `json:"selector"`
	SelectorTruncated    bool      `json:"selectorTruncated,omitempty"`
	MaxConcurrent        int       `json:"maxConcurrent"`
	WatchIntervalSeconds int64     `json:"watchIntervalSeconds,omitempty"`
	Passes               int       `json:"passes"`
	LastPassAt           time.Time `json:"lastPassAt,omitzero"`
	InFlight             []string  `json:"inFlight"`
	InFlightTruncated    bool      `json:"inFlightTruncated,omitempty"`
	// Intake lists the Issues the loop is starting, before Run has named their
	// Molecule; each is replaced by its Molecule id once Run returns one. Its
	// overflow also sets InFlightTruncated.
	Intake []LoopIntake `json:"intake,omitempty"`
}

// LoopIntakePlanning is the phase of an Issue whose intake is still running.
const LoopIntakePlanning = "planning"

// LoopIntake is an Issue in flight while its intake has not yet named a
// Molecule: a GitHub Issue by number, or a tracker plugin's Issue by Key, never
// both.
type LoopIntake struct {
	Issue int64  `json:"issue,omitempty"`
	Key   string `json:"key,omitempty"`
	Phase string `json:"phase"`
}

// maxLoopStatusKeyBytes bounds the keyed intake entries of one status by their
// marshalled JSON, escapes included, so a status of long keys stays under
// maxLoopStatusBytes; the overflow sets InFlightTruncated. loopStatusKeySlack
// is the room kept for the fields a keyed entry can add around itself.
const (
	maxLoopStatusKeyBytes = 8 << 10
	loopStatusKeySlack    = 64
)

// loopIntakeKeyBytes is the marshalled size of key's intake entry with its
// separating comma; a key that cannot be marshalled never fits.
func loopIntakeKeyBytes(key string) int {
	raw, err := json.Marshal(LoopIntake{Key: key, Phase: LoopIntakePlanning})
	if err != nil {
		return maxLoopStatusBytes + 1
	}
	return len(raw) + 1
}

// loopStatusRecord is the status file: the status with its schema and token.
type loopStatusRecord struct {
	SchemaVersion string `json:"schemaVersion"`
	Token         string `json:"token"`
	LoopStatus
}

// LoopMarkerState is what ReadLoopStatus reports. Running is true only while the
// marker's lock is held and the status carries the holder's token; Held reports
// the lock alone, so an unreadable status still refuses a second loop.
type LoopMarkerState struct {
	Running bool        `json:"running"`
	State   string      `json:"state"`
	Held    bool        `json:"held"`
	Status  *LoopStatus `json:"status,omitempty"`
}

// LoopMarker is one loop's hold on its Lab's marker. RunLoop acquires it on first
// use and keeps it across calls; whoever created it releases it. A nil marker
// does nothing.
type LoopMarker struct {
	starter string
	watch   time.Duration

	mu       sync.Mutex
	lab      string
	release  func()
	status   LoopStatus
	inFlight map[string]int
	intake   map[int64]int
	keys     map[string]int
	// writeErr is the first status write refused or failed since a keyed loop
	// last reported one.
	writeErr error
}

// NewLoopMarker prepares a marker for a loop started by starter (LoopStarterCLI
// or LoopStarterUI). watch is the --watch interval, or zero.
func NewLoopMarker(starter string, watch time.Duration) *LoopMarker {
	return &LoopMarker{starter: starter, watch: watch}
}

// Acquire takes the Lab's marker and writes the loop's first status. It refuses
// with ErrLoopRunning while another loop holds the marker, and it never creates
// the Lab directory. Acquiring a marker already held for lab does nothing.
func (m *LoopMarker) Acquire(lab string, sel intake.Selector, maxConcurrent int) error {
	abs, err := filepath.Abs(lab)
	if err != nil {
		return err
	}
	if m.starter != LoopStarterCLI && m.starter != LoopStarterUI {
		return fmt.Errorf("unknown loop starter")
	}
	if maxConcurrent < 1 || maxConcurrent > maxLoopConcurrency {
		return fmt.Errorf("max concurrent must be from 1 to %d", maxLoopConcurrency)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.release != nil {
		if m.lab != abs {
			return fmt.Errorf("this loop already holds another Lab's loop marker")
		}
		return nil
	}
	if info, err := os.Lstat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("the Lab directory is missing or not a directory, so no loop was started")
	}
	tok, err := randomHex(16)
	if err != nil {
		return err
	}
	file, release, err := takeLoopMarker(abs)
	if errors.Is(err, errLocked) {
		return loopRunningError(abs)
	}
	if err != nil {
		return fmt.Errorf("the Lab's loop marker could not be taken, so no loop was started: %w", pathlessErr(err))
	}
	selector, truncated := boundedSelector(sel.String())
	status := LoopStatus{Token: tok, Starter: m.starter, StartedAt: time.Now().UTC(), Selector: selector, SelectorTruncated: truncated, MaxConcurrent: maxConcurrent, WatchIntervalSeconds: int64(m.watch / time.Second), InFlight: []string{}}
	if err = writeMarkerNote(file, tok); err == nil {
		err = writeLoopStatus(abs, status)
	}
	if err != nil {
		release()
		return fmt.Errorf("the Lab's loop status could not be written, so no loop was started: %w", pathlessErr(err))
	}
	m.lab, m.release, m.status, m.inFlight, m.intake, m.keys = abs, release, status, map[string]int{}, map[int64]int{}, map[string]int{}
	return nil
}

// recordTrackerSelector replaces the status's selector with a tracker plugin's
// raw selector, bounded as Acquire bounds a GitHub selector. A keyed loop
// acquires the marker with a zero intake.Selector and then records its own.
func (m *LoopMarker) recordTrackerSelector(raw string) {
	m.update(func(s *LoopStatus) {
		s.Selector, s.SelectorTruncated = boundedSelector(raw)
	})
}

// Release removes the loop's own status and drops the marker.
func (m *LoopMarker) Release() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.release == nil {
		return
	}
	// The marker is still held, so no other loop writes the status meanwhile, and
	// only a status carrying this loop's token is removed.
	if rec, present, err := readLoopStatusFile(m.lab); present && err == nil && rec.Token == m.status.Token {
		os.Remove(filepath.Join(m.lab, loopStatusFile))
	}
	m.release()
	m.release = nil
}

// Token is the held marker's token, or "".
func (m *LoopMarker) Token() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.release == nil {
		return ""
	}
	return m.status.Token
}

// beginPass records a new pass. A status write that fails leaves the previous
// status in place; the lock alone decides whether a loop runs.
func (m *LoopMarker) beginPass() {
	m.update(func(s *LoopStatus) {
		s.Passes = min(s.Passes+1, maxLoopCount)
		s.LastPassAt = time.Now().UTC()
	})
}

// Track records a Molecule the loop is working on; Untrack removes it.
func (m *LoopMarker) Track(id string) {
	m.update(func(*LoopStatus) {
		if safeLoopID(id) && len(id) <= 200 {
			m.inFlight[id]++
		}
	})
}

func (m *LoopMarker) Untrack(id string) {
	m.update(func(*LoopStatus) {
		if m.inFlight[id] > 1 {
			m.inFlight[id]--
		} else {
			delete(m.inFlight, id)
		}
	})
}

// TrackIntake records an Issue whose intake the loop is running, before Run has
// named its Molecule; UntrackIntake removes it. Callers defer UntrackIntake, so
// a recovered panic never leaves the Issue listed.
func (m *LoopMarker) TrackIntake(issue int64) {
	m.update(func(*LoopStatus) {
		if validLoopIssue(issue) {
			m.intake[issue]++
		}
	})
}

func (m *LoopMarker) UntrackIntake(issue int64) {
	m.update(func(*LoopStatus) {
		if m.intake[issue] > 1 {
			m.intake[issue]--
		} else {
			delete(m.intake, issue)
		}
	})
}

// TrackIntakeKey is TrackIntake for a tracker plugin's Issue key; a key that is
// not a valid Issue key is never listed. UntrackIntakeKey removes it.
func (m *LoopMarker) TrackIntakeKey(key string) {
	m.update(func(*LoopStatus) {
		if validLoopKey(key) {
			m.keys[key]++
		}
	})
}

func (m *LoopMarker) UntrackIntakeKey(key string) {
	m.update(func(*LoopStatus) {
		if m.keys[key] > 1 {
			m.keys[key]--
		} else {
			delete(m.keys, key)
		}
	})
}

func (m *LoopMarker) update(change func(*LoopStatus)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.release == nil {
		return
	}
	change(&m.status)
	ids := make([]string, 0, len(m.inFlight))
	for id := range m.inFlight {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	issues := make([]int64, 0, len(m.intake))
	for issue := range m.intake {
		issues = append(issues, issue)
	}
	slices.Sort(issues)
	keys := make([]string, 0, len(m.keys))
	for key := range m.keys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	m.status.InFlightTruncated = len(ids) > MaxLoopStatusInFlight || len(issues)+len(keys) > MaxLoopStatusInFlight
	m.status.InFlight = ids[:min(len(ids), MaxLoopStatusInFlight)]
	m.status.Intake = nil
	for _, issue := range issues[:min(len(issues), MaxLoopStatusInFlight)] {
		m.status.Intake = append(m.status.Intake, LoopIntake{Issue: issue, Phase: LoopIntakePlanning})
	}
	// Keyed entries follow the numeric ones within the same bound, by their
	// marshalled size within maxLoopStatusKeyBytes and within what the rest of the
	// status leaves of maxLoopStatusBytes; a numeric-only loop writes what it
	// always has.
	if len(keys) > 0 {
		room := maxLoopStatusKeyBytes
		if raw, err := json.Marshal(loopStatusRecord{SchemaVersion: loopStatusSchema, Token: m.status.Token, LoopStatus: m.status}); err != nil {
			room = 0
		} else {
			room = min(room, maxLoopStatusBytes-loopStatusKeySlack-len(raw))
		}
		for _, key := range keys {
			n := loopIntakeKeyBytes(key)
			if len(m.status.Intake) >= MaxLoopStatusInFlight || n > room {
				m.status.InFlightTruncated = true
				break
			}
			room -= n
			m.status.Intake = append(m.status.Intake, LoopIntake{Key: key, Phase: LoopIntakePlanning})
		}
	}
	// A refused or failed write leaves the previous status in place; it is kept
	// so a keyed loop reports it rather than leave the status stale unnoticed.
	if err := writeLoopStatus(m.lab, m.status); err != nil && m.writeErr == nil {
		m.writeErr = err
	}
}

// takeStatusWriteError returns the first status write refused or failed since
// the last call, without its local path, and forgets it.
func (m *LoopMarker) takeStatusWriteError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.writeErr
	m.writeErr = nil
	if err == nil {
		return nil
	}
	return pathlessErr(err)
}

// ReadLoopStatus reports whether a loop holds lab's marker, with its bounded
// status. It is read-only: it never creates the lock file, the ledgers directory
// or a status, and anything it cannot verify is never reported as running.
func ReadLoopStatus(lab string) LoopMarkerState {
	abs, err := filepath.Abs(lab)
	if err != nil {
		return LoopMarkerState{State: LoopMarkerUnreadable}
	}
	held, note, err := probeLoopMarker(abs)
	if err != nil {
		return LoopMarkerState{State: LoopMarkerUnreadable}
	}
	rec, present, err := readLoopStatusFile(abs)
	if !held {
		if present {
			return LoopMarkerState{State: LoopMarkerStale}
		}
		return LoopMarkerState{State: LoopMarkerNotRunning}
	}
	if err != nil || !present || rec.Token != note {
		return LoopMarkerState{State: LoopMarkerUnreadable, Held: true}
	}
	status := rec.LoopStatus
	status.Token = rec.Token
	status.InFlight = slices.Clone(status.InFlight)
	if status.InFlight == nil {
		status.InFlight = []string{}
	}
	status.Intake = slices.Clone(status.Intake)
	return LoopMarkerState{Running: true, State: LoopMarkerRunning, Held: true, Status: &status}
}

// refuseWhileLoopMarkerHeld refuses a loop that runs without a marker of its own
// while another loop holds lab's marker.
func refuseWhileLoopMarkerHeld(lab string) error {
	held, _, err := probeLoopMarker(lab)
	if err != nil {
		return fmt.Errorf("the Lab's loop marker could not be checked, so no loop was started: %w", pathlessErr(err))
	}
	if held {
		return loopRunningError(lab)
	}
	return nil
}

// loopRunningError names who started the loop that holds lab's marker.
func loopRunningError(lab string) error {
	detail := "it holds this Lab's loop marker; wait for it to finish or stop it where it was started"
	if s := ReadLoopStatus(lab); s.Running {
		switch s.Status.Starter {
		case LoopStarterCLI:
			detail = "it was started in a terminal with forgecell loop; stop it there"
		case LoopStarterUI:
			detail = "it was started from a Lab UI; stop it from that UI"
		}
	}
	return fmt.Errorf("%w: %s. One loop runs per Lab, so this loop listed no Issue and started nothing", ErrLoopRunning, detail)
}

func takeLoopMarker(lab string) (*os.File, func(), error) {
	file, release, err := flockLedgerHandle(lab, loopMarkerFile)
	for _, wait := range loopMarkerRetry {
		if !errors.Is(err, errLocked) {
			break
		}
		time.Sleep(wait)
		file, release, err = flockLedgerHandle(lab, loopMarkerFile)
	}
	return file, release, err
}

// probeLoopMarker tests the marker's lock with a shared non-blocking lock and
// reads the holder's note when it is held. A missing lock file means no loop.
func probeLoopMarker(lab string) (held bool, note string, err error) {
	path := filepath.Join(lab, "ledgers", loopMarkerFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if !info.Mode().IsRegular() || !ownedByUser(info) {
		return false, "", errUnsafeLoopFile
	}
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer file.Close()
	if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
		return false, "", errUnsafeLoopFile
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return false, "", nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, "", err
	}
	buf := make([]byte, maxLoopMarkerNote)
	n, _ := file.ReadAt(buf, 0)
	return true, strings.TrimSpace(string(buf[:n])), nil
}

// writeMarkerNote replaces the lock file's note while the lock is held.
func writeMarkerNote(file *os.File, note string) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err := file.WriteAt([]byte(note+"\n"), 0)
	return err
}

// readMarkerNote reads a lock file's short note without locking or creating it.
func readMarkerNote(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !ownedByUser(info) {
		return ""
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer file.Close()
	buf := make([]byte, maxLoopMarkerNote)
	n, _ := file.ReadAt(buf, 0)
	return strings.TrimSpace(string(buf[:n]))
}

// writeLoopStatus replaces the status atomically: a fresh exclusive temporary
// file in the Lab root, renamed over the status.
func writeLoopStatus(lab string, s LoopStatus) error {
	raw, err := json.Marshal(loopStatusRecord{SchemaVersion: loopStatusSchema, Token: s.Token, LoopStatus: s})
	if err != nil {
		return err
	}
	if len(raw) > maxLoopStatusBytes {
		return fmt.Errorf("the loop status is larger than %d bytes", maxLoopStatusBytes)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(lab, loopStatusTempPrefix+suffix+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(lab, loopStatusFile))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readLoopStatusFile reads and validates the status. present reports that some
// entry exists at the status path, valid or not.
func readLoopStatusFile(lab string) (rec loopStatusRecord, present bool, err error) {
	path := filepath.Join(lab, loopStatusFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, true, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLoopStatusBytes || !ownedByUser(info) {
		return rec, true, errUnsafeLoopFile
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return rec, true, err
	}
	defer file.Close()
	if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
		return rec, true, errUnsafeLoopFile
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLoopStatusBytes+1))
	if err != nil {
		return rec, true, err
	}
	if len(raw) > maxLoopStatusBytes {
		return rec, true, errUnsafeLoopFile
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&rec); err != nil {
		return loopStatusRecord{}, true, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return loopStatusRecord{}, true, fmt.Errorf("the loop status has trailing data")
	}
	if err = rec.validate(); err != nil {
		return loopStatusRecord{}, true, err
	}
	return rec, true, nil
}

func (r loopStatusRecord) validate() error {
	s := r.LoopStatus
	switch {
	case r.SchemaVersion != loopStatusSchema:
		return fmt.Errorf("unsupported loop status schema")
	case !validLoopToken(r.Token):
		return fmt.Errorf("invalid loop token")
	case s.Starter != LoopStarterCLI && s.Starter != LoopStarterUI:
		return fmt.Errorf("invalid loop starter")
	case s.StartedAt.IsZero():
		return fmt.Errorf("the loop status has no start time")
	case harness.ReviewDisplayText(s.Selector, maxLoopSelectorRunes) != s.Selector:
		return fmt.Errorf("invalid loop selector")
	case s.MaxConcurrent < 1 || s.MaxConcurrent > maxLoopConcurrency:
		return fmt.Errorf("invalid loop max concurrent")
	case s.WatchIntervalSeconds < 0 || s.WatchIntervalSeconds > maxLoopCount:
		return fmt.Errorf("invalid loop watch interval")
	case s.Passes < 0 || s.Passes > maxLoopCount:
		return fmt.Errorf("invalid loop pass count")
	case len(s.InFlight) > MaxLoopStatusInFlight:
		return fmt.Errorf("too many in-flight Molecules")
	case len(s.Intake) > MaxLoopStatusInFlight:
		return fmt.Errorf("too many in-flight intakes")
	}
	for _, id := range s.InFlight {
		if !safeLoopID(id) || len(id) > 200 {
			return fmt.Errorf("invalid in-flight Molecule id")
		}
	}
	keyBytes := 0
	for _, in := range s.Intake {
		// Exactly one of a valid Issue number or a valid Key.
		numbered := in.Key == "" && validLoopIssue(in.Issue)
		keyed := in.Issue == 0 && validLoopKey(in.Key)
		if numbered == keyed || in.Phase != LoopIntakePlanning {
			return fmt.Errorf("invalid in-flight intake")
		}
		if keyed {
			keyBytes += loopIntakeKeyBytes(in.Key)
		}
	}
	if keyBytes > maxLoopStatusKeyBytes {
		return fmt.Errorf("invalid in-flight intake")
	}
	return nil
}

// validLoopIssue bounds an Issue number as GitHub's API does.
func validLoopIssue(n int64) bool {
	return n > 0 && n <= maxLoopCount
}

// validLoopKey admits a tracker Issue key that intake.ValidateIssueKey accepts
// and that names a Molecule (a non-empty slug).
func validLoopKey(key string) bool {
	return intake.ValidateIssueKey(key) == nil && (Issue{Key: key}).KeySlug() != ""
}

func validLoopToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// boundedSelector is the selector as one bounded display line, and whether it
// had to change.
func boundedSelector(s string) (string, bool) {
	clean := harness.ReviewDisplayText(s, maxLoopSelectorRunes)
	return clean, clean != s
}

func ownedByUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pathlessErr drops the path from a file error, so no message names the Lab's
// absolute location.
func pathlessErr(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s %s: %w", pathErr.Op, filepath.Base(pathErr.Path), pathErr.Err)
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("%s %s: %w", linkErr.Op, filepath.Base(linkErr.New), linkErr.Err)
	}
	return err
}

// The owner notes a per-Issue start lock carries, so a refusal names who holds it.
const (
	issueOwnerLoop = "loop"
	issueOwnerRun  = "run"
)

// lockIssueAs is lockIssue that records owner in the lock file.
func lockIssueAs(lab, repository string, number int64, owner string) (func(), error) {
	name, err := issueLockName(repository, number)
	if err != nil {
		return nil, err
	}
	file, release, err := flockLedgerHandle(lab, name)
	if err != nil {
		return nil, err
	}
	if err = writeMarkerNote(file, owner); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// lockIssueKeyAs is lockIssueAs for a keyed Issue: it takes the key's start
// lock, issueProjectKeyLockName, the lock RunIntakeKeyed takes, and records
// owner in it.
func lockIssueKeyAs(lab, repository, key, owner string) (func(), error) {
	if err := intake.ValidateIssueKey(key); err != nil {
		return nil, err
	}
	name, err := issueProjectKeyLockName(repository, key)
	if err != nil {
		return nil, err
	}
	file, release, err := flockLedgerHandle(lab, name)
	if err != nil {
		return nil, err
	}
	if err = writeMarkerNote(file, owner); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// issueLockHolder names the holder of an Issue's start lock from its note.
func issueLockHolder(lab, repository string, number int64) string {
	name, err := issueLockName(repository, number)
	if err != nil {
		return "another process"
	}
	switch readMarkerNote(filepath.Join(lab, "ledgers", name)) {
	case issueOwnerLoop:
		return "a running loop (forgecell loop or a Lab UI)"
	case issueOwnerRun:
		return "a forgecell run (from a terminal or a Lab UI)"
	}
	return "another process"
}

// RunIntake is Run for the CLI's and the UI's single runs. Intake first takes
// the Issue's start lock, the lock a loop holds for the whole of its intake, so a
// run and a loop never create two Molecules for one Issue; a held lock refuses
// with ErrIssueBusy, naming its holder. An approval (Approve set) takes the
// Molecule lock instead and goes straight to Run. The loop itself calls Run
// while it holds the Issue lock.
func RunIntake(ctx context.Context, o Options) (Record, error) {
	if o.Approve != "" {
		return Run(ctx, o)
	}
	lab, err := filepath.Abs(o.LabDir)
	if err != nil {
		return Record{}, err
	}
	loaded, err := loadLoopFormula(lab, o.FormulaID)
	if err != nil {
		return Record{}, err
	}
	ref, err := intake.ParseIssueRef(o.Issue)
	if err != nil {
		return Record{}, err
	}
	if ref, err = intake.BindRepository(ref, loaded.Formula.Intake.Repo); err != nil {
		return Record{}, err
	}
	repository := ref.Owner + "/" + ref.Repo
	unlock, err := lockIssueAs(lab, repository, ref.Number, issueOwnerRun)
	if errors.Is(err, errLocked) {
		return Record{}, fmt.Errorf("%w: %s holds the start lock for Issue #%s; wait for it to finish, then run this again. Nothing was started", ErrIssueBusy, issueLockHolder(lab, repository, ref.Number), strconv.FormatInt(ref.Number, 10))
	}
	if err != nil {
		return Record{}, fmt.Errorf("the Issue start lock could not be taken: %w", pathlessErr(err))
	}
	defer unlock()
	return Run(ctx, o)
}
