# Progressive Auth Loading - Hugging Face Validation

Environment: same Space, persistent volume, config, and 1,144 JSON credentials used for the 188-second baseline.

Baseline (pre-change, serial dual scan):

- process start to `API server started successfully`: ~188 s
- root cause: listener blocked on serial credential discovery, then watcher rescanned the same files
- storage: file store only (no Postgres); `usage.db` excluded from the auth path

## Local control benchmark

Command:

```bash
go test ./internal/watcher -run '^$' -bench BenchmarkInitialAuthLoad1144 -benchmem -count=3
```

Each iteration asserts exactly 1,144 initial payload reads (one per discovered JSON). This measures loader structure only; HF owns end-to-end latency.

| workers | ns/op | B/op | allocs/op | notes |
|---:|---:|---:|---:|:---|
| 1 | 24,229,649 | 11,419,028 | 61,910 | local SSD control; each iter reads exactly 1144 payloads |
| 4 | 13,146,480 | 11,418,913 | 61,908 | local SSD control |
| 8 | 11,570,300 | 11,421,876 | 61,915 | local SSD control |
| 16 | 13,058,937 | 11,427,118 | 61,926 | local SSD control; keep as default until HF evidence |
| 32 | 13,101,695 | 11,436,795 | 61,942 | local SSD control; no clear win over 16 |

Source command (count=1 default benchtime on developer machine, 2026-07-14):

```text
BenchmarkInitialAuthLoad1144/workers-1-16    45   24229649 ns/op  11419028 B/op  61910 allocs/op
BenchmarkInitialAuthLoad1144/workers-4-16    90   13146480 ns/op  11418913 B/op  61908 allocs/op
BenchmarkInitialAuthLoad1144/workers-8-16    93   11570300 ns/op  11421876 B/op  61915 allocs/op
BenchmarkInitialAuthLoad1144/workers-16-16   84   13058937 ns/op  11427118 B/op  61926 allocs/op
BenchmarkInitialAuthLoad1144/workers-32-16   85   13101695 ns/op  11436795 B/op  61942 allocs/op
```

Local control is not HF latency. HF persistent volume I/O dominates the original 188 s baseline.

## Same-HF end-to-end table

| workers | listener ms | first auth ms | full load ms | read failures | peak FD | peak RSS MiB | peak CPU % | request during load | final providers/auths |
|---:|---:|---:|---:|---:|---:|---:|---:|:---:|:---|
| 1 | | | | | | | | | |
| 4 | | | | | | | | | |
| 8 | | | | | | | | | |
| 16 | | | | | | | | | |
| 32 | | | | | | | | | |

Acceptance:

- listener <= 2,000 ms
- first selectable file auth <= 3,000 ms
- full load at selected default <= 30,000 ms
- initial payload reads = discovered JSON files
- peak concurrent reads <= configured workers
- final expected counts: xAI 1,120; Antigravity 19; Codex 5; total files 1,144

## Measurement procedure

For each value `1 4 8 16 32`, change only `auth-load-workers`, restart the same Space without changing the volume, and poll:

```bash
curl -fsS -H "Authorization: Bearer ${MANAGEMENT_KEY}" \
  "http://127.0.0.1:8317/v0/management/auth-files/load-status"
```

Poll at 100 ms until `state` is `ready` or `degraded`. Record process start, the existing `API server started successfully` / `API server listener ready` timestamp, the first status with `auths_loaded > 0`, and terminal `completed_at`. During `loading`, send one request for a model already present in the first acknowledged batch and record its success.

Collect process resource peaks using the HF container's available `/proc/1/fd`, `/proc/1/status`, and process CPU telemetry. Do not add a permanent profiler or debug endpoint for this one validation.

## Status

- Local unit/race/build verification: tracked on branch `feat/progressive-parallel-auth-loading`
- Same-HF 1,144-file measurement: **external blocker** until Space deploy access is available
- Default workers remain **16** until same-HF evidence shows FD/RSS/CPU pressure or that 8 is faster across repeated runs
