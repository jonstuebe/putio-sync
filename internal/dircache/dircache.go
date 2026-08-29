package dircache

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/log"
	"github.com/putdotio/go-putio"
)

// DirCache holds a map for accessing IDs by path.
//
// DirCache is safe for concurrent use. The mutex is held across the
// CreateFolder API call in Mkdirp on purpose: releasing it around the call
// would let two goroutines create the same remote folder twice.
type DirCache struct {
	client         *putio.Client
	requestTimeout time.Duration
	remoteFolderID int64
	mu             sync.Mutex
	m              map[string]int64
}

func New(client *putio.Client, requestTimeout time.Duration, remoteFolderID int64) *DirCache {
	return &DirCache{
		client:         client,
		requestTimeout: requestTimeout,
		remoteFolderID: remoteFolderID,
		m:              make(map[string]int64),
	}
}

func (c *DirCache) Debug() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		log.Debugln("DirCache", k, v)
	}
}

func (c *DirCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]int64)
}

func (c *DirCache) Set(relpath string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	relpath = strings.TrimRight(relpath, "/")
	c.m[relpath] = id
}

func (c *DirCache) Mkdirp(ctx context.Context, relpath string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mkdirp(ctx, relpath)
}

// mkdirp must be called with c.mu held.
func (c *DirCache) mkdirp(ctx context.Context, relpath string) (int64, error) {
	relpath = strings.TrimRight(relpath, "/")
	log.Debugln("DirCache.Mkdirp", relpath)
	if relpath == "." || relpath == "" {
		return c.remoteFolderID, nil
	}
	if id, ok := c.m[relpath]; ok {
		return id, nil
	}
	dir, base := path.Split(relpath)
	dirID, err := c.mkdirp(ctx, dir)
	if err != nil {
		return 0, err
	}
	log.Debugf("DirCache.Mkdirp Creating remote folder %q", relpath)
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	f, err := c.client.Files.CreateFolder(ctx, base, dirID)
	if err != nil {
		return 0, err
	}
	c.m[relpath] = f.ID
	return f.ID, nil
}
