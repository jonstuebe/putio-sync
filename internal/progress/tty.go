package progress

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cenkalti/log"
)

const (
	// redrawInterval is how often the bars are repainted. Fast enough to look
	// live, slow enough not to be the reason the CPU fan spins up.
	redrawInterval = 200 * time.Millisecond

	// maxBars is how many transfers are shown at once. Beyond this the display
	// stops being readable and starts being a wall.
	maxBars = 10

	maxBarWidth  = 18
	minBarWidth  = 6
	minNameWidth = 12
	maxNameWidth = 48

	// columnsWidth is a bar line without the name or the bar: the indent, the
	// direction arrow, the four right-hand columns, and the spaces between
	// them. What is left over is split between the name and the bar.
	columnsWidth = 5 + 1 + 4 + 2 + 13 + 2 + 10 + 2 + 7

	// compactWidth is the same for the narrow layout, which drops the bar and
	// the byte counts.
	compactWidth = 5 + 4 + 2 + 10
)

const (
	ansiClearDown  = "\x1b[J"
	ansiHideCursor = "\x1b[?25l"
	ansiShowCursor = "\x1b[?25h"
)

// ttyRenderer draws a live region at the bottom of the terminal: one bar per
// active transfer plus a summary. Completed files are printed above it as
// ordinary lines and scroll away, so the terminal keeps a normal scrollback.
//
// Everything that writes to the terminal goes through mu, including log
// records, which are parked: the region is erased, the record is written, the
// region is drawn again.
type ttyRenderer struct {
	reg *Registry
	out *os.File

	mu        sync.Mutex
	liveLines int      // lines currently occupied by the live region
	completed []string // finished transfers waiting to be printed above it
	stopped   bool

	once     sync.Once
	stop     chan struct{}
	done     chan struct{}
	prevLog  log.Handler
	sigC     chan os.Signal
	winchC   chan os.Signal
	winchOff func()
}

func newTTYRenderer(reg *Registry, out *os.File) *ttyRenderer {
	return &ttyRenderer{
		reg:  reg,
		out:  out,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (r *ttyRenderer) Start() {
	// Park log output above the live region rather than letting it land in
	// the middle of the bars.
	r.prevLog = log.DefaultHandler
	log.DefaultLogger.SetHandler(&parkingHandler{r: r, next: r.prevLog})

	r.winchC, r.winchOff = watchResize()

	// log.Fatal exits without running deferred functions, and a signal can
	// stop the process anywhere. Either way the terminal must not be left
	// without a cursor.
	r.sigC = make(chan os.Signal, 1)
	signal.Notify(r.sigC, os.Interrupt, syscall.SIGTERM)

	r.write(ansiHideCursor)
	go r.loop()
}

func (r *ttyRenderer) Stop() {
	r.once.Do(func() { close(r.stop) })
	<-r.done
}

func (r *ttyRenderer) loop() {
	defer close(r.done)
	t := time.NewTicker(redrawInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.redraw()
		case <-r.winchC:
			r.redraw()
		case <-r.sigC:
			r.teardown()
			return
		case <-r.stop:
			r.teardown()
			return
		}
	}
}

func (r *ttyRenderer) teardown() {
	signal.Stop(r.sigC)
	if r.winchOff != nil {
		r.winchOff()
	}
	if r.prevLog != nil {
		log.DefaultLogger.SetHandler(r.prevLog)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLocked()
	r.flushCompletedLocked()
	r.stopped = true
	r.writeLocked(ansiShowCursor)
}

// Started is called when a transfer begins. The next tick picks it up; there
// is nothing to do but let the bars appear.
func (r *ttyRenderer) Started(State) {}

// Finished queues the permanent line for a completed transfer and repaints
// immediately, so a file that finishes does not appear to hang around until
// the next tick.
func (r *ttyRenderer) Finished(s State) {
	r.mu.Lock()
	r.completed = append(r.completed, completionLine(s))
	r.mu.Unlock()
	r.redraw()
}

func (r *ttyRenderer) redraw() {
	lines := r.buildLines()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.eraseLocked()
	r.flushCompletedLocked()
	r.drawLocked(lines)
}

// eraseLocked removes the live region, leaving the cursor where the region
// started.
func (r *ttyRenderer) eraseLocked() {
	if r.liveLines == 0 {
		return
	}
	r.writeLocked(fmt.Sprintf("\x1b[%dA\r%s", r.liveLines, ansiClearDown))
	r.liveLines = 0
}

// flushCompletedLocked prints the finished transfers as permanent lines. They
// scroll away with the rest of the terminal history.
func (r *ttyRenderer) flushCompletedLocked() {
	if len(r.completed) == 0 {
		return
	}
	var b strings.Builder
	for _, line := range r.completed {
		b.WriteString(line)
		b.WriteString("\n")
	}
	r.completed = r.completed[:0]
	r.writeLocked(b.String())
}

func (r *ttyRenderer) drawLocked(lines []string) {
	if len(lines) == 0 {
		return
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	r.writeLocked(b.String())
	r.liveLines = len(lines)
}

func (r *ttyRenderer) write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(s)
}

// writeLocked is the only place this package writes to the terminal. Errors
// are dropped on purpose: a renderer that logged its own failures would
// re-enter the handler that parks it.
func (r *ttyRenderer) writeLocked(s string) {
	_, _ = r.out.WriteString(s)
}

// buildLines renders the live region: a bar per active transfer, then a
// summary. It reads the registry without holding the render lock.
func (r *ttyRenderer) buildLines() []string {
	return renderRegion(r.reg.Snapshot(), terminalWidth(r.out))
}

func renderRegion(s Snapshot, width int) []string {
	if len(s.Active) == 0 && s.Queued == 0 {
		return nil
	}

	lines := make([]string, 0, maxBars+2)

	shown := s.Active
	if len(shown) > maxBars {
		shown = shown[:maxBars]
	}
	for _, st := range shown {
		lines = append(lines, barLine(st, width))
	}
	if hidden := len(s.Active) - len(shown); hidden > 0 {
		lines = append(lines, fmt.Sprintf("  …and %d more", hidden))
	}
	lines = append(lines, "  "+strings.Repeat("─", clamp(width-4, 8, 70)))
	lines = append(lines, summaryLine(s))

	// Last line of defence. A wrapped line makes the region taller than the
	// renderer thinks it is, and from then on it erases the wrong rows.
	for i, line := range lines {
		lines[i] = clipLine(line, width)
	}
	return lines
}

func clipLine(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) <= width {
		return s
	}
	return string(r[:width])
}

func summaryLine(s Snapshot) string {
	parts := []string{
		fmt.Sprintf("%d active", len(s.Active)),
		fmt.Sprintf("%d done", s.Done),
		fmt.Sprintf("%d queued", s.Queued),
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	parts = append(parts,
		humanBytes(s.BytesRemaining)+" left",
		humanRate(s.Rate),
	)
	return "  " + strings.Join(parts, " · ")
}

func barLine(s State, width int) string {
	// ↓ and ↑ rather than the heavier ⬇ and ⬆: those are East Asian Ambiguous
	// and some terminals give them two cells, which pulls every column out of
	// alignment.
	icon := "↓"
	if s.Direction == Upload {
		icon = "↑"
	}
	// The bar gives up width to keep the name readable, and disappears
	// entirely before the numbers do.
	barW := clamp(width-columnsWidth-minNameWidth, 0, maxBarWidth)
	if barW < minBarWidth {
		nameWidth := clamp(width-compactWidth, minNameWidth, maxNameWidth)
		return fmt.Sprintf("  %s %s %s %s", icon,
			padRight(truncateMiddle(shortName(s.RelPath), nameWidth), nameWidth),
			padLeft(fmt.Sprintf("%d%%", s.Percent()), 4),
			padLeft(humanRate(s.Rate), 10))
	}

	nameWidth := clamp(width-columnsWidth-barW, minNameWidth, maxNameWidth)
	name := padRight(truncateMiddle(shortName(s.RelPath), nameWidth), nameWidth)

	return fmt.Sprintf("  %s %s %s %s  %s  %s  %s",
		icon,
		name,
		bar(s.Percent(), barW),
		padLeft(fmt.Sprintf("%d%%", s.Percent()), 4),
		padLeft(humanBytePair(s.Offset, s.Size), 13),
		padLeft(humanRate(s.Rate), 10),
		padLeft(formatETA(s.Remaining(), s.Rate), 7),
	)
}

func completionLine(s State) string {
	name := shortName(s.RelPath)
	if s.Status == Failed {
		return fmt.Sprintf("✗ %s  %s", name, s.Err)
	}
	return fmt.Sprintf("✓ %s  %s in %s", name, humanBytes(s.Size), humanDuration(s.Elapsed))
}

// shortName is the file's own name. The directory is dropped: on a bar that
// has room for 40 characters, the name is what tells files apart.
func shortName(relpath string) string {
	if i := strings.LastIndexByte(relpath, '/'); i >= 0 {
		return relpath[i+1:]
	}
	return relpath
}

func bar(percent, width int) string {
	filled := percent * width / 100
	filled = clamp(filled, 0, width)
	var b bytes.Buffer
	for i := 0; i < width; i++ {
		if i < filled {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	return b.String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parkingHandler writes a log record above the live region: erase, delegate,
// draw. It is synchronous, so log ordering is exactly what it would be without
// a renderer in the way.
type parkingHandler struct {
	r    *ttyRenderer
	next log.Handler
}

func (h *parkingHandler) SetFormatter(f log.Formatter) { h.next.SetFormatter(f) }
func (h *parkingHandler) SetLevel(l log.Level)         { h.next.SetLevel(l) }
func (h *parkingHandler) Close() error                 { return h.next.Close() }

func (h *parkingHandler) Handle(rec *log.Record) {
	lines := h.r.buildLines()

	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	h.r.eraseLocked()
	h.r.flushCompletedLocked()
	h.next.Handle(rec)
	if !h.r.stopped {
		h.r.drawLocked(lines)
	}
}
