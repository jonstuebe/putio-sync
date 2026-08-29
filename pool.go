package putiosync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cenkalti/log"
	"github.com/putdotio/go-putio"
	"github.com/putdotio/putio-sync/v2/internal/auth"
)

// transferAttempts is the total number of times a transfer job is run before
// giving up on it for this pass. Both transfer jobs resume rather than
// restart, so a retry costs only the bytes that were in flight.
const transferAttempts = 3

// transferBackoff holds the delay before each retry. len(transferBackoff)
// must be transferAttempts-1.
var transferBackoff = [...]time.Duration{time.Second, 4 * time.Second}

// iTransferJob is a job that moves the contents of a single file. These are
// the only jobs that run concurrently.
type iTransferJob interface {
	iJob

	// RelPath is the path of the file being transferred, relative to the sync
	// root. Two jobs with the same RelPath never run at the same time.
	RelPath() string
}

// runTransfers runs the given jobs on a pool of concurrency workers.
//
// A failing job does not stop the pass: the failure is logged and the
// remaining jobs still run, and all failures are returned joined together at
// the end. Invalid credentials and cancellation of ctx are the exceptions;
// both abort the pass immediately, because nothing after them can succeed.
func runTransfers(ctx context.Context, jobs []iTransferJob, concurrency int) error {
	if len(jobs) == 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}
	log.Infof("Transferring %d file(s), %d at a time", len(jobs), concurrency)

	// A fatal error cancels workerCtx to wind the pass down early. The parent
	// ctx is kept separate so we can tell "we gave up" from "the caller quit".
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	inflight := newInflightSet()
	jobC := make(chan iTransferJob)

	var (
		mu    sync.Mutex
		errs  []error
		fatal error
	)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobC {
				err := runTransfer(workerCtx, inflight, job)
				if err == nil {
					continue
				}
				mu.Lock()
				switch {
				case errors.Is(err, auth.ErrInvalidCredentials):
					if fatal == nil {
						fatal = err
						cancel()
					}
				case ctx.Err() != nil:
					// The caller cancelled. Not this file's fault, and not
					// worth reporting once per in-flight file.
				default:
					log.Warningf("%s failed: %s", job.String(), err)
					errs = append(errs, fmt.Errorf("%s: %w", job.String(), err))
				}
				mu.Unlock()
			}
		}()
	}

feed:
	for _, job := range jobs {
		select {
		case jobC <- job:
		case <-workerCtx.Done():
			break feed
		}
	}
	close(jobC)
	wg.Wait()

	if fatal != nil {
		return fatal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(errs...)
}

func runTransfer(ctx context.Context, inflight *inflightSet, job iTransferJob) error {
	inflight.acquire(job.RelPath())
	defer inflight.release(job.RelPath())
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Infoln(job.String())
	return runWithRetry(ctx, job)
}

// runWithRetry runs job, retrying transient failures. Both transfer jobs
// resume from their persisted offset, so retrying is the same operation as
// resuming.
func runWithRetry(ctx context.Context, job iJob) error {
	var err error
	for attempt := 1; attempt <= transferAttempts; attempt++ {
		if attempt > 1 {
			delay := transferBackoff[attempt-2]
			log.Warningf("%s failed (attempt %d/%d): %s. Retrying in %s.", job.String(), attempt-1, transferAttempts, err, delay)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		}
		err = job.Run(ctx)
		if err == nil {
			return nil
		}
		// Our own context is gone, so the error is a symptom of that rather
		// than something a retry could fix.
		if ctx.Err() != nil {
			return err
		}
		if !isRetryable(err) {
			return err
		}
	}
	return err
}

// isRetryable reports whether err is worth another attempt. It is a
// whitelist: anything unrecognized (a full disk, a permission error, a bad
// request) fails immediately rather than being tried three times.
//
// It must only be consulted while the job's own context is still alive.
// Otherwise a cancellation from the caller looks identical to a stall.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return false
	}
	// The caller has established that our context is alive, so a cancelled
	// context here is the per-transfer stall timer firing.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// A truncated response body. The remaining bytes are still there.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var putioErr *putio.ErrorResponse
	if errors.As(err, &putioErr) {
		if putioErr.Response == nil {
			return false
		}
		code := putioErr.Response.StatusCode
		return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
	}
	var statusErr *unexpectedStatusError
	if errors.As(err, &statusErr) {
		return statusErr.code == http.StatusTooManyRequests || statusErr.code >= http.StatusInternalServerError
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// inflightSet tracks which files are currently being transferred, so the same
// path is never worked on by two workers at once. reconciliation emits at most
// one job per path per pass, which makes this a guard rather than a
// bottleneck: acquire blocks instead of skipping, so a duplicate is delayed,
// never dropped.
//
// The scope is this process only. Two putio-sync processes sharing a local dir
// are kept apart by the exclusive lock on the database file.
type inflightSet struct {
	cond *sync.Cond
	m    map[string]struct{}
}

func newInflightSet() *inflightSet {
	return &inflightSet{
		cond: sync.NewCond(&sync.Mutex{}),
		m:    make(map[string]struct{}),
	}
}

func (s *inflightSet) acquire(relpath string) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	for {
		if _, ok := s.m[relpath]; !ok {
			s.m[relpath] = struct{}{}
			return
		}
		log.Debugf("Waiting for in-flight transfer of %q", relpath)
		s.cond.Wait()
	}
}

func (s *inflightSet) release(relpath string) {
	s.cond.L.Lock()
	delete(s.m, relpath)
	s.cond.L.Unlock()
	s.cond.Broadcast()
}
