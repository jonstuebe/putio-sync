# Plan: concurrent transfers + download-manager progress UI

Status: agreed, not yet implemented.
Branch: off `v2`.
Toolchain: Go 1.24 via `mise` (`mise.toml`). Baseline `go build ./...` and `go test ./...` are green
(cgo deprecation warnings from `fsevents` only).

## Goal

1. Transfer multiple files at once, with a configurable concurrency limit.
2. Replace the once-per-second log line per transfer with a live, download-manager-style
   progress display on a TTY, and quiet structured logging when not on a TTY.

## Background: how it works today

- `syncRoots` (`sync.go:130`) walks both sides, reconciles, and then runs every job strictly
  sequentially in a `for` loop (`sync.go:182`). The first error aborts the whole pass; the outer
  loop in `Sync` logs it and retries after the wait interval.
- `downloadJob.Run` (`job_download.go:64`) opens a single `GET` with a `Range: bytes=N-` header and
  does one `io.CopyN`. No chunking. The range header exists for **resume**, not parallelism.
- Both transfer jobs are already re-enterable: a second `Run()` resumes rather than restarts.
  Download resumes from the persisted `Offset` / `DownloadTempName`; upload asks the tus endpoint
  via `client.Upload.GetOffset`. **Retry and resume are the same operation** - this is what makes
  in-pass retry cheap.
- State lives in bbolt (`state.go`). Every write is a full fsync'd `db.Update` transaction. bbolt
  serializes them, so concurrent writes are safe but contend.
- `internal/progress` is a reader wrapper that owns a per-instance `time.Ticker` and logs directly
  via `log.Infof`. Two call sites: `job_download.go:107`, `job_upload.go:93`.

## Constraints discovered while planning

These are the reasons the plan is shaped the way it is.

- **`DirCache` is not thread-safe.** `internal/dircache/dircache.go:18` is a bare
  `map[string]int64` with unsynchronized reads and writes in `Mkdirp`. `uploadJob.Run` calls
  `dirCache.Mkdirp` at `job_upload.go:63`, i.e. *inside* what will be the worker pool. Concurrent
  uploads into new subdirectories would panic on concurrent map read/write, and racing `Mkdirp`
  calls for the same path would create duplicate remote folders. Must be fixed before concurrency
  is safe at all.
- **`syncing` and `syncStatus` globals (`sync.go:36-38`) become a data race** once jobs run on
  multiple goroutines - written by workers, read by the HTTP handler. The progress registry
  replaces them.
- **`bbolt.Open(dbPath, 0666, nil)` (`sync.go:55`) takes an exclusive flock with no timeout.** A
  second `putio-sync` on the same XDG data dir hangs forever at startup with no output.
- **The 10s stall canceller conflates two things.** `timerResetWriter` (`job_download.go:104`)
  reuses `defaultTimeout`, which is also the API request timeout. With several streams sharing a
  constrained link, a merely-starved stream can go quiet for 10s and get killed.
- **`cenkalti/log` supports handler replacement**: `log.DefaultLogger.SetHandler(h)` with
  `Handler{ SetFormatter; SetLevel; Handle(*Record); Close() error }`; default is
  `NewFileHandler(os.Stderr)`, which colorizes when the fd is a tty. Parking log output above a
  live region is a supported extension point.

## Decisions

### Scope of locking
In-process only. A per-relpath in-flight set inside the pool. No cross-machine coordination -
put.io has no lock primitive and that is a separate project. Cross-process is left to the bbolt
flock, made explicit (below).

### Which jobs run concurrently
Two phases, replacing the loop at `sync.go:182`:

1. **Metadata phase, sequential, existing order** - `createLocalFolderJob`,
   `createRemoteFolderJob`, `moveLocalFileJob`, `deleteLocalFileJob`, `writeFileStateJob`,
   `writeDirStateJob`, `deleteStateJob`. Millisecond-scale, so serializing costs nothing, and it
   preserves the `createRemoteFolderJob` -> `dirCache` -> `uploadJob` dependency for free.
2. **Transfer phase, worker pool** - `downloadJob`, `uploadJob` only.

`recon.go` is untouched. Note that uploads still create folders lazily via `dirCache.Mkdirp` at
transfer time, which is why the `DirCache` fix is required regardless of the phase split.

`reconciliation` emits at most one job per relpath per pass, so the phase reordering cannot make a
delete race a transfer of the same path. Worth a test to keep it true.

### Configuration
`Config.Concurrency int`, default **4**, picked up by koanf as `concurrency` in TOML and
`PUTIO_CONCURRENCY` in env. Values `<= 0` clamp to 1. One knob for both directions until there is
evidence the two need different numbers.

### Error handling and retry
- 3 attempts total, exponential backoff 1s then 4s.
- Retryable, by whitelist: `net.Error`, put.io 5xx / 429, and stall-cancellation **when the parent
  ctx is still alive**. Everything else (local FS errors like `ENOSPC`, permission denied, auth)
  fails immediately.
- "File modified while downloading/uploading" already returns `nil`, not an error. Unchanged.
- After the final attempt: `log.Warningf` the file, continue the rest of the pass. Persisted state
  means the next sync pass resumes it anyway - in-pass retry only avoids a 15-minute wait for a
  blip.
- Per-file failures aggregate with `errors.Join` and are returned at the end of the pass.
- `auth.ErrInvalidCredentials` and `ctx.Err()` abort the whole pass immediately.
- Retry lives in the pool as `runWithRetry(ctx, job)` wrapping `job.Run`, with a package-level
  `isRetryable(err) bool`. No change to the `iJob` interface and no changes to the six job files.
  If uploads later need different rules, promote to a `Retryable(error) bool` method then.

### Output
TTY-detected via `term.IsTerminal`. Two renderers over one shared data model.

TTY layout - completed files print permanently and scroll up; the live region is the bars plus a
summary row:

```
✓ The.Bear.S03E01.mkv                     2.1 GB in 3m12s
✓ notes.pdf                               1.4 MB in 0s
  ⬇ Big.Buck.Bunny.2160p.mkv   ███████████░░░░░░░░  58%  4.2/7.3 GB  11.4 MB/s  4m32s
  ⬇ show.s01e02.mkv            ██████░░░░░░░░░░░░░  31%  1.1/3.5 GB   6.8 MB/s  6m01s
  ⬆ home-video.mp4             ██████████████████░  94%  890/945 MB   2.1 MB/s     26s
  ───────────────────────────────────────────────────────────────────────
  3 active · 12 done · 47 queued · 189 GB left · 20.3 MB/s
```

- Visible bars capped at 10, with an "…and N more" row.
- Long filenames truncate in the middle (`Big.Buck…2160p.mkv`) to preserve the extension.
- Relayout on `SIGWINCH`.
- Cursor restored and region cleared on exit - via `defer` **and** a signal path, because
  `log.Fatal` calls `os.Exit` and skips defers.

Non-TTY (systemd, Docker, CI): start line per file, completion line with duration and average
speed, plus an aggregate line every 30s
(`Syncing: 3 active, 47 queued, 189 GB left, 20.3 MB/s`). Same events as the TTY view, so both
modes tell one story. Completion-lines-only was rejected: a daemon would look dead during a
4-hour transfer.

Rendering is hand-rolled ANSI, not `mpb` or `bubbletea`. The registry has to exist anyway for
`/status`, so the data model is not a library's to own; and owning the write path is what makes
log-parking a five-line function. New dependency: `golang.org/x/term` for `IsTerminal` and
`GetSize` (small, official; `golang.org/x/text` and `golang.org/x/oauth2` are already deps).

Log interleaving: a `Handler` wrapping `NewFileHandler(os.Stderr)` that, in TTY mode, takes the
renderer mutex, erases the live region, delegates, and redraws. Synchronous, so log ordering stays
exact. **Rule: the renderer never logs** - any error in the render path is swallowed or stashed,
never `log.Errorf`'d. That removes the only re-entrancy path.

Rates: widen the existing `ratecounter` from a 1s to a **5s** window and derive ETA from it. Show
`--` for the first couple of seconds and for any ETA over 24h. Aggregate speed is the sum over
active trackers. Keeps the existing dep; 5s is where the number stops flickering but still reacts
to a stall.

### `internal/progress` decomposition
Stays one package - it is one concern, and splitting before there is a second consumer is
premature.

- `Tracker` - per transfer: relpath, direction, size, atomic offset, rate counter, state
  (queued/active/done/failed). Still the `io.Reader` wrapper. (This is today's `Progress`,
  renamed; the per-instance ticker goes away.)
- `Registry` - `RWMutex`-guarded set of trackers. Single source of truth for the TTY renderer,
  the log renderer, and the `/status` handler.
- `Renderer` - one interface, two impls (tty, log), one ticker for the whole program.

### HTTP surface
`/status` keeps its `status` key as a summary string (`"Syncing: 3 active, 47 queued"`) and gains
a `transfers` array with per-file name / direction / bytes / total / speed, read from the
registry. Existing consumers of `status` keep working.

### Misc
`bbolt.Open` gets `&bbolt.Options{Timeout: 5 * time.Second}` and a clear "another instance is
already running" error, replacing the silent infinite hang.

New `transferStallTimeout = 60 * time.Second` constant for `timerResetWriter`, separate from
`defaultTimeout` (which stays 10s for API calls). Reusing one constant for "an API call should
answer in 10s" and "a byte stream is dead" conflates two unrelated things, and under concurrency
the false positives are invisible - the retry loop turns them into wasted work rather than a
visible error.

## Commits

Four commits, in order. Each is independently reviewable; commit 2 is independently shippable, so
if the renderer turns into a slog there is still working concurrency. Also keeps `git bisect`
useful.

1. **Safety prep** (no behavior change) - `DirCache` mutex; `bbolt.Open` timeout + clear error;
   `transferStallTimeout` split out.
2. **Concurrency** - `Config.Concurrency`; phase split in `syncRoots`; worker pool; per-relpath
   in-flight set; retry + `isRetryable`; `errors.Join` aggregation. Still logs the old way.
3. **Output** - `progress` rewrite (Tracker / Registry / Renderer); TTY renderer; log renderer;
   log-handler parking; terminal teardown on defer and signal.
4. **Surface** - structured `/status`; README Configuration section.

## Tests

The repo has one test file (`recon_test.go`, table-driven over `reconciliation`) and no network
fakes, so job execution has no existing harness.

Under `-race`:
- **Pool**, with a fake `iJob`: concurrency limit respected, per-relpath exclusion, error
  aggregation, ctx cancellation, abort-on-auth-error.
- **Retry classifier**: table test over error kinds, including stall-cancel with a live parent vs
  a cancelled one.
- **`DirCache`**: concurrent `Mkdirp` on overlapping paths - no panic, no duplicate
  `CreateFolder` calls for the same path.

Plain table test:
- **Renderer formatting**: byte and rate humanization, ETA formatting and its `--` cases,
  mid-string truncation at a range of widths.

Not tested: end-to-end ANSI output. Asserting on escape sequences is brittle and says nothing
about whether it looks right. The TTY view gets a manual eyeball against a live sync.

## Docs

`README.md` currently documents no configuration options at all. Add a Configuration section
covering the full `Config` field list with defaults (not just `Concurrency`, since we are in
there anyway), the TTY vs non-TTY output modes, and the `/status` JSON shape.

## Rejected alternatives

- **Cross-machine file locking** - no put.io lock primitive; needs remote leases with expiry and
  crash recovery. Separate project.
- **One pool for all job types** with a dependency graph (`Deps()` on `iJob`) - most of the risk
  for none of the remaining win, since metadata jobs are millisecond-scale.
- **`RWMutex` on the `DirCache` map only** - fixes the panic, leaves the duplicate-remote-folder
  race. A trap: the crash disappears and a data-corruption bug stays.
- **Per-path singleflight in `DirCache`** - correct, but optimizes something that is not a
  bottleneck.
- **`vbauerster/mpb`** - fights us on all three of the "+N more" cap, the pinned summary row, and
  routing arbitrary log records above the region.
- **`bubbletea`** - enormous for seven lines of text, and its event loop wants to own `main`.
- **Fail-fast on first transfer error** - throws away in-progress bytes for unrelated files.
- **Scaling the stall timeout with `Concurrency`** - clever and unpredictable.
