# ZeroOut

Overwrite the free space on a volume — one or more passes of zeros, ones or
random data — and find, recover or audit deleted data still sitting in that
free space. A small web UI that runs in Docker, built first for a UGREEN NAS;
a desktop variant is planned on the same engine.

## Wipe free space

ZeroOut never touches existing files. Each pass fills the free space with
files in a hidden `.zeroout` directory on the target, syncs them to disk,
optionally reads them back to verify, then deletes them.

- Multiple passes (up to 35), each with its own pattern: zeros, ones, random.
- **Safety margin** mode (leave N GB free) or **fill completely**, which as
  root also overwrites ext4's root-reserved blocks.
- Optional read-back verification of every pass.
- `O_DIRECT` writes on Linux: terabytes of fill data don't flush the NAS's
  page cache.
- Resumable: if the container stops mid-job, fill files are cleaned up on the
  next start and the remaining passes can be resumed.
- Refuses targets that aren't mount points (so a missing bind mount can't
  fill the container's own disk).

## Find deleted files

Read-only. ZeroOut reads ext4's block bitmaps straight from the disk, then
reads **only the free blocks** and:

- reports how much free space still holds data, with a map of where it is —
  after a wipe this should be close to zero, so it doubles as proof the wipe
  worked;
- recognises ~70 file types by their headers (photos and camera RAW, video,
  audio, Office/PDF/OpenDocument, archives, SQLite, programs, text, email…).
  ext4 always starts a file on a block boundary, so only block starts are
  checked, which keeps false positives low;
- works out where each file ends from its own structure where the format
  allows (JPEG markers, PNG/ZIP/GIF/Ogg terminators, MP4/MKV/RIFF/7z/SQLite
  sizes…). A file whose blocks were partly reused is marked **partial**.

Deleted ext4 files lose their names and folders, so results are listed by
type and position. Fragmented files (common for big videos) may come back
incomplete.

Recover selected files — or everything matching a filter — to a destination
on a **different** disk (writing to the scanned volume could overwrite what
you are recovering; ZeroOut refuses). Each file's blocks are re-checked
before copying: if the space was reused since the scan, it is skipped. A
`manifest.csv` lists every file written.

Images, video, audio and text can be previewed in the browser. Carved data
is untrusted, so it's served with `nosniff` and a sandbox policy, and
HTML/SVG are shown as plain text.

### Device access

Scanning needs read access to the volume's block device, passed in with
`devices:` in the compose file. ZeroOut detects bcache (UGOS's SSD cache) and
reads the **backing** hard-drive device, so a multi-terabyte scan doesn't
flood the cache SSDs — that is only done when the cache has no unwritten
data. If the device isn't available, the UI shows the exact line to add.

## Running on the NAS

The image is published to `ghcr.io/seraphofnineball/zeroout` (linux/amd64
and linux/arm64) by GitHub Actions on every push to `main`; version tags
(`v0.2.0`) also publish `0.2.0` and `0.2` tags for pinning or rollback.

1. In the NAS's Docker app, create a new project (compose) and paste
   [`compose.yaml`](compose.yaml).
2. Adjust it: one `/volumeN:/targets/volumeN` line per volume, recovery
   destinations under `/recover`, `/data` on a **different** disk, and the
   block device for each volume you want to scan.
3. Deploy, then open `http://<nas>:8088`.

**Set `ZEROOUT_PASSWORD`.** Anyone who can open the UI can read deleted
files from the scanned volumes.

## Configuration

| Env var | Default | |
|---|---|---|
| `ZEROOUT_LISTEN` | `:8080` | HTTP listen address |
| `ZEROOUT_TARGETS` | `/targets` | Each subdirectory is a volume to wipe or scan |
| `ZEROOUT_RECOVER` | `/recover` | Each subdirectory is a recovery destination |
| `ZEROOUT_DATA` | `/data` | Job state, history and scan results |
| `ZEROOUT_PASSWORD` | *(empty)* | Require basic auth when set |
| `ZEROOUT_DEVICES` | *(auto)* | Override scan devices: `volume1=/dev/x,volume2=/dev/y` |
| `ZEROOUT_MAX_BYTES_PER_PASS` | `0` | Cap per wipe pass, for trial runs |
| `ZEROOUT_ALLOW_UNMOUNTED` | `false` | Development only: relaxes mount, size and same-disk checks |

## Notes

- **One pass is enough** for modern hard drives (NIST SP 800-88). More passes
  are there for policies that require them.
- **SSDs:** overwriting free space doesn't reliably reach the old flash
  blocks; use TRIM instead.
- **Btrfs:** not supported yet for wiping guarantees or scanning — ZeroOut
  targets ext2/3/4 first.
- **bcache:** in `writearound` mode (UGOS default) fill writes bypass the SSD
  cache, and scans read the backing device.
- **USB destinations:** plug the drive in before starting ZeroOut. A missing
  drive is detected (Docker creates an empty folder on the system disk) and
  refused.

## Development

```sh
go test ./...
go run ./cmd/zeroout -targets ./dev-targets -recover ./dev-recover -data ./dev-data \
  -listen :8080 -allow-unmounted -max-bytes-per-pass 1073741824 \
  -devices volume1=./dev-data/volume1.img
```

Create subfolders in `dev-targets` / `dev-recover` to act as volumes and
destinations; point `-devices` at an ext4 image to scan. On Linux CI,
`TestRealExt4` builds real filesystems with `mkfs.ext4` and `debugfs`.
