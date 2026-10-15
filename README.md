# ZeroOut

Overwrite the free space on a volume — one or more passes of zeros, ones or
random data — from a small web UI that runs in Docker. Built first for a
UGREEN NAS; a desktop variant is planned on the same engine.

ZeroOut never touches existing files. Each pass fills the free space with
files in a hidden `.zeroout` directory on the target, syncs them to disk,
optionally reads them back to verify, then deletes them.

## Features

- Multiple passes (up to 35), each with its own pattern: zeros, ones, random.
- **Safety margin** mode (leave N GB free) or **fill completely**, which as
  root also overwrites ext4's root-reserved blocks.
- Optional read-back verification of every pass.
- `O_DIRECT` writes on Linux: terabytes of fill data don't flush the NAS's
  page cache.
- Live progress, speed and ETA; job log and history.
- Resumable: if the container stops mid-job, fill files are cleaned up on the
  next start and the remaining passes can be resumed.
- Refuses targets that aren't mount points (so a missing bind mount can't
  fill the container's own disk).
- Optional HTTP basic auth (`ZEROOUT_PASSWORD`).

## Running on the NAS

The image is published to `ghcr.io/seraphofnineball/zeroout` (linux/amd64
and linux/arm64) by GitHub Actions on every push to `main`.

1. In the NAS's Docker app, create a new project (compose) and paste
   [`compose.yaml`](compose.yaml).
2. Adjust the volumes: one `/volumeN:/targets/volumeN` line per volume you
   want to offer, and a `/data` path on a **different** volume.
3. Deploy, then open `http://<nas>:8088`.

The sample compose file starts in **trial mode**
(`ZEROOUT_MAX_BYTES_PER_PASS` caps each pass at 1 GiB). Delete that line and
redeploy for a real wipe.

To build locally instead: `docker build -t zeroout .`

## Configuration

| Env var | Default | |
|---|---|---|
| `ZEROOUT_LISTEN` | `:8080` | HTTP listen address |
| `ZEROOUT_TARGETS` | `/targets` | Each subdirectory is offered as a volume |
| `ZEROOUT_DATA` | `/data` | Job state and history (`state.json`) |
| `ZEROOUT_PASSWORD` | *(empty)* | Require basic auth when set |
| `ZEROOUT_MAX_BYTES_PER_PASS` | `0` | Cap per pass, for trial runs |
| `ZEROOUT_ALLOW_UNMOUNTED` | `false` | Allow non-mount-point targets (development only) |

## Notes

- **One pass is enough** for modern hard drives (NIST SP 800-88). More passes
  are there for policies that require them.
- **SSDs:** overwriting free space doesn't reliably reach the old flash
  blocks; use TRIM instead.
- **Btrfs:** compression makes zero passes ineffective and snapshots keep
  deleted data out of free space. Not handled yet — ZeroOut targets ext4 first.
- **bcache:** in `writearound` mode (UGOS default) fill writes bypass the SSD
  cache. In `writeback` mode they would go through it.

## Development

```sh
go test ./...
go run ./cmd/zeroout -targets ./dev-targets -data ./dev-data -listen :8080 -allow-unmounted -max-bytes-per-pass 1073741824
```

Create subfolders in `dev-targets` to act as volumes. Windows builds are for
development; fill writes are buffered there.
