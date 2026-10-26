package molecule

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// StatusRetired is the terminal ledger status Retire sets. A retired Molecule is
// finished history: the loop never resumes it, and it never blocks a fresh
// Molecule for the same open Issue. Nothing it recorded is deleted.
const StatusRetired = "retired"

// MaxRetireReason bounds a retire reason, in runes after control characters are
// stripped. A longer reason is refused, never truncated.
const MaxRetireReason = 200

// maxRetireReasonBytes bounds the raw reason before it is examined at all.
const maxRetireReasonBytes = 4096

// maxRetireReceiptBytes matches the delivery receipt read bound.
const maxRetireReceiptBytes = 8_000_000

// Retirement is the retired event: when, why, and the status it replaced. It is
// additive and omitempty, so a ledger without it decodes unchanged, and a reader
// that predates it ignores it and still sees status retired.
type Retirement struct {
	At          string `json:"at"`
	Reason      string `json:"reason"`
	PriorStatus string `json:"priorStatus"`
}

// Retire refusals. Each error Retire returns wraps exactly one of them, so a
// caller can map it to a fixed category; the error text never names a local path.
var (
	ErrRetireInvalid  = errors.New("retire refused: invalid request")
	ErrRetireMissing  = errors.New("retire refused: no such Molecule")
	ErrRetireLedger   = errors.New("retire refused: the ledger was refused")
	ErrRetireActive   = errors.New("retire refused: the Molecule is active")
	ErrRetireLocked   = errors.New("retire refused: the Molecule is locked")
	ErrRetireRetired  = errors.New("retire refused: the Molecule is already retired")
	ErrRetireDelivery = errors.New("retire refused: delivery is in progress")
	ErrRetireWrite    = errors.New("retire failed: the ledger could not be written")
)

// RetireReason validates and cleans a retire reason: it is required, one line,
// at most MaxRetireReason runes once control and format characters are
// stripped, and never empty after stripping.
func RetireReason(raw string) (string, error) {
	if len(raw) > maxRetireReasonBytes {
		return "", fmt.Errorf("%w: the reason is longer than %d characters", ErrRetireInvalid, MaxRetireReason)
	}
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: the reason is not valid UTF-8", ErrRetireInvalid)
	}
	if strings.ContainsAny(raw, "\r\n\v\f\u0085  ") {
		return "", fmt.Errorf("%w: the reason must be one line", ErrRetireInvalid)
	}
	clean := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, raw))
	if clean == "" {
		return "", fmt.Errorf("%w: give a reason for retiring the Molecule", ErrRetireInvalid)
	}
	if utf8.RuneCountInString(clean) > MaxRetireReason {
		return "", fmt.Errorf("%w: the reason is longer than %d characters", ErrRetireInvalid, MaxRetireReason)
	}
	return clean, nil
}

// Retire appends a retired event to the Molecule's ledger and sets its terminal
// status retired. It holds the per-Molecule flock for the whole read, check and
// write, and refuses a Molecule that is running, allocating or queued for checks,
// one locked by another command or by an approval lock directory, one already
// retired, and one whose delivery receipt is prepared or committed. It deletes
// nothing: the ledger, workspace, evidence and receipts all stay.
func Retire(lab, id, reason string) (Record, error) {
	var empty Record
	if !safeLoopID(id) {
		return empty, fmt.Errorf("%w: invalid Molecule id", ErrRetireInvalid)
	}
	clean, err := RetireReason(reason)
	if err != nil {
		return empty, err
	}
	if lab, err = filepath.Abs(lab); err != nil {
		return empty, fmt.Errorf("%w: the Lab directory could not be resolved", ErrRetireInvalid)
	}
	// The ledger must exist before the flock creates its lock file beside it.
	if _, err = ReadRecord(lab, id); err != nil {
		return empty, retireReadError(err)
	}
	unlock, err := lockMolecule(lab, id)
	if errors.Is(err, errLocked) {
		return empty, fmt.Errorf("%w: a running loop or another forgecell command holds the Molecule lock; wait for it to finish, then retire again", ErrRetireLocked)
	}
	if err != nil {
		return empty, fmt.Errorf("%w: the Molecule lock could not be taken", ErrRetireLocked)
	}
	defer unlock()
	if _, err = os.Lstat(filepath.Join(lab, "ledgers", id+".lock")); !errors.Is(err, os.ErrNotExist) {
		return empty, fmt.Errorf("%w: the Molecule's approval lock exists (an approval, amend or recover holds it, or an interrupted one left it); inspect it with forgecell ledger %s", ErrRetireLocked, id)
	}
	r, err := ReadRecord(lab, id)
	if err != nil {
		return empty, retireReadError(err)
	}
	switch {
	case r.Status == StatusRetired || r.Retired != nil:
		return r, fmt.Errorf("%w: it was retired at %s", ErrRetireRetired, oneLine(retiredAt(r)))
	case r.Status == "running" || r.Status == "allocating":
		what := "status " + r.Status
		if QueuedForChecks(r) {
			what = QueuedForChecksLabel
		}
		return r, fmt.Errorf("%w: the ledger reports %s; stop it or wait for it to finish first", ErrRetireActive, what)
	}
	if _, err = os.Lstat(filepath.Join(lab, ".delivery-lock")); !errors.Is(err, os.ErrNotExist) {
		return r, fmt.Errorf("%w: the Lab's delivery lock exists, so a delivery may be running; wait for it, or recover the lock with the forgecell CLI", ErrRetireDelivery)
	}
	state, found, err := retireReceiptState(lab, id)
	if err != nil {
		return r, fmt.Errorf("%w: the delivery receipt could not be read safely, so a delivery may be in progress", ErrRetireDelivery)
	}
	if found && state != "published" {
		return r, fmt.Errorf("%w: the delivery receipt is %s; finish or reconcile the delivery with forgecell deliver first", ErrRetireDelivery, oneLine(state))
	}
	r.Retired = &Retirement{At: stamp(), Reason: clean, PriorStatus: r.Status}
	r.Status = StatusRetired
	if err = save(r); err != nil {
		return r, fmt.Errorf("%w; read it back with forgecell ledger %s", ErrRetireWrite, id)
	}
	return r, nil
}

// ErrMoleculeRetired is the one refusal every path that would resurrect a
// retired Molecule returns: approval, amend, rebase and recover.
var ErrMoleculeRetired = errors.New("Molecule is retired; start a new Molecule for the Issue")

// refuseRetired refuses a retired Molecule. Callers apply it to the ledger read
// under the Molecule lock, so a Retire that won the lock is always seen.
func refuseRetired(r Record) error {
	if r.Status == StatusRetired || r.Retired != nil {
		return ErrMoleculeRetired
	}
	return nil
}

func retiredAt(r Record) string {
	if r.Retired != nil && r.Retired.At != "" {
		return r.Retired.At
	}
	return "an unrecorded time"
}

// retireReadError maps a ledger read error to a refusal; the read error itself
// can name local paths, so only its category is kept.
func retireReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no ledger in this Lab has this id", ErrRetireMissing)
	}
	return fmt.Errorf("%w: it is invalid, unsafe, oversized or not this Lab's", ErrRetireLedger)
}

// retireReceiptState reads the state of deliveries/<id>.json without locking or
// writing it. It refuses a symlinked or non-directory deliveries directory, a
// symlink, a non-regular or oversized receipt and one replaced while opened. A
// receipt naming no known state is an error, so the caller fails safe.
func retireReceiptState(lab, id string) (state string, found bool, err error) {
	dir := filepath.Join(lab, "deliveries")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		return "", true, errors.New("deliveries is not a directory")
	}
	file := filepath.Join(dir, id+".json")
	info, err = os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRetireReceiptBytes {
		return "", true, errors.New("receipt is not a bounded regular file")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", true, err
	}
	defer f.Close()
	if stat, err := f.Stat(); err != nil || !os.SameFile(info, stat) {
		return "", true, errors.New("opened receipt identity mismatch")
	}
	var receipt struct {
		State string `json:"state"`
	}
	limited := &io.LimitedReader{R: f, N: maxRetireReceiptBytes + 1}
	if err = json.NewDecoder(limited).Decode(&receipt); err != nil || limited.N <= 0 {
		return "", true, errors.New("malformed or oversized receipt")
	}
	switch receipt.State {
	case "prepared", "committed", "published":
		return receipt.State, true, nil
	}
	return "", true, errors.New("unknown receipt state")
}
