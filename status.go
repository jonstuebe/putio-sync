package putiosync

import (
	"fmt"
	"sync"

	"github.com/putdotio/putio-sync/v2/internal/progress"
)

// Sync status, read by the HTTP server on its own goroutine and written by
// whichever goroutine is running jobs.
//
// This is a placeholder shape: the progress registry will become the single
// source of truth for what is happening, and these will be derived from it.
var (
	statusMu   sync.RWMutex
	syncing    bool
	syncStatus = "Starting sync..."
	registry   *progress.Registry
)

func setSyncing(v bool) {
	statusMu.Lock()
	syncing = v
	statusMu.Unlock()
}

func isSyncing() bool {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return syncing
}

func setSyncStatus(s string) {
	statusMu.Lock()
	syncStatus = s
	statusMu.Unlock()
}

func getSyncStatus() string {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return syncStatus
}

// setRegistry publishes the current pass's transfers so the HTTP server can
// report them. Passing nil marks the end of the pass.
func setRegistry(r *progress.Registry) {
	statusMu.Lock()
	registry = r
	statusMu.Unlock()
}

func getRegistry() *progress.Registry {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return registry
}

// currentStatus is the one-line summary reported by /status. While files are
// moving it describes the queue; otherwise it is whatever the sync loop last
// set, such as "Sync finished successfully".
func currentStatus() string {
	reg := getRegistry()
	if reg == nil {
		return getSyncStatus()
	}
	s := reg.Snapshot()
	if len(s.Active) == 0 {
		return getSyncStatus()
	}
	return fmt.Sprintf("Syncing: %d active, %d queued", len(s.Active), s.Queued)
}
