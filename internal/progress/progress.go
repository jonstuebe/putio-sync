// Package progress tracks in-flight file transfers and renders them.
//
// A Registry is the single source of truth for what is being transferred. A
// Tracker is one file's slice of it, and doubles as the io.Reader wrapper that
// counts bytes. A Renderer turns the registry into output: a live,
// download-manager-style display on a terminal, or periodic log lines when
// there is no terminal to draw on.
//
// The renderer never logs. Anything it wants to say has to go through the
// registry, because a log call from inside the render path would re-enter the
// handler that parks the live region.
package progress

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulbellamy/ratecounter"
)

// rateWindow is the window transfer speeds are averaged over. Short enough to
// notice a stall, long enough that the number stops flickering.
const rateWindow = 5 * time.Second

// Direction is which way a file is moving.
type Direction int

const (
	Download Direction = iota
	Upload
)

func (d Direction) String() string {
	if d == Upload {
		return "Uploading"
	}
	return "Downloading"
}

// Status is where a transfer has got to.
type Status int

const (
	Queued Status = iota
	Active
	Done
	Failed
)

// Tracker follows a single file transfer. It is safe for concurrent use: the
// transfer goroutine writes to it through Read while the renderer reads it.
type Tracker struct {
	relpath   string
	direction Direction

	offset int64 // atomic
	size   int64 // atomic
	// startOffset is where the current attempt began, so a resumed transfer
	// does not claim credit for bytes an earlier attempt moved.
	startOffset int64 // atomic

	counter *ratecounter.RateCounter

	mu        sync.Mutex
	status    Status
	err       error
	startedAt time.Time
	endedAt   time.Time
}

func newTracker(direction Direction, relpath string, size int64) *Tracker {
	return &Tracker{
		relpath:   relpath,
		direction: direction,
		size:      size,
		counter:   ratecounter.NewRateCounter(rateWindow),
	}
}

// NewTracker returns a Tracker that belongs to no registry. Transfers started
// outside a sync pass still have somewhere to report to.
func NewTracker(direction Direction, relpath string, size int64) *Tracker {
	return newTracker(direction, relpath, size)
}

// Reset points the tracker at a fresh attempt. A retried transfer resumes from
// its persisted offset rather than starting over, so the bar picks up where the
// file does.
func (t *Tracker) Reset(offset, size int64) {
	atomic.StoreInt64(&t.offset, offset)
	atomic.StoreInt64(&t.startOffset, offset)
	atomic.StoreInt64(&t.size, size)
}

// Wrap returns a reader that counts everything read from r.
func (t *Tracker) Wrap(r io.Reader) io.Reader {
	return &trackedReader{t: t, r: r}
}

func (t *Tracker) add(n int64) {
	atomic.AddInt64(&t.offset, n)
	t.counter.Incr(n)
}

func (t *Tracker) start() {
	t.mu.Lock()
	t.status = Active
	t.startedAt = time.Now()
	t.mu.Unlock()
}

func (t *Tracker) finish(err error) {
	t.mu.Lock()
	if err != nil {
		t.status = Failed
		t.err = err
	} else {
		t.status = Done
	}
	t.endedAt = time.Now()
	t.mu.Unlock()
}

// State is a consistent copy of a tracker, taken so the renderer never has to
// read a moving target.
type State struct {
	RelPath   string
	Direction Direction
	Status    Status
	Offset    int64
	Size      int64
	Rate      int64 // bytes per second
	// Moved is the number of bytes transferred in the current attempt.
	Moved   int64
	Elapsed time.Duration
	Err     error
}

// Remaining is the number of bytes still to transfer.
func (s State) Remaining() int64 {
	if s.Size <= s.Offset {
		return 0
	}
	return s.Size - s.Offset
}

// Percent is how far along the transfer is, 0-100.
func (s State) Percent() int {
	size := s.Size
	if size <= 0 {
		return 0
	}
	p := int((s.Offset * 100) / size)
	if p > 100 {
		return 100
	}
	return p
}

func (t *Tracker) state() State {
	t.mu.Lock()
	status, err, startedAt, endedAt := t.status, t.err, t.startedAt, t.endedAt
	t.mu.Unlock()

	var elapsed time.Duration
	switch {
	case startedAt.IsZero():
	case endedAt.IsZero():
		elapsed = time.Since(startedAt)
	default:
		elapsed = endedAt.Sub(startedAt)
	}

	offset := atomic.LoadInt64(&t.offset)

	var rate int64
	if status == Active {
		rate = t.counter.Rate() / int64(rateWindow/time.Second)
	}

	return State{
		RelPath:   t.relpath,
		Direction: t.direction,
		Status:    status,
		Offset:    offset,
		Size:      atomic.LoadInt64(&t.size),
		Moved:     offset - atomic.LoadInt64(&t.startOffset),
		Rate:      rate,
		Elapsed:   elapsed,
		Err:       err,
	}
}

type trackedReader struct {
	t *Tracker
	r io.Reader
}

func (r *trackedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.t.add(int64(n))
	return n, err
}

type contextKey struct{}

// WithTracker attaches a tracker to ctx so the job running under it can report
// progress without being handed one explicitly.
func WithTracker(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, contextKey{}, t)
}

// TrackerFrom returns the tracker attached to ctx. It always returns a usable
// tracker, so callers never have to nil-check: one that belongs to no registry
// simply goes unrendered.
func TrackerFrom(ctx context.Context) *Tracker {
	if t, ok := ctx.Value(contextKey{}).(*Tracker); ok {
		return t
	}
	return newTracker(Download, "", 0)
}
