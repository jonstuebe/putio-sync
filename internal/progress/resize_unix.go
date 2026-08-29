//go:build !windows

package progress

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize returns a channel that fires when the terminal is resized, and a
// function to stop watching.
func watchResize() (chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
