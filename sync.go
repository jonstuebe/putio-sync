package putiosync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/adrg/xdg"
	"github.com/cenkalti/log"
	"github.com/putdotio/go-putio"
	"github.com/putdotio/putio-sync/v2/internal/auth"
	"github.com/putdotio/putio-sync/v2/internal/dircache"
	"github.com/putdotio/putio-sync/v2/internal/progress"
	"github.com/putdotio/putio-sync/v2/internal/tmpdir"
	"github.com/putdotio/putio-sync/v2/internal/updates"
	"github.com/putdotio/putio-sync/v2/internal/walker"
	"github.com/putdotio/putio-sync/v2/internal/watcher"
	"go.etcd.io/bbolt"
)

const (
	// defaultTimeout is the timeout for a single API request.
	defaultTimeout = 10 * time.Second

	// transferStallTimeout is how long a transfer may go without receiving any
	// bytes before it is considered stalled and cancelled. It is deliberately
	// separate from defaultTimeout: with several transfers sharing a link, a
	// merely starved stream can go quiet for a while without being dead.
	transferStallTimeout = 60 * time.Second

	// dbOpenTimeout is how long to wait for the exclusive lock on the database
	// file before reporting that another instance is running.
	dbOpenTimeout = 5 * time.Second
)

var ErrInvalidCredentials = errors.New("invalid credentials")

// Variables that used by Sync function.
var (
	cfg            Config
	db             *bbolt.DB
	token          string
	client         *putio.Client
	notifier       = updates.NewNotifier("wss://socket.put.io/socket/sockjs/websocket", 10*time.Second, 5*time.Second)
	watcherUpdates chan string
	localPath      string
	remoteFolderID int64
	dirCache       *dircache.DirCache
	tempDirPath    string
	triggerSyncC   = make(chan struct{}, 1)
)

func Sync(ctx context.Context, config Config) error {
	config.setDefaults()
	if err := config.validate(); err != nil {
		return err
	}
	if config.Debug {
		log.SetLevel(log.DEBUG)
	}
	dbPath, err := xdg.DataFile(filepath.Join("putio-sync", "sync.db"))
	if err != nil {
		return err
	}
	log.Infof("Using database file %q", dbPath)
	db, err = bbolt.Open(dbPath, 0666, &bbolt.Options{Timeout: dbOpenTimeout})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return fmt.Errorf("cannot lock database file %q: another instance is already running", dbPath)
		}
		return err
	}
	defer db.Close()
	cfg = config
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketFiles)
		return err
	})
	if err != nil {
		return err
	}
	var srv *httpServer
	if cfg.Server != "" {
		srv = newServer(cfg.Server)
		srv.Start()
		defer srv.Close()
	}

	for {
		err = syncOnce(ctx)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			return ErrInvalidCredentials
		}
		if err != nil {
			if cfg.Once {
				return err
			}
			log.Error(err)
		} else {
			setSyncStatus("Sync finished successfully")
			log.Infoln(getSyncStatus())
		}
		ok := waitNextSync(ctx)
		if !ok {
			break
		}
	}
	if srv != nil {
		if err := srv.Shutdown(); err != nil {
			return err
		}
		log.Debug("Server has shutdown successfully")
	}
	return nil
}

func syncOnce(ctx context.Context) error {
	var err error
	token, client, err = auth.Authenticate(ctx, httpClient, defaultTimeout, cfg.Username, cfg.Password)
	if err != nil {
		return err
	}
	err = ensureRoots(ctx)
	if err != nil {
		return err
	}
	tempDirPath, err = tmpdir.Create(localPath)
	if err != nil {
		return err
	}
	dirCache = dircache.New(client, defaultTimeout, remoteFolderID)
	if !cfg.Once {
		notifier.SetToken(token)
		notifier.Start()
	}
	if watcherUpdates == nil {
		watcherUpdates, err = watcher.Watch(ctx, localPath)
		if err != nil {
			log.Error(err)
		}
	}
	return syncRoots(ctx)
}

func syncRoots(ctx context.Context) error {
	remoteURL := fmt.Sprintf("https://put.io/files/%d", remoteFolderID)
	log.Infof("Syncing %q with %q", remoteURL, localPath)

	// Read previous sync state from db.
	states, err := readAllStates()
	if err != nil {
		return err
	}

	// Walk on local and remote folders in parallel
	w := walker.Walker{
		LocalPath:      localPath,
		RemoteFolderID: remoteFolderID,
		TempDirName:    tmpdir.Name,
		Client:         client,
		RequestTimeout: defaultTimeout,
	}
	localFiles, remoteFiles, err := w.Walk(ctx)
	if err != nil {
		return err
	}

	// Set DirCache entries for existing remote folders
	for _, rf := range remoteFiles {
		if rf.PutioFile().IsDir() {
			dirCache.Set(rf.RelPath(), rf.PutioFile().ID)
		}
	}

	// Calculate what needs to be done
	syncFiles := groupFiles(states, localFiles, remoteFiles)
	filterOutInvalidNames(syncFiles)
	jobs := reconciliation(syncFiles)

	// Print jobs for debugging
	for _, job := range jobs {
		log.Debugln("Job:", job.String())
	}
	// dirCache.Debug()

	// Run all jobs one by one
	if cfg.DryRun {
		log.Noticeln("Command run in dry-run mode, no changes will be made")
	}
	if len(jobs) == 0 {
		log.Infoln("No changes detected")
		return nil
	}
	setSyncing(true)
	defer setSyncing(false)

	// Metadata jobs run first, one at a time, in the order reconciliation put
	// them in. They are millisecond-scale API and filesystem calls, so there
	// is nothing to win by running them concurrently, and running them first
	// means a folder always exists before anything is transferred into it.
	metadata, transfers := splitJobs(jobs)
	for _, job := range metadata {
		setSyncStatus(job.String())
		log.Infoln(job.String())
		if cfg.DryRun {
			continue
		}
		if err := job.Run(ctx); err != nil {
			setSyncStatus("Error: " + err.Error())
			return err
		}
	}

	if cfg.DryRun {
		for _, job := range transfers {
			log.Infoln(job.String())
		}
		return nil
	}

	setSyncStatus(fmt.Sprintf("Transferring %d file(s)", len(transfers)))
	reg := progress.NewRegistry()
	renderer := progress.NewRenderer(reg)
	reg.SetListener(renderer)
	renderer.Start()
	defer renderer.Stop()

	setRegistry(reg)
	defer setRegistry(nil)

	err = runTransfers(ctx, transfers, cfg.Concurrency, reg)
	if err != nil {
		setSyncStatus("Error: " + err.Error())
		return err
	}
	return nil
}

// splitJobs separates the jobs that move file contents from the ones that only
// touch metadata. Only the former run concurrently.
func splitJobs(jobs []iJob) (metadata []iJob, transfers []iTransferJob) {
	for _, job := range jobs {
		switch j := job.(type) {
		case *downloadJob:
			transfers = append(transfers, j)
		case *uploadJob:
			transfers = append(transfers, j)
		default:
			metadata = append(metadata, job)
		}
	}
	return metadata, transfers
}

func waitNextSync(ctx context.Context) bool {
	if cfg.Once {
		return false
	}
	var tc <-chan time.Time
	startTimer := func() {
		if tc == nil {
			tc = time.After(5 * time.Second)
		}
	}
	var d time.Duration
	if notifier.Connected() && watcher.Recursive {
		d = 2 * time.Hour
	} else {
		d = 15 * time.Minute
	}
	for {
		select {
		case <-time.After(d):
			return true
		case name := <-notifier.HasUpdates:
			log.Debugf("Change detected at remote filesystem: %q", name)
			startTimer()
		case name := <-watcherUpdates:
			log.Debugf("Change detected at local filesystem: %q", name)
			startTimer()
		case <-triggerSyncC:
			log.Debugf("Sync triggered manually")
			return true
		case <-tc:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func triggerSync() {
	select {
	case triggerSyncC <- struct{}{}:
	default:
	}
}
