# `doctor` — conversational plant diagnosis over SSE (`POST /v1/doctor`)

## 1. Why

The iOS Plant Doctor feature is a multi-turn conversation: the user sends
photos, the model streams back a structured diagnosis. Two properties force a
new endpoint instead of extending `/v1/diagnose`:

- **The reply is generated, not looked up.** `/v1/diagnose` maps a photo to a
  catalog disease id; doctor produces free-form structured JSON (observations →
  clarification/diagnosis → actions → spokenSummary) whose schema is the
  product contract (see §5).
- **The wait is long enough that progress must be visible.** Generation takes
  5–15 s. The schema deliberately orders `observations` before everything
  else, so their strings complete early in the token stream; this package
  extracts each completed observation from the *partial* JSON and pushes it to
  the client immediately over SSE. The client renders them as the "thinking"
  subtitle. Field order in the schema is therefore load-bearing (§5).

The OpenAI key stays server-side (same posture as identify/diagnose). Clients
never talk to OpenAI.

## 2. Request

`POST /v1/doctor`, `multipart/form-data`, body ≤ 9 MB (`http.MaxBytesReader`,
matches the nginx `client_max_body_size 9M`).

Headers (same contract as `/v1/diagnose`):

- `X-Device-Install-Id` — RFC-4122 UUID, required (also the per-device
  rate-limit key, enforced by middleware in `server.go`).
- `X-App-Version` — required.

Form fields (all optional unless said otherwise):

| field      | meaning                                                        |
|------------|----------------------------------------------------------------|
| `image`    | 0–3 file parts, jpeg/png/webp (sniffed). >3 → `too_many_images` |
| `text`     | user's message, ≤ 2 KB after trim                              |
| `history`  | JSON array of prior turns (see below), ≤ 8 turns               |
| `language` | app UI locale code (`en`, `zh-Hans`, …); unknown → English     |
| `units`    | `metric` \| `imperial`; anything else → `metric`               |
| `model`    | override; honored ONLY when `DOCTOR_ALLOW_MODEL_OVERRIDE=true` **and** the value is in the model whitelist. Silently ignored otherwise (never an error — a stale debug client must not break prod). |

At least one of `text` / `image` must be present → else `empty_request`.

`history` element: `{"user": string, "had_images": bool, "reply": <raw JSON>}`
where `reply` is the assistant's structured JSON from the prior turn, passed
back verbatim. The first turn's images are NOT re-sent (several hundred image
tokens per turn saved); `had_images` lets the prompt say "[sent a photo]".
Caps: `user` ≤ 2 KB, `reply` ≤ 8 KB each, else `bad_history`.

## 3. Response — SSE event contract

`Content-Type: text/event-stream`. Events, in guaranteed order:

```
event: observation        zero or more, as each completes upstream
data: {"index":0,"text":"Most leaves are hanging down"}

event: usage              zero or one, before reply
data: {"prompt_tokens":1842,"completion_tokens":318,"model":"gpt-4o-mini"}

event: reply              exactly one on success, terminal
data: {<full structured reply JSON, validated json>}

event: error              terminal instead of reply on failure
data: {"code":"bad_upstream"}
```

Client rule: a stream that ends without `reply` or `error` = interrupted
(client shows retry). Errors before SSE headers are plain JSON
`{"error":"<code>"}` with 4xx/5xx status (same shape as proxy endpoints);
errors after streaming started arrive as the `error` event (HTTP status is
already 200 and unchangeable).

Error codes: `missing_app_version`, `missing_device_id`, `bad_multipart`,
`image_too_large` (413), `too_many_images`, `bad_image`, `bad_history`,
`empty_request`, `rate_limit_global` (429), `server_busy` (503, from
inflight), `bad_upstream`, `upstream_timeout`, `bad_reply`.

Buffering: the handler sets `X-Accel-Buffering: no` (nginx honors it) AND the
vhost has a dedicated `location = /v1/doctor` with `proxy_buffering off` —
belt and suspenders, because a silently buffering proxy turns streaming back
into a 15-second blank wait, which is this endpoint's whole reason to exist.

No heartbeat: gpt-4o-mini's first content token arrives in ~1–3 s and chunks
flow continuously after; nginx `proxy_read_timeout` (180 s on this location)
counts *between* reads and never trips. Revisit only if a future model idles
longer than that before its first token.

## 4. Protection — same posture as identify/diagnose, two deliberate differences

Inherited by mounting inside the same `/v1` per-device group (`server.go`):
per-IP rate limit, per-device rate limit, App-Attest headers logged.
Validation runs BEFORE the spend gate so malformed requests cannot drain the
budget (same rationale as identify/diagnose).

Differences, both because SSE streams are long-lived:

- **Own `inflight.Limiter`** (`YARDMATE_API_DOCTOR_INFLIGHT_MAX`, default 4;
  wait-then-reject like the shared one). A doctor request holds its slot for
  the full 5–15 s generation — sharing the identify/diagnose limiter would let
  a handful of chat streams starve the scan pipeline whose requests finish in
  ~3 s. Peak memory ≈ cap × (≤9 MB) keeps the OOM math intact.
- **Own hourly spend bucket** (`DOCTOR_HOURLY_BUDGET`, default 200/h) via
  `ratelimit.GlobalGate`. Doctor's per-call cost (streamed vision chat) has
  different economics than Plant.id calls; a doctor burst must not exhaust the
  identify/diagnose budget or vice versa.

Subscription enforcement is deliberately NOT here yet — identify/diagnose have
none either; adding a paid-tier credential is a separate cross-endpoint
project. When it lands it must cover all three.

## 5. Upstream call

`chat/completions`, `stream: true`, `stream_options.include_usage: true`,
`response_format: json_schema strict` with the schema in `prompt.go`.

**Schema field order is the product contract**: `observations` is declared
before `spokenSummary`, so its strings finish early and can be streamed as
progress; `spokenSummary` is declared last so the model writes it after
settling the structure. The schema is a hand-written raw string — never build
it from Go maps (`encoding/json` sorts map keys; the reordering silently kills
the early-observations behaviour with zero errors).

Prompt rules ported from the PoC (fmpoc repo): two mutually-exclusive branches
(`photoProblem` = photo unusable vs `clarification` = ask-first), four-band
`healthLevel` with no numeric scores, units spoken natively (never converted
precision like "7.87 inches"), reply language from `language`, enum values
never translated. `followUp` stays in the schema — the reminder feature is
deferred client-side, and removing/re-adding schema fields is a contract
change; the client simply doesn't render it yet.

`reasoning_effort: minimal` is sent ONLY for gpt-5-family models (other
models 400 on the parameter).

Timeouts: 20 s to response headers (`http.Transport.ResponseHeaderTimeout`),
180 s total stream (context). Client disconnect cancels the upstream request
via `r.Context()` — we stop paying for tokens nobody will read.

## 6. Config (secrets vault — NOT `os.Getenv`; Codex #23)

| key                          | default       | meaning                          |
|------------------------------|---------------|----------------------------------|
| `DOCTOR_ENABLED`             | `false`       | master switch — deploying the binary must not open the paid surface by itself; staging validates with `true`, prod flips at iOS release |
| `OPENAI_API_KEY`             | — (required)  | absent → route unregistered      |
| `DOCTOR_MODEL`               | `gpt-4o-mini` | must be in whitelist, else default + WARN |
| `DOCTOR_ALLOW_MODEL_OVERRIDE`| `false`       | dev/staging only; prod stays false |
| `DOCTOR_HOURLY_BUDGET`       | `200`         | shared hourly call ceiling       |

Inflight knobs are systemd env (like the existing inflight):
`YARDMATE_API_DOCTOR_INFLIGHT_MAX` (4) / `_MAX_WAIT` (8).

Model whitelist: `gpt-4o-mini`, `gpt-4o`, `gpt-5-mini`, `gpt-5`.

## 7. Not in scope

- Persistence — cases live on-device (SwiftData); the server is stateless.
- Subscription / entitlement checks (§4).
- Reminder scheduling from `followUp` (client-side, deferred).
- Non-OpenAI upstreams. The SSE contract is upstream-agnostic on purpose: the
  client consumes typed events, so swapping the model provider never touches
  the app.
