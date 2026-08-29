package progress

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cenkalti/log"
)

// newTestTTY returns a renderer writing to a file, plus a function reading
// back everything written to it. A file is not a terminal, but the renderer
// only ever writes bytes, so the escape sequences can be checked exactly.
func newTestTTY(t *testing.T) (*ttyRenderer, *Registry, func() string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	reg := NewRegistry()
	r := newTTYRenderer(reg, f)
	reg.SetListener(r)
	return r, reg, func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

func TestTTYRendererErasesExactlyWhatItDrew(t *testing.T) {
	r, reg, _ := newTestTTY(t)

	tr := reg.Enqueue(Download, "a.mkv", 1000)
	reg.Start(tr)
	tr.Reset(0, 1000)

	r.redraw()
	drawn := r.liveLines
	if drawn == 0 {
		t.Fatal("nothing was drawn")
	}

	// A second redraw must erase the first region, not add to it.
	r.redraw()
	if r.liveLines != drawn {
		t.Errorf("live region grew from %d to %d lines across redraws", drawn, r.liveLines)
	}

	r.mu.Lock()
	r.eraseLocked()
	r.mu.Unlock()
	if r.liveLines != 0 {
		t.Errorf("liveLines = %d after erasing, want 0", r.liveLines)
	}
}

func TestTTYRendererMovesUpByTheLinesItDrew(t *testing.T) {
	r, reg, output := newTestTTY(t)

	tr := reg.Enqueue(Download, "a.mkv", 1000)
	reg.Start(tr)
	r.redraw()
	n := r.liveLines
	r.redraw()

	// The second redraw has to walk back up over exactly the region it left
	// behind. Off by one here and the display eats a line of scrollback on
	// every tick.
	if want := "\x1b[" + strconv.Itoa(n) + "A"; !strings.Contains(output(), want) {
		t.Errorf("output does not contain %q, so the cursor is not being put back where the region started", want)
	}
}

func TestTTYRendererPrintsCompletedTransfersAboveTheRegion(t *testing.T) {
	r, reg, output := newTestTTY(t)

	done := reg.Enqueue(Download, "shows/done.mkv", 1000)
	rest := reg.Enqueue(Download, "rest.mkv", 1000)
	reg.Start(done)
	reg.Start(rest)
	r.redraw()

	reg.Finish(done, nil)

	out := output()
	if !strings.Contains(out, "✓ done.mkv") {
		t.Errorf("no completion line for the finished transfer:\n%s", out)
	}
	// The finished file leaves the live region; the unfinished one stays.
	lines := renderRegion(reg.Snapshot(), 100)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "done.mkv") {
		t.Errorf("finished transfer is still in the live region:\n%s", joined)
	}
	if !strings.Contains(joined, "rest.mkv") {
		t.Errorf("unfinished transfer left the live region:\n%s", joined)
	}
}

func TestTTYRendererParksLogOutput(t *testing.T) {
	r, reg, output := newTestTTY(t)

	// Start installs the log handler and the signal handlers; Stop puts them
	// back.
	saved := log.DefaultLogger
	t.Cleanup(func() { log.DefaultLogger = saved })

	tr := reg.Enqueue(Download, "a.mkv", 1000)
	reg.Start(tr)
	r.Start()
	r.redraw()

	log.Infoln("something happened")
	r.Stop()

	out := output()
	if !strings.Contains(out, ansiHideCursor) {
		t.Error("the cursor was never hidden")
	}
	if !strings.Contains(out, ansiShowCursor) {
		t.Error("the cursor was not restored on stop")
	}
	if strings.LastIndex(out, ansiShowCursor) < strings.LastIndex(out, ansiHideCursor) {
		t.Error("the cursor was hidden after it was restored")
	}
	// The log record itself goes to stderr through the wrapped handler, not
	// to the renderer's output. What must show up here is the erase that made
	// room for it.
	if !strings.Contains(out, ansiClearDown) {
		t.Error("the live region was not erased to make room for the log record")
	}
	if r.liveLines != 0 {
		t.Errorf("liveLines = %d after stopping, want the region cleared", r.liveLines)
	}
}

func TestTTYRendererStopIsIdempotent(t *testing.T) {
	r, _, _ := newTestTTY(t)
	saved := log.DefaultLogger
	t.Cleanup(func() { log.DefaultLogger = saved })

	r.Start()
	r.Stop()
	r.Stop()
}
