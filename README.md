putio-sync
==========

Command-line program to sync a folder between put.io and your computer.

**WARNING: The project is still in development and features are subject to change!**

If you are using MacOS or Windows, you can install desktop version: [putio-sync-desktop](https://github.com/putdotio/putio-sync-desktop).

Installing
----------

If you are on macOS you can install with [Homebrew](https://brew.sh/):
```sh
brew install putdotio/tap/putio-sync
```

> [!NOTE]
> If you previously installed via the old tap (`putdotio/putio-sync`), you'll need to uninstall first and reinstall with the new formula name:
> ```sh
> brew uninstall putio-sync
> brew untap putdotio/putio-sync
> brew install putdotio/tap/putio-sync
> ```

Otherwise, get the latest binary from [releases page](https://github.com/putdotio/putio-sync/releases).

Usage
-----

Run the program with your account credentials:
```sh
PUTIO_Username=<username> PUTIO_Password=<password> putio-sync
```

Then program is going to sync the contents of these folders:
- **$HOME/putio-sync** in your computer
- **/putio-sync** in your Put.io account

The folders are created if they don't exist.
Files will be synced periodically or when a change has been detected.

Configuration
-------------

Options can be set in a config file or in the environment. Environment
variables take precedence. Run `putio-sync -print-config-path` to see where the
config file is expected, or pass `-config <path>` to use another one.

```toml
Username    = "your-username"
Password    = "your-password"  # or "token/<oauth-token>"
LocalDir    = "~/putio-sync"
Concurrency = 4
Server      = "127.0.0.1:3000"
```

The matching environment variables are the field names with a `PUTIO_` prefix,
for example `PUTIO_Username` and `PUTIO_Concurrency`. Note that the underscore
is a separator, so multi-word options keep their spelling: `PUTIO_LocalDir`,
not `PUTIO_LOCAL_DIR`.

| Option | Default | Description |
| --- | --- | --- |
| `Username` | | put.io account username. Not needed when `Password` holds a token. |
| `Password` | | Account password, or an OAuth token prefixed with `token/`. |
| `LocalDir` | `~/putio-sync` | Folder on this computer to sync. |
| `Concurrency` | `4` | How many files to transfer at the same time. Values below 1 are treated as 1. |
| `DryRun` | `false` | Report what would be done without changing anything. |
| `Once` | `false` | Sync once and exit, instead of running continuously. |
| `Server` | | Listen address for the HTTP status server, e.g. `127.0.0.1:3000`. Off when empty. |
| `Debug` | `false` | Log at debug level. |

Output
------

When the output is a terminal, transfers are shown as a live display: a
progress bar per file over a summary of the whole sync, with completed files
printed above it.

```
✓ The.Bear.S03E01.mkv  2.1 GB in 3m12s
  ↓ Big.Buck.Bunny.2160p.mkv  ██████████░░░░░░░░  57%   4.2/7.3 GB   11.4 MB/s   4m38s
  ↑ home-video.mp4            █████████████████░  94%   890/945 MB    2.1 MB/s      26s
  ──────────────────────────────────────────────────────────────────────
  2 active · 12 done · 47 queued · 189 GB left · 13.5 MB/s
```

Anywhere else - under systemd, in a container, or with output redirected - the
same events are written as log lines, with a progress summary every 30 seconds.

HTTP server
-----------

Setting `Server` starts an HTTP server with these endpoints:

| Endpoint | Description |
| --- | --- |
| `/syncing` | `true` or `false`, whether a sync is in progress. |
| `/trigger` | Starts a sync immediately. |
| `/status` | Sync status as JSON. |

```json
{
  "status": "Syncing: 2 active, 47 queued",
  "transfers": [
    {
      "path": "shows/Big.Buck.Bunny.2160p.mkv",
      "direction": "download",
      "bytes": 4509715660,
      "total": 7838315479,
      "speed": 11953766,
      "percent": 57
    }
  ]
}
```

`transfers` lists the files moving right now, and is an empty list when nothing
is in flight. `speed` is in bytes per second.
