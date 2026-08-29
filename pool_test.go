package putiosync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/putdotio/go-putio"
	"github.com/putdotio/putio-sync/v2/internal/auth"
	"github.com/putdotio/putio-sync/v2/internal/progress"
)

// fakeJob is a transfer job whose Run is supplied by the test.
type fakeJob struct {
	relpath string
	runs    int32
	run     func(ctx context.Context, attempt int32) error
}

func (j *fakeJob) String() string                { return "Transferring " + j.relpath }
func (j *fakeJob) RelPath() string               { return j.relpath }
func (j *fakeJob) Direction() progress.Direction { return progress.Download }
func (j *fakeJob) Size() int64                   { return 1 << 20 }

func (j *fakeJob) Run(ctx context.Context) error {
	attempt := atomic.AddInt32(&j.runs, 1)
	if j.run == nil {
		return nil
	}
	return j.run(ctx, attempt)
}

// noBackoff shrinks the retry delays for the duration of a test.
func noBackoff(t *testing.T) {
	t.Helper()
	saved := transferBackoff
	for i := range transferBackoff {
		transferBackoff[i] = time.Millisecond
	}
	t.Cleanup(func() { transferBackoff = saved })
}

func TestRunTransfersRespectsConcurrency(t *testing.T) {
	const (
		concurrency = 3
		total       = 20
	)
	var (
		mu     sync.Mutex
		active int
		peak   int
	)
	jobs := make([]iTransferJob, 0, total)
	for i := 0; i < total; i++ {
		jobs = append(jobs, &fakeJob{
			relpath: fmt.Sprintf("file-%d", i),
			run: func(ctx context.Context, _ int32) error {
				mu.Lock()
				active++
				if active > peak {
					peak = active
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
				return nil
			},
		})
	}

	if err := runTransfers(context.Background(), jobs, concurrency, nil); err != nil {
		t.Fatal(err)
	}
	if peak > concurrency {
		t.Errorf("ran %d jobs at once, limit was %d", peak, concurrency)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d, expected the pool to overlap work", peak)
	}
}

func TestRunTransfersExcludesSameRelPath(t *testing.T) {
	var (
		mu       sync.Mutex
		inflight = make(map[string]int)
		overlaps int
	)
	newJob := func(relpath string) *fakeJob {
		return &fakeJob{
			relpath: relpath,
			run: func(ctx context.Context, _ int32) error {
				mu.Lock()
				inflight[relpath]++
				if inflight[relpath] > 1 {
					overlaps++
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				inflight[relpath]--
				mu.Unlock()
				return nil
			},
		}
	}

	// Several jobs for the same path plus some unrelated ones. reconciliation
	// should never produce the duplicates, but the pool must not rely on that.
	jobs := []iTransferJob{
		newJob("same"), newJob("same"), newJob("same"),
		newJob("other-1"), newJob("other-2"),
	}
	if err := runTransfers(context.Background(), jobs, 5, nil); err != nil {
		t.Fatal(err)
	}
	if overlaps != 0 {
		t.Errorf("the same path was transferred concurrently %d times", overlaps)
	}
	for _, job := range jobs {
		if n := atomic.LoadInt32(&job.(*fakeJob).runs); n != 1 {
			t.Errorf("%s ran %d times, want 1: a delayed job must not be dropped", job.String(), n)
		}
	}
}

func TestRunTransfersAggregatesFailures(t *testing.T) {
	noBackoff(t)
	errBad := errors.New("disk on fire")

	jobs := []iTransferJob{
		&fakeJob{relpath: "ok-1"},
		&fakeJob{relpath: "bad-1", run: func(context.Context, int32) error { return errBad }},
		&fakeJob{relpath: "ok-2"},
		&fakeJob{relpath: "bad-2", run: func(context.Context, int32) error { return errBad }},
	}
	err := runTransfers(context.Background(), jobs, 2, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"bad-1", "bad-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The failures must not have stopped the healthy jobs.
	for _, name := range []string{"ok-1", "ok-2"} {
		for _, job := range jobs {
			if job.RelPath() == name && atomic.LoadInt32(&job.(*fakeJob).runs) != 1 {
				t.Errorf("%s did not run", name)
			}
		}
	}
	// A non-retryable error is tried exactly once.
	for _, job := range jobs {
		if strings.HasPrefix(job.RelPath(), "bad") {
			if n := atomic.LoadInt32(&job.(*fakeJob).runs); n != 1 {
				t.Errorf("%s ran %d times, want 1", job.RelPath(), n)
			}
		}
	}
}

func TestRunTransfersRetriesTransientFailures(t *testing.T) {
	noBackoff(t)
	temporary := &net.DNSError{Err: "no route", IsTemporary: true}

	flaky := &fakeJob{relpath: "flaky", run: func(_ context.Context, attempt int32) error {
		if attempt < transferAttempts {
			return temporary
		}
		return nil
	}}
	doomed := &fakeJob{relpath: "doomed", run: func(context.Context, int32) error { return temporary }}

	err := runTransfers(context.Background(), []iTransferJob{flaky, doomed}, 2, nil)
	if err == nil {
		t.Fatal("want an error for the job that never succeeded")
	}
	if !strings.Contains(err.Error(), "doomed") {
		t.Errorf("error %q does not mention the failing job", err)
	}
	if strings.Contains(err.Error(), "flaky") {
		t.Errorf("error %q mentions the job that eventually succeeded", err)
	}
	if n := atomic.LoadInt32(&flaky.runs); n != transferAttempts {
		t.Errorf("flaky ran %d times, want %d", n, transferAttempts)
	}
	if n := atomic.LoadInt32(&doomed.runs); n != transferAttempts {
		t.Errorf("doomed ran %d times, want %d", n, transferAttempts)
	}
}

func TestRunTransfersAbortsOnInvalidCredentials(t *testing.T) {
	noBackoff(t)
	started := make(chan struct{})
	var ran int32

	jobs := []iTransferJob{
		&fakeJob{relpath: "auth", run: func(context.Context, int32) error {
			close(started)
			return auth.ErrInvalidCredentials
		}},
	}
	for i := 0; i < 50; i++ {
		jobs = append(jobs, &fakeJob{relpath: fmt.Sprintf("file-%d", i), run: func(ctx context.Context, _ int32) error {
			<-started
			atomic.AddInt32(&ran, 1)
			time.Sleep(time.Millisecond)
			return nil
		}})
	}

	err := runTransfers(context.Background(), jobs, 2, nil)
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	// Nothing after the auth failure can succeed, so the pass should stop
	// well before working through the queue.
	if n := atomic.LoadInt32(&ran); n > 10 {
		t.Errorf("%d jobs ran after the auth failure, expected the pass to abort", n)
	}
	// A credentials failure is not retried.
	if n := atomic.LoadInt32(&jobs[0].(*fakeJob).runs); n != 1 {
		t.Errorf("auth job ran %d times, want 1", n)
	}
}

func TestRunTransfersStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var ran int32

	jobs := make([]iTransferJob, 0, 50)
	for i := 0; i < 50; i++ {
		jobs = append(jobs, &fakeJob{relpath: fmt.Sprintf("file-%d", i), run: func(ctx context.Context, _ int32) error {
			// The caller quits partway through the first transfer.
			if atomic.AddInt32(&ran, 1) == 1 {
				cancel()
			}
			<-ctx.Done()
			return ctx.Err()
		}})
	}

	err := runTransfers(ctx, jobs, 2, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := atomic.LoadInt32(&ran); n > 10 {
		t.Errorf("%d jobs ran after cancellation, expected the pass to wind down", n)
	}
}

func TestRunTransfersEmpty(t *testing.T) {
	if err := runTransfers(context.Background(), nil, 4, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIsRetryable(t *testing.T) {
	putioErr := func(code int) error {
		return &putio.ErrorResponse{Response: &http.Response{StatusCode: code}}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unknown", errors.New("boom"), false},
		{"invalid credentials", auth.ErrInvalidCredentials, false},
		{"wrapped invalid credentials", fmt.Errorf("auth: %w", auth.ErrInvalidCredentials), false},
		{"stall cancellation", context.Canceled, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"truncated body", io.ErrUnexpectedEOF, true},
		{"plain EOF", io.EOF, false},
		{"network error", &net.DNSError{Err: "no route", IsTemporary: true}, true},
		{"wrapped network error", fmt.Errorf("get: %w", &net.DNSError{Err: "no route"}), true},
		{"api 500", putioErr(http.StatusInternalServerError), true},
		{"api 502", putioErr(http.StatusBadGateway), true},
		{"api 429", putioErr(http.StatusTooManyRequests), true},
		{"api 404", putioErr(http.StatusNotFound), false},
		{"api 401", putioErr(http.StatusUnauthorized), false},
		{"api response missing", &putio.ErrorResponse{}, false},
		{"download 503", &unexpectedStatusError{code: http.StatusServiceUnavailable}, true},
		{"download 404", &unexpectedStatusError{code: http.StatusNotFound}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryable(c.err); got != c.want {
				t.Errorf("isRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRunWithRetryDoesNotRetryAfterCancellation(t *testing.T) {
	noBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// context.Canceled is retryable on its own, because that is what a stall
	// looks like. It must not be retried when our own context is the one that
	// is gone.
	job := &fakeJob{relpath: "x", run: func(context.Context, int32) error { return context.Canceled }}
	if err := runWithRetry(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := atomic.LoadInt32(&job.runs); n != 1 {
		t.Errorf("ran %d times after cancellation, want 1", n)
	}
}

func TestSplitJobs(t *testing.T) {
	download := &downloadJob{}
	upload := &uploadJob{}
	folder := &createLocalFolderJob{}
	state := &writeFileStateJob{}

	metadata, transfers := splitJobs([]iJob{folder, download, state, upload})
	if len(transfers) != 2 || transfers[0] != iTransferJob(download) || transfers[1] != iTransferJob(upload) {
		t.Errorf("transfers = %v, want the download and upload jobs", transfers)
	}
	if len(metadata) != 2 || metadata[0] != iJob(folder) || metadata[1] != iJob(state) {
		t.Errorf("metadata = %v, want the folder and state jobs in their original order", metadata)
	}
}
