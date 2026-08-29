package progress

import "sync"

// Listener is notified as transfers start and finish. Renderers implement it
// so that a file appearing or completing shows up immediately, rather than at
// the next tick.
type Listener interface {
	Started(State)
	Finished(State)
}

// Registry holds every transfer in a sync pass. It is the single source of
// truth for the renderers and for the HTTP status endpoint.
type Registry struct {
	mu       sync.RWMutex
	trackers []*Tracker
	listener Listener
}

func NewRegistry() *Registry {
	return &Registry{}
}

// SetListener registers the renderer. It must be called before any transfer
// starts.
func (r *Registry) SetListener(l Listener) {
	r.mu.Lock()
	r.listener = l
	r.mu.Unlock()
}

// Enqueue adds a file that is going to be transferred. Knowing the whole queue
// up front is what lets the display say how much work is left.
func (r *Registry) Enqueue(direction Direction, relpath string, size int64) *Tracker {
	t := newTracker(direction, relpath, size)
	r.mu.Lock()
	r.trackers = append(r.trackers, t)
	r.mu.Unlock()
	return t
}

// Start marks a queued transfer as running.
func (r *Registry) Start(t *Tracker) {
	t.start()
	r.notify(func(l Listener) { l.Started(t.state()) })
}

// Finish marks a transfer as done, or failed if err is non-nil.
func (r *Registry) Finish(t *Tracker, err error) {
	t.finish(err)
	r.notify(func(l Listener) { l.Finished(t.state()) })
}

// notify calls the listener without holding the lock, so a listener is free to
// call back into Snapshot.
func (r *Registry) notify(fn func(Listener)) {
	r.mu.RLock()
	l := r.listener
	r.mu.RUnlock()
	if l != nil {
		fn(l)
	}
}

// Snapshot is a consistent view of the whole pass.
type Snapshot struct {
	// Active transfers, in the order they were queued.
	Active []State
	// All transfers, in the order they were queued.
	All []State

	Queued int
	Done   int
	Failed int

	// BytesRemaining covers queued and active transfers.
	BytesRemaining int64
	// Rate is the combined speed of the active transfers, in bytes per second.
	Rate int64
}

// Total is the number of files in the pass.
func (s Snapshot) Total() int {
	return len(s.All)
}

func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	trackers := make([]*Tracker, len(r.trackers))
	copy(trackers, r.trackers)
	r.mu.RUnlock()

	s := Snapshot{All: make([]State, 0, len(trackers))}
	for _, t := range trackers {
		st := t.state()
		s.All = append(s.All, st)
		switch st.Status {
		case Queued:
			s.Queued++
			s.BytesRemaining += st.Remaining()
		case Active:
			s.Active = append(s.Active, st)
			s.BytesRemaining += st.Remaining()
			s.Rate += st.Rate
		case Done:
			s.Done++
		case Failed:
			s.Failed++
		}
	}
	return s
}
