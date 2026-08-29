package progress

import (
	"os"

	"golang.org/x/term"
)

// Renderer turns a Registry into output. It is also the registry's Listener,
// so a file appearing or completing shows up right away instead of at the next
// tick.
type Renderer interface {
	Listener

	// Start begins rendering. It must be called before any transfer starts.
	Start()
	// Stop ends rendering and leaves the terminal as it found it.
	Stop()
}

// NewRenderer picks a renderer for the current output. A terminal gets the
// live display; anything else - systemd, Docker, a CI log - gets periodic log
// lines, which carry the same events without any cursor movement.
func NewRenderer(reg *Registry) Renderer {
	out := os.Stderr
	if term.IsTerminal(int(out.Fd())) {
		return newTTYRenderer(reg, out)
	}
	return newLogRenderer(reg)
}

// terminalWidth returns the usable width of f, falling back to a conventional
// 80 columns when the size cannot be determined.
func terminalWidth(f *os.File) int {
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
