# `inflight` — concurrency bound for expensive proxy endpoints

## 1. Why

`/v1/identify` and `/v1/diagnose` each read the uploaded image (≤8 MB) into
memory (`proxy/handlers.go`, `io.ReadAll`) and hold it for the upstream call
(2–30 s). The service runs under a systemd `MemoryMax` cgroup cap. With **no
concurrency bound**, a burst of simultaneous uploads can exceed the cap and the
whole process is OOM-killed — dropping **every** in-flight request, followed by
a cold restart. Raising `MemoryMax` only moves the ceiling; it does not remove
the failure mode.

This package caps how many of these requests process **at once**, so peak
memory ≈ `maxInflight × per-request image`. Overflow is shed cleanly instead of
crashing the service.

## 2. Design — wait-then-reject (not reject-fast)

The already-shipped iOS client maps `503 → .serviceUnavailable` and times out
at **30 s (identify) / 20 s (diagnose)** (`RecognitionViewModel.mapError`).
So overflow requests **wait** for a slot up to a short budget instead of failing
immediately:

- The wait happens in **middleware, before the handler reads the body**, so a
  waiting request holds only a goroutine + connection (~KB) — **not the 8 MB
  image**. The memory bound stays `maxInflight × image` regardless of queue
  depth.
- Under realistic bursts a slot frees within a second or two (each in-flight
  identify completes in 2–8 s), so the waiter is served a normal **200** — the
  client just sees a slightly slower identify. **No client update required.**
- Only **sustained extreme overload** (wait budget elapses, or the wait queue
  is already at `maxWait`) yields a **503** — which old clients already render
  as a normal "try again" state, and new clients can auto-retry using
  `Retry-After`.

## 3. Parameters (`inflight.New`)

| param | env override | default | meaning |
|---|---|---|---|
| `maxInflight` | `YARDMATE_API_INFLIGHT_MAX` | `30` | concurrency cap = memory guard (30 × ~16 MB ≈ 480 MB, well under the 4 GB cgroup) |
| `maxWait` | `YARDMATE_API_INFLIGHT_MAX_WAIT` | `200` | max queued waiters; bounds goroutine/connection growth (200 × ~KB ≈ negligible) |
| `waitBudget` | `YARDMATE_API_INFLIGHT_WAIT_BUDGET` | `5s` | how long overflow waits for a slot before 503; kept `<` client timeout so most overflow resolves to a served 200 |

Throughput at defaults: 30 slots × (1 completion / ~5 s) ≈ 6 identify/s ≈
~21 k/hr — far above realistic load, and aligned with what the upstream
identify APIs tolerate.

## 4. Application point

Applied as middleware on a sub-group of the per-device group in `server.go`,
wrapping **only** `/v1/identify` + `/v1/diagnose` (the image-buffering
endpoints). `/v1/plants/enrichment` (text), `/v1/account/delete`,
`/v1/plants/signal` stay outside — they don't buffer large images.

Layering (outermost → innermost): per-IP rate limit (`/v1`) → per-device rate
limit → **inflight concurrency bound** → handler. The rate limiters bound
long-run abuse per key; this limiter bounds instantaneous concurrency (memory).

## 5. Response shape

On shed: `503 Service Unavailable`, `Retry-After: <waitBudget seconds>`, body
`{"error":"server_busy"}` — same JSON envelope shape as the rate limiter's 429
(`ratelimit.Write429`) so clients parse one body.

## 6. Not in scope

- Removing the in-memory image buffer (streaming pass-through) — a separate
  optimization that raises the ceiling further; the bound here makes the current
  buffering safe.
- Async job queue / client-direct-to-R2 upload — only needed to absorb
  sustained pulses far beyond current scale.
