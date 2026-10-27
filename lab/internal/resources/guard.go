package resources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Default guard timing: the soft-floor wait re-checks every 30 s for up to 10 min,
// and running work is sampled against the hard floors every 10 s.
const (
	DefaultPoll    = 30 * time.Second
	DefaultMaxWait = 10 * time.Minute
	DefaultSample  = 10 * time.Second
)

// LowError is a refusal to start: readings stayed below a soft floor for the whole
// wait. Reason is "resources: low disk (...)" or "resources: low memory (...)".
type LowError struct{ Reason string }

func (e *LowError) Error() string { return e.Reason }

// ExhaustedError is the cancellation cause a Monitor records when readings fall below
// a hard floor while work runs. Reason starts with "resources-exhausted".
type ExhaustedError struct{ Reason string }

func (e *ExhaustedError) Error() string { return e.Reason }

// Unwrap makes the cause a cancellation, so process.Run records an interrupted (and
// therefore incomplete) result rather than an ordinary failure.
func (e *ExhaustedError) Unwrap() error { return context.Canceled }

// Guard enforces Floors with readings from Reader. Sleep waits d or until ctx ends and
// reports whether the full wait elapsed; tests replace it so they never sleep.
type Guard struct {
	Floors  Floors
	Reader  Reader
	Poll    time.Duration
	MaxWait time.Duration
	Sample  time.Duration
	Sleep   func(ctx context.Context, d time.Duration) bool
	// Log, when set, receives one line per soft-floor wait.
	Log func(string)
}

// NewGuard guards labDir's volume, TMPDIR's and GOCACHE's with the Lab's floors
// (defaults, or <lab>/resources.json).
func NewGuard(labDir string) (*Guard, error) {
	floors, err := LoadFloors(labDir)
	if err != nil {
		return nil, err
	}
	return &Guard{Floors: floors, Reader: SystemReader(labDir)}, nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *Guard) sleep(ctx context.Context, d time.Duration) bool {
	if g.Sleep != nil {
		return g.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// Read takes one sample.
func (g *Guard) Read() Readings { return g.Reader.Read() }

// Check samples once and evaluates the floors.
func (g *Guard) Check() (Level, string) { return g.Floors.Evaluate(g.Read()) }

// Memory returns available memory, and false when it cannot be read.
func (g *Guard) Memory() (uint64, bool) {
	r := g.Read()
	return r.Memory, r.MemoryKnown
}

// WaitForCapacity returns nil once readings are at or above every soft floor. Below
// one it re-checks every Poll until MaxWait has passed, then returns a *LowError; a
// cancelled ctx returns ctx's error.
func (g *Guard) WaitForCapacity(ctx context.Context) error {
	poll, wait := orDefault(g.Poll, DefaultPoll), orDefault(g.MaxWait, DefaultMaxWait)
	checks := int(wait/poll) + 1
	for i := 1; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		level, reason := g.Check()
		if level == OK {
			return nil
		}
		if i >= checks {
			return &LowError{Reason: reason}
		}
		if g.Log != nil {
			g.Log(reason + "; waiting " + poll.String() + " before re-checking")
		}
		if !g.sleep(ctx, poll) {
			return ctx.Err()
		}
	}
}

// Monitor returns a context that is cancelled, with an *ExhaustedError cause, as soon
// as a sample falls below a hard floor. It samples at once and then every Sample until
// stop is called or parent ends. Cancelling the context stops every process.Run under
// it through its process-group kill.
func (g *Guard) Monitor(parent context.Context) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	finished := make(chan struct{})
	sample := orDefault(g.Sample, DefaultSample)
	go func() {
		defer close(finished)
		for {
			if level, reason := g.Check(); level == BelowHard {
				cancel(&ExhaustedError{Reason: strings.Replace(reason, "resources:", "resources-exhausted:", 1)})
				return
			}
			waitCtx, stopWait := context.WithCancel(ctx)
			go func() {
				select {
				case <-done:
				case <-waitCtx.Done():
				}
				stopWait()
			}()
			ok := g.sleep(waitCtx, sample)
			stopWait()
			if !ok {
				return
			}
		}
	}()
	return ctx, func() {
		select {
		case <-done:
		default:
			close(done)
		}
		<-finished
		cancel(nil)
	}
}

// SlotsAllowed returns how many of the configured verification slots may be in use
// under readings r. The first slot is the existing guard's (WaitForCapacity) to
// refuse; every slot past it is refused below any soft floor, and each slot is
// budgeted MemoryPerAttempt of available memory when memory is known. reason says why
// fewer than configured are allowed, and is empty otherwise.
func (f Floors) SlotsAllowed(configured int, r Readings) (int, string) {
	configured = max(1, min(configured, MaxVerificationSlots))
	if configured == 1 {
		return 1, ""
	}
	if level, reason := f.Evaluate(r); level != OK {
		return 1, reason + "; extra verification slots are refused"
	}
	if r.MemoryKnown {
		if n := max(1, min(configured, int(r.Memory/MemoryPerAttempt))); n < configured {
			return n, fmt.Sprintf("resources: %s of available memory allows %d verification slot(s) at %s each", Format(r.Memory), n, Format(MemoryPerAttempt))
		}
	}
	return configured, ""
}

// VerificationSlots is SlotsAllowed for one fresh sample.
func (g *Guard) VerificationSlots(configured int) (int, string) {
	return g.Floors.SlotsAllowed(configured, g.Read())
}

// ConcurrentViews reports whether a Molecule's candidate and protected views may run
// at the same time under readings r: every reading must be at or above its soft floor
// and, when memory is known, it must hold MemoryPerAttempt for each of the two views.
// reason says why they may not.
func (f Floors) ConcurrentViews(r Readings) (bool, string) {
	if level, reason := f.Evaluate(r); level != OK {
		return false, reason + "; the views run one after the other"
	}
	if r.MemoryKnown && r.Memory < 2*MemoryPerAttempt {
		return false, fmt.Sprintf("resources: %s of available memory is below %s for two concurrent views; the views run one after the other", Format(r.Memory), Format(2*MemoryPerAttempt))
	}
	return true, ""
}

// ConcurrentViews is Floors.ConcurrentViews for one fresh sample.
func (g *Guard) ConcurrentViews() (bool, string) { return g.Floors.ConcurrentViews(g.Read()) }

type viewConcurrencyKey struct{}

// WithViewConcurrency carries the decision of whether a verification's two views may
// run concurrently; allow is called once, just before the views start.
func WithViewConcurrency(ctx context.Context, allow func() (bool, string)) context.Context {
	return context.WithValue(ctx, viewConcurrencyKey{}, allow)
}

// ViewConcurrency asks the decision WithViewConcurrency carries. Without one the views
// run one after the other and the reason is empty.
func ViewConcurrency(ctx context.Context) (bool, string) {
	allow, _ := ctx.Value(viewConcurrencyKey{}).(func() (bool, string))
	if allow == nil {
		return false, ""
	}
	return allow()
}

// Exhausted reports the hard-floor reason when ctx was cancelled by a Monitor.
func Exhausted(ctx context.Context) (string, bool) {
	var e *ExhaustedError
	if errors.As(context.Cause(ctx), &e) {
		return e.Reason, true
	}
	return "", false
}

// AsLow reports the soft-floor reason when err is a *LowError.
func AsLow(err error) (string, bool) {
	var e *LowError
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}
