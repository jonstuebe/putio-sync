package progress

import (
	"sync"
	"time"

	"github.com/cenkalti/log"
)

// aggregateInterval is how often the non-terminal renderer reports overall
// progress. Rare enough not to fill a journal, frequent enough that a daemon
// four hours into a large transfer does not look dead.
const aggregateInterval = 30 * time.Second

// logRenderer reports progress as ordinary log lines. It is what runs under
// systemd, in a container, or with output redirected to a file.
type logRenderer struct {
	reg *Registry

	once sync.Once
	stop chan struct{}
	done chan struct{}
}

func newLogRenderer(reg *Registry) *logRenderer {
	return &logRenderer{
		reg:  reg,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (r *logRenderer) Start() {
	go r.loop()
}

func (r *logRenderer) Stop() {
	r.once.Do(func() { close(r.stop) })
	<-r.done
}

func (r *logRenderer) loop() {
	defer close(r.done)
	t := time.NewTicker(aggregateInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.reportAggregate()
		case <-r.stop:
			return
		}
	}
}

func (r *logRenderer) reportAggregate() {
	s := r.reg.Snapshot()
	if len(s.Active) == 0 {
		return
	}
	log.Infof("Syncing: %d active, %d queued, %s left, %s",
		len(s.Active), s.Queued, humanBytes(s.BytesRemaining), humanRate(s.Rate))
}

func (r *logRenderer) Started(s State) {
	log.Infof("%s %q (%s)", s.Direction, s.RelPath, humanBytes(s.Size))
}

func (r *logRenderer) Finished(s State) {
	if s.Status == Failed {
		// The pool reports failures, with the retry count and the error.
		return
	}
	verb := "Downloaded"
	if s.Direction == Upload {
		verb = "Uploaded"
	}
	log.Infof("%s %q (%s in %s, %s)", verb, s.RelPath, humanBytes(s.Size),
		humanDuration(s.Elapsed), averageRate(s))
}

// averageRate is the speed over the whole transfer, which is a fairer summary
// than whatever the 5s window happened to hold at the end.
func averageRate(s State) string {
	secs := s.Elapsed.Seconds()
	if secs < 1 {
		return humanRate(s.Moved)
	}
	return humanRate(int64(float64(s.Moved) / secs))
}
