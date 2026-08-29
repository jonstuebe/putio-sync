package putiosync

import "sync"

// Sync status, read by the HTTP server on its own goroutine and written by
// whichever goroutine is running jobs.
//
// This is a placeholder shape: the progress registry will become the single
// source of truth for what is happening, and these will be derived from it.
var (
	statusMu   sync.RWMutex
	syncing    bool
	syncStatus = "Starting sync..."
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
