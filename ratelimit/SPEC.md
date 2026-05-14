# `ratelimit` package — per-IP + per-keyID rate limiting

> Sibling of `attest/` and `secrets/`. Implements the two-bucket rate-limiting
> scheme committed to in `attest/SPEC §8.4`.

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Fixed-window per-key counters. One `Bucket` = one (limit, window) policy.
- `Limiter` bundles the two production buckets (PerIP + PerKeyID).
- A chi-style HTTP middleware that applies a `Bucket` to all requests with the remote IP as the key.
- A background sweeper that drops expired entries to bound memory.

### 1.2 What this package is NOT responsible for

- Persistence — buckets live in memory only. Restarts reset all counters. This is intentional (SPEC §8.4 trailing rationale).
- Distributed rate limiting — V1 is a single instance behind one address. Multi-instance ratelimit would need Redis or similar; out of scope.
- Choosing the bucket key for non-IP cases — the handler decides whether to use `keyID`, a header, or anything else. This package just operates on opaque string keys.
- Composing buckets — handlers and middleware compose them, not the package.
- Cooperating with the attest package — the limiter has no awareness of which keyIDs are registered. It just counts.

### 1.3 Inputs

| Function | Input |
|---|---|
| `NewBucket(limit, window)` | the policy |
| `Bucket.Allow(key, now)` | opaque key + injectable time |
| `Bucket.Sweep(now)` | injectable time |
| `PerIPMiddleware(b, code)` | bucket + error-code string for the 429 body |

### 1.4 Outputs

| Function | Output |
|---|---|
| `Bucket.Allow` | `(allowed bool, retryAfter time.Duration)` — `retryAfter` is the time until the current window resets, 0 when allowed |
| `Bucket.Sweep` | number of entries removed |
| `Bucket.Size` | number of tracked keys (test-only diagnostic) |
| `PerIPMiddleware` | a `func(http.Handler) http.Handler` |

### 1.5 External dependencies

stdlib only (`sync`, `time`, `net`, `net/http`, `strconv`). No third-party.

## 2. Algorithm — fixed window

For each key, store `{count, resetAt}`. On `Allow(key, now)`:

1. If no entry OR `now > resetAt`: write `{count: 1, resetAt: now + window}`, allow.
2. Else if `count >= limit`: deny, return `resetAt - now` as retry-after.
3. Else: increment count, allow.

Chose fixed-window over token-bucket / sliding-window because:

- Boring is good — one map + one mutex + one int.
- The "burst at window boundary" classic fixed-window flaw is acceptable at YardMate scale (a worst-case 2× limit burst over a 2-second slice doesn't matter when limits are 100/h or 50/day).
- Memory cost is one entry per active key, bounded by `Sweep`.

## 3. Production policy (V1 defaults)

| Bucket | Limit | Window | Rationale |
|---|---|---|---|
| Per-IP | 100 | 1 hour | NAT-friendly. Corporate networks where many real users share one egress IP must not lock out. 100/h ≈ 1 every 36 s — well above a normal user's needs. |
| Per-keyID | 50 | 24 hours | iOS clients cache vended secrets in memory; a typical user issues 1–10 fetches/day. 50 leaves headroom for cold starts, app reinstalls, and the occasional client bug, while still rate-limiting abuse from a leaked private key. |

Both numbers are env-overridable for staging vs production:

```
YARDMATE_API_RL_IP_LIMIT          (default 100)
YARDMATE_API_RL_IP_WINDOW         (default 1h)
YARDMATE_API_RL_KEYID_LIMIT       (default 50)
YARDMATE_API_RL_KEYID_WINDOW      (default 24h)
```

Restart required to pick up changes.

## 4. Application points

| Endpoint | Per-IP middleware | Per-keyID inside handler |
|---|---|---|
| `/healthz` | — (excluded; uptime probes) | — |
| `/v1/attest/challenge` | ✅ | — (no keyID yet) |
| `/v1/attest/register` | ✅ | — |
| `/v1/secrets/challenge` | ✅ | — (keyID not in request body) |
| `/v1/app-secrets` | ✅ | ✅ (after assertion verify) |

### Per-keyID is post-verify

`/v1/app-secrets` checks the keyID rate limit AFTER `VerifyAssertion` succeeds. This trades a tiny amount of wasted CPU on rate-limited requests against a much bigger downside: a pre-verify check would let any caller with knowledge of a keyID (from leaked logs etc) burn through that keyID's daily budget and lock out the real owner. The post-verify position means only the holder of the private key can move the counter.

Side effect: when the limit denies, the assertion verification has already updated the stored counter. That is intentional — the next legitimate assertion from the same key carries a fresh higher counter from the iOS framework, so nothing breaks.

## 5. Sweeper

`Limiter.StartSweeper(interval)` runs a goroutine that calls `Bucket.Sweep` on both buckets every `interval`. With `Sweep` removing entries whose `resetAt < now`, the cache size is bounded by:

- Per-IP: at most one entry per active IP in the last `IPWindow + interval`.
- Per-keyID: at most one entry per registered keyID in the last `KeyIDWindow + interval`.

For V1 scale (sub-1000 active devices) this is trivially small. `interval = 1 minute` is the default.

## 6. Pitfalls

### 6.1 IP comes from chi.RealIP

The middleware reads `r.RemoteAddr` (port stripped). This is only the actual client IP if the upstream chi router has `middleware.RealIP` set up so that `X-Forwarded-For` / `X-Real-IP` is honoured. If you reverse-proxy yardmate-api behind nginx, also pin a `set_real_ip_from` directive so the forwarded header isn't spoofable.

If neither chi.RealIP nor a fronting proxy is in the path, every request appears to come from `127.0.0.1` (or the LB's address) and per-IP becomes effectively per-instance.

### 6.2 Fixed-window burst

A client can fire up to `2 * limit` requests across a 1-second slice straddling a window boundary. Sliding window would smooth this. For V1, accept the burst; revisit only if logs show real abuse exploits the boundary.

### 6.3 No persistence

Process restart resets all counters. An attacker who can trigger a restart (e.g., OOM from a memory leak) regains a full budget. Mitigation: keep the binary leak-free + monitor restart frequency.
