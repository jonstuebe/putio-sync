package progress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingListener struct {
	mu       sync.Mutex
	started  []string
	finished []State
}

func (l *recordingListener) Started(s State) {
	l.mu.Lock()
	l.started = append(l.started, s.RelPath)
	l.mu.Unlock()
}

func (l *recordingListener) Finished(s State) {
	l.mu.Lock()
	l.finished = append(l.finished, s)
	l.mu.Unlock()
}

func TestTrackerCountsBytesRead(t *testing.T) {
	tr := NewTracker(Download, "a.mkv", 100)
	tr.Reset(20, 100)

	r := tr.Wrap(bytes.NewReader(make([]byte, 30)))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}

	s := tr.state()
	if s.Offset != 50 {
		t.Errorf("offset = %d, want 50: the resumed bytes plus the ones just read", s.Offset)
	}
	if s.Moved != 30 {
		t.Errorf("moved = %d, want 30: a resumed transfer does not claim the earlier attempt's bytes", s.Moved)
	}
	if s.Percent() != 50 {
		t.Errorf("percent = %d, want 50", s.Percent())
	}
	if s.Remaining() != 50 {
		t.Errorf("remaining = %d, want 50", s.Remaining())
	}
}

func TestTrackerResetOnRetry(t *testing.T) {
	tr := NewTracker(Download, "a.mkv", 100)
	tr.Reset(0, 100)
	_, _ = io.Copy(io.Discard, tr.Wrap(bytes.NewReader(make([]byte, 40))))

	// The attempt failed at 40 bytes; the next one resumes from what was
	// persisted rather than starting the bar over.
	tr.Reset(40, 100)
	_, _ = io.Copy(io.Discard, tr.Wrap(bytes.NewReader(make([]byte, 10))))

	s := tr.state()
	if s.Offset != 50 {
		t.Errorf("offset = %d, want 50", s.Offset)
	}
	if s.Moved != 10 {
		t.Errorf("moved = %d, want 10 for this attempt", s.Moved)
	}
}

func TestTrackerPercentWithUnknownSize(t *testing.T) {
	tr := NewTracker(Download, "a.mkv", 0)
	if got := tr.state().Percent(); got != 0 {
		t.Errorf("percent = %d, want 0 rather than a division by zero", got)
	}
}

func TestRegistryCountsEveryState(t *testing.T) {
	reg := NewRegistry()
	l := &recordingListener{}
	reg.SetListener(l)

	done := reg.Enqueue(Download, "done.mkv", 100)
	failed := reg.Enqueue(Upload, "failed.mkv", 200)
	active := reg.Enqueue(Download, "active.mkv", 300)
	queued := reg.Enqueue(Download, "queued.mkv", 400)

	reg.Start(done)
	done.Reset(0, 100)
	_, _ = io.Copy(io.Discard, done.Wrap(bytes.NewReader(make([]byte, 100))))
	reg.Finish(done, nil)

	reg.Start(failed)
	reg.Finish(failed, errors.New("connection reset"))

	reg.Start(active)
	active.Reset(50, 300)

	s := reg.Snapshot()
	if s.Done != 1 || s.Failed != 1 || s.Queued != 1 || len(s.Active) != 1 {
		t.Errorf("got done=%d failed=%d queued=%d active=%d, want one of each",
			s.Done, s.Failed, s.Queued, len(s.Active))
	}
	if s.Total() != 4 {
		t.Errorf("total = %d, want 4", s.Total())
	}
	// Remaining covers what is still to do: the active file's 250 bytes and
	// the queued file's 400. The failed one is not coming back this pass.
	if s.BytesRemaining != 650 {
		t.Errorf("bytes remaining = %d, want 650", s.BytesRemaining)
	}
	if s.Active[0].RelPath != "active.mkv" {
		t.Errorf("active = %q, want active.mkv", s.Active[0].RelPath)
	}
	_ = queued

	if got := strings.Join(l.started, ","); got != "done.mkv,failed.mkv,active.mkv" {
		t.Errorf("started = %q, want the three that ran, in order", got)
	}
	if len(l.finished) != 2 {
		t.Fatalf("finished %d transfers, want 2", len(l.finished))
	}
	if l.finished[0].Status != Done || l.finished[1].Status != Failed {
		t.Errorf("finished states = %v/%v, want Done then Failed", l.finished[0].Status, l.finished[1].Status)
	}
	if l.finished[1].Err == nil {
		t.Error("the failed transfer did not carry its reason")
	}
}

// The renderer reads the registry on its own goroutine while transfers write
// to it. This is the shape that has to stay race-free.
func TestRegistryConcurrentUse(t *testing.T) {
	reg := NewRegistry()
	reg.SetListener(&recordingListener{})

	var wg, renderWG sync.WaitGroup
	stop := make(chan struct{})

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	renderWG.Add(1)
	go func() {
		defer renderWG.Done()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = renderRegion(reg.Snapshot(), 100)
			}
		}
	}()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr := reg.Enqueue(Download, "file.mkv", 1000)
			reg.Start(tr)
			tr.Reset(0, 1000)
			_, _ = io.Copy(io.Discard, tr.Wrap(bytes.NewReader(make([]byte, 1000))))
			reg.Finish(tr, nil)
		}(i)
	}

	wg.Wait()
	close(stop)
	renderWG.Wait()

	if s := reg.Snapshot(); s.Done != 20 {
		t.Errorf("done = %d, want 20", s.Done)
	}
}

func TestTrackerFromContextAlwaysReturnsOne(t *testing.T) {
	// A job run outside a pass still has somewhere to report to.
	tr := TrackerFrom(context.Background())
	if tr == nil {
		t.Fatal("got nil, want a usable tracker")
	}
	tr.Reset(0, 10)
	if _, err := io.Copy(io.Discard, tr.Wrap(bytes.NewReader(make([]byte, 10)))); err != nil {
		t.Fatal(err)
	}

	want := NewTracker(Upload, "a.mkv", 1)
	if got := TrackerFrom(WithTracker(context.Background(), want)); got != want {
		t.Error("the tracker attached to the context did not come back")
	}
}
