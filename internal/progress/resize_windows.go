package progress

import "os"

// watchResize is a no-op on Windows, which has no resize signal. The regular
// redraw picks up the new width within a tick. Receiving from a nil channel
// blocks forever, which is what the render loop wants.
func watchResize() (chan os.Signal, func()) {
	return nil, func() {}
}
