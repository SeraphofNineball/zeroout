# ZeroOut

Free-space wiper plus deleted-file scanner/recovery: Go backend + embedded
vanilla-JS web UI, shipped as a `scratch` Docker image. Desktop variant
planned later on the same engine.

## Layout

- `internal/wipe` — wipe engine. Fills `<target>/.zeroout/fill-NNNNN.bin`
  chunk files per pass, verifies, deletes. Platform I/O in `io_linux.go`
  (O_DIRECT + buffered tail top-off after ENOSPC) and `io_windows.go`.
- `internal/ext4` — reads superblock, group descriptors (incl. meta_bg,
  64bit) and block bitmaps; `FreeExtents`, `IsFree`. `ext4test` builds tiny
  synthetic images.
- `internal/carve` — file carver over free extents: `kinds.go` (type table,
  append-only — scans store indices), `detect.go` (header detectors and
  known-length parsers), `stream.go` (end-of-file streamers), `scan.go`
  (per-block state machine, stats, heat map).
- `internal/blockdev` — finds the raw device behind a mount via sysfs,
  prefers the bcache backing device when the cache is clean, probes the
  filesystem offset; O_DIRECT reader with bounce buffers for unaligned reads.
- `internal/volume` — statfs / GetDiskFreeSpaceEx, mountinfo, device IDs.
- `internal/job` — single-job manager (wipe/scan/recover), `state.json`,
  resume; scan results in `<data>/scans/<id>/{summary.json,items.bin}`.
- `internal/web` — HTTP API + `static/` UI (embedded).
- `cmd/zeroout` — flags/env, signal handling.

## Rules

- Tests must never write unbounded data: always set `MaxBytes` /
  `MaxBytesPerPass` (a test without a cap would fill the dev machine's disk).
- Random pattern data is generated per 1 MiB unit from (pass seed, chunk,
  unit index) so any range can be regenerated for verification — keep that
  property if the generator changes.
- Fill files must be removed on every exit path (error, cancel, shutdown) and
  stale `.zeroout` dirs on startup.
- Scanning is read-only on the target. Never open a target device writable.
- Carved content is untrusted: keep `nosniff` + sandbox CSP on the content
  endpoint and serve text kinds (incl. HTML/SVG) as `text/plain`.
- Recovery must re-check `IsFree` per item, refuse destinations on the same
  device as the scanned volume, and refuse overlay/tmpfs destinations.
- `carve.Kinds` is append-only; scans persist kind indices.
- State-changing API calls require `Content-Type: application/json` (CSRF guard).
- Check both platforms: `go vet ./... && GOOS=linux go vet ./...`.
  `TestRealExt4` (mkfs.ext4 + debugfs) only runs on Linux — CI covers it.
- Avoid writing a literal U+FEFF in Go source (tools turn `﻿` escapes
  into the raw BOM); build it with `string(rune(0xFEFF))`.

## Primary target

UGREEN NAS (UGOS Pro): `/volume1` is ext4 on bcache (writearound) over
LVM over RAID10 HDDs; backing device
`/dev/mapper/ug_A733F9_1771451578_pool1-volume1_bcache_data`. Docker lives
on `/volume3`. Images: `ghcr.io/seraphofnineball/zeroout` via
`.github/workflows/image.yml`. See `compose.yaml`.
