package putiosync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/putdotio/putio-sync/v2/internal/progress"
)

func getStatus(t *testing.T) statusResponse {
	t.Helper()
	w := httptest.NewRecorder()
	newServer(":0").srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return got
}

func TestStatusWhenIdle(t *testing.T) {
	setRegistry(nil)
	setSyncStatus("Sync finished successfully")

	got := getStatus(t)
	if got.Status != "Sync finished successfully" {
		t.Errorf("status = %q, want what the sync loop last set", got.Status)
	}
	if got.Transfers == nil {
		t.Error("transfers is null; it should be an empty list so clients need no special case")
	}
	if len(got.Transfers) != 0 {
		t.Errorf("got %d transfers while idle", len(got.Transfers))
	}
}

func TestStatusReportsActiveTransfers(t *testing.T) {
	reg := progress.NewRegistry()
	setRegistry(reg)
	t.Cleanup(func() { setRegistry(nil) })

	down := reg.Enqueue(progress.Download, "shows/a.mkv", 1000)
	up := reg.Enqueue(progress.Upload, "b.mp4", 500)
	reg.Enqueue(progress.Download, "waiting.mkv", 2000)

	reg.Start(down)
	down.Reset(250, 1000)
	reg.Start(up)
	up.Reset(0, 500)

	got := getStatus(t)
	if want := "Syncing: 2 active, 1 queued"; got.Status != want {
		t.Errorf("status = %q, want %q", got.Status, want)
	}
	if len(got.Transfers) != 2 {
		t.Fatalf("got %d transfers, want the 2 active ones", len(got.Transfers))
	}

	first := got.Transfers[0]
	if first.Path != "shows/a.mkv" {
		t.Errorf("path = %q, want the full relative path", first.Path)
	}
	if first.Direction != "download" {
		t.Errorf("direction = %q, want download", first.Direction)
	}
	if first.Bytes != 250 || first.Total != 1000 || first.Percent != 25 {
		t.Errorf("got %d/%d (%d%%), want 250/1000 (25%%)", first.Bytes, first.Total, first.Percent)
	}
	if got.Transfers[1].Direction != "upload" {
		t.Errorf("direction = %q, want upload", got.Transfers[1].Direction)
	}

	// A finished transfer drops off the list.
	reg.Finish(down, nil)
	got = getStatus(t)
	if len(got.Transfers) != 1 {
		t.Errorf("got %d transfers after one finished, want 1", len(got.Transfers))
	}
}

// The status field predates the transfers list. Anything already reading it
// has to keep working.
func TestStatusKeepsItsOriginalShape(t *testing.T) {
	setRegistry(nil)
	setSyncStatus("Downloading \"a.mkv\"")

	w := httptest.NewRecorder()
	newServer(":0").srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))

	var legacy struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Status != `Downloading "a.mkv"` {
		t.Errorf("status = %q", legacy.Status)
	}
}

func TestSyncingEndpoint(t *testing.T) {
	for _, want := range []string{"false", "true"} {
		setSyncing(want == "true")
		w := httptest.NewRecorder()
		newServer(":0").srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/syncing", nil))
		if w.Body.String() != want {
			t.Errorf("got %q, want %q", w.Body.String(), want)
		}
	}
	setSyncing(false)
}
