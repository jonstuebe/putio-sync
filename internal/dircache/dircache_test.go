package dircache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/putdotio/go-putio"
)

// fakeAPI serves /v2/files/create-folder, records every folder created and
// hands out a fresh ID for each call.
type fakeAPI struct {
	mu      sync.Mutex
	created []string // "parentID/name", in call order
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v2/files/create-folder" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.created = append(a.created, r.PostForm.Get("parent_id")+"/"+r.PostForm.Get("name"))
	id := 1000 + len(a.created)
	a.mu.Unlock()
	// Sleep so a racing goroutine has a real chance to enter the critical
	// section while this call is in flight.
	time.Sleep(5 * time.Millisecond)
	fmt.Fprintf(w, `{"status":"OK","file":{"id":%d,"name":%q,"content_type":"application/x-directory"}}`, id, r.PostForm.Get("name"))
}

func newTestCache(t *testing.T) (*DirCache, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	client := putio.NewClient(srv.Client())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = u
	return New(client, 5*time.Second, 1), api
}

func TestMkdirpConcurrent(t *testing.T) {
	c, api := newTestCache(t)

	paths := []string{
		"a/b/c",
		"a/b/d",
		"a/b/c",
		"a/e",
		"a/b/c/f",
		"a",
	}

	var wg sync.WaitGroup
	ids := make([]int64, len(paths))
	errs := make([]error, len(paths))
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			ids[i], errs[i] = c.Mkdirp(context.Background(), p)
		}(i, p)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Mkdirp(%q): %v", paths[i], err)
		}
	}

	// Same path must resolve to the same ID.
	if ids[0] != ids[2] {
		t.Errorf("Mkdirp(%q) returned %d and %d", paths[0], ids[0], ids[2])
	}

	// Each distinct folder must be created exactly once, no matter the
	// interleaving.
	seen := make(map[string]int)
	for _, f := range api.created {
		seen[f]++
	}
	// Six distinct folders: a, a/b, a/b/c, a/b/d, a/e, a/b/c/f.
	if len(api.created) != 6 {
		t.Errorf("got %d CreateFolder calls %v, want 6", len(api.created), api.created)
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("folder %q created %d times", f, n)
		}
	}
}

func TestMkdirpRoot(t *testing.T) {
	c, api := newTestCache(t)
	for _, p := range []string{".", "", "/"} {
		id, err := c.Mkdirp(context.Background(), p)
		if err != nil {
			t.Fatalf("Mkdirp(%q): %v", p, err)
		}
		if id != 1 {
			t.Errorf("Mkdirp(%q) = %d, want the root folder ID 1", p, id)
		}
	}
	if len(api.created) != 0 {
		t.Errorf("root lookups hit the API: %v", api.created)
	}
}

func TestMkdirpUsesSetEntries(t *testing.T) {
	c, api := newTestCache(t)
	c.Set("a/b/", 42)
	id, err := c.Mkdirp(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("got %d, want 42", id)
	}
	if len(api.created) != 0 {
		t.Errorf("cached path hit the API: %v", api.created)
	}
}
