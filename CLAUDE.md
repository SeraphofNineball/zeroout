# ZeroOut

Free-space wiper: Go backend + embedded vanilla-JS web UI, shipped as a
`scratch` Docker image. Desktop variant planned later on the same engine.

## Layout

- `internal/wipe` — engine. Fills `<target>/.zeroout/fill-NNNNN.bin` chunk
  files per pass, verifies, deletes. Platform I/O in `io_linux.go`
  (O_DIRECT + buffered tail top-off after ENOSPC) and `io_windows.go`.
- `internal/volume` — statfs / GetDiskFreeSpaceEx, mount detection via
  `/proc/self/mountinfo`.
- `internal/job` — single-job manager, `state.json` persistence, resume.
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
- State-changing API calls require `Content-Type: application/json` (CSRF guard).
- Check both platforms: `go vet ./... && GOOS=linux go vet ./...`.

## Primary target

UGREEN NAS (UGOS Pro): `/volume1` is ext4 on bcache (writearound) over
RAID10 HDDs; Docker lives on `/volume3`. See `compose.yaml`.
