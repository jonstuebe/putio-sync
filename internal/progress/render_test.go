package progress

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var errFake = errors.New("connection reset")

func activeState(relpath string, offset, size, rate int64) State {
	return State{
		RelPath: relpath, Direction: Download, Status: Active,
		Offset: offset, Size: size, Rate: rate, Elapsed: time.Minute,
	}
}

func TestRenderRegionFitsTheTerminal(t *testing.T) {
	s := Snapshot{
		Active: []State{
			activeState("shows/Big.Buck.Bunny.2160p.remastered.mkv", 4509715660, 7838315479, 11953766),
			activeState("show.s01e02.mkv", 1181116006, 3758096384, 7130316),
		},
		All:            make([]State, 20),
		Queued:         5,
		Done:           12,
		BytesRemaining: 202937204736,
		Rate:           21285437,
	}
	s.Active[1].Direction = Upload

	for _, width := range []int{200, 120, 100, 80, 70, 60, 40, 20} {
		lines := renderRegion(s, width)
		if len(lines) == 0 {
			t.Fatalf("width %d: nothing rendered", width)
		}
		for _, line := range lines {
			if n := len([]rune(line)); n > width {
				t.Errorf("width %d: line is %d characters and will wrap: %q", width, n, line)
			}
		}
		if testing.Verbose() {
			t.Logf("width %d:\n%s", width, strings.Join(lines, "\n"))
		}
	}
}

func TestRenderRegionCapsTheNumberOfBars(t *testing.T) {
	var s Snapshot
	for i := 0; i < maxBars+7; i++ {
		s.Active = append(s.Active, activeState("file.mkv", 1, 2, 3))
	}
	lines := renderRegion(s, 120)
	// maxBars bars, the overflow row, the rule, the summary.
	if len(lines) != maxBars+3 {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), maxBars+3, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[maxBars], "and 7 more") {
		t.Errorf("overflow row is %q, want it to mention the 7 hidden transfers", lines[maxBars])
	}
}

func TestRenderRegionEmptyWhenNothingToDo(t *testing.T) {
	if lines := renderRegion(Snapshot{Done: 3}, 100); lines != nil {
		t.Errorf("got %q, want no live region once everything is finished", lines)
	}
}

func TestSummaryLineReportsFailuresOnlyWhenThereAreSome(t *testing.T) {
	quiet := summaryLine(Snapshot{Active: []State{{}}, Done: 2, Queued: 1})
	if strings.Contains(quiet, "failed") {
		t.Errorf("%q mentions failures when there are none", quiet)
	}
	loud := summaryLine(Snapshot{Active: []State{{}}, Done: 2, Queued: 1, Failed: 3})
	if !strings.Contains(loud, "3 failed") {
		t.Errorf("%q does not report the failures", loud)
	}
}

func TestCompletionLine(t *testing.T) {
	done := completionLine(State{
		RelPath: "shows/The.Bear.S03E01.mkv", Status: Done,
		Size: 2254857830, Elapsed: 3*time.Minute + 12*time.Second,
	})
	if want := "✓ The.Bear.S03E01.mkv  2.1 GB in 3m12s"; done != want {
		t.Errorf("got %q, want %q", done, want)
	}

	failed := completionLine(State{RelPath: "notes.pdf", Status: Failed, Err: errFake})
	if !strings.HasPrefix(failed, "✗ notes.pdf") || !strings.Contains(failed, errFake.Error()) {
		t.Errorf("got %q, want the name and the reason", failed)
	}
}

func TestBar(t *testing.T) {
	if got, want := bar(0, 4), "░░░░"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := bar(50, 4), "██░░"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := bar(100, 4), "████"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A file that reports more bytes than it claimed must not overflow the bar.
	if got, want := bar(140, 4), "████"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestShortName(t *testing.T) {
	if got, want := shortName("a/b/c.mkv"), "c.mkv"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := shortName("c.mkv"), "c.mkv"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
