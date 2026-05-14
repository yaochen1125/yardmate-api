# `proxy` package — server-side proxy for Plant.id + OpenAI (V1)

> Status: **design spec, no code yet**. Implementation lands in the same PR as this SPEC.
> Companion (client): `yardmate-swiftui/app/YardMate/YardMate/Identify/SPEC.md` + `AIChat/SPEC.md` (to be written before iOS impl).
> Background: this package replaces the D-Server "key vending" flow (`/v1/app-secrets`) for V1, because the iOS 26 App Attest assertion verification path is blocked by an upstream issue (see memory `option_d_progress.md`). The `secrets` package and `/v1/app-secrets` endpoint stay compiled in and tested but are marked **deprecated for V1**. Revival when Apple addresses the iOS 26 assertion behavior.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Accept image upload from the iOS client, forward to Plant.id v3 for plant identification, sanitize the response, return to client (`POST /v1/identify`).
- Accept a typed question + plant context from the iOS client, forward to OpenAI Chat Completions (model `gpt-4o-mini`), return the assistant's text (`POST /v1/ai-chat`).
- Enforce per-IP and per-device-install-id rate limits on both endpoints.
- Enforce hard limits on request body sizes (8 MB image, 4 KB chat).
- Log every request's risk signals (deviceInstallId, IP, app version, App Attest assertion status if present, outcome, latency) for forensics + future adaptive risk scoring.
- Map upstream Plant.id / OpenAI errors to stable client-facing error codes; never leak upstream raw error bodies.

### 1.2 What this package is NOT responsible for

- **Storing images or chat content.** Both endpoints are stream-through and stateless — no R2, no disk, no DB write of user-content bytes.
- **Storing conversation history.** Each `/v1/ai-chat` call is independent. The iOS client owns multi-turn state if needed.
- **App Attest verification.** That's the `attest` package. V1 reads the (optional) assertion headers from the request and logs the keyID + assertion presence; it does **not** call `attest.VerifyAssertion()` (the iOS 26 issue would only produce noise). When Apple addresses iOS 26, a `attest.SoftVerifyAssertion()` shim can be added and called from this package as a risk signal.
- **Secret vending.** The `secrets` package and `/v1/app-secrets` route still exist but are deprecated for V1 and not consumed by V1 clients. See `secrets/SPEC.md` §"Deprecation for V1".
- **User accounts / identity / billing.** YardMate V1 has no user accounts; the only client identifier is `X-Device-Install-Id` (a client-generated UUID stored in Keychain). It is **not** an authenticated identity — only a rate-limit / risk-signal scope.
- **Plant.id account management.** API key + billing are operator concerns; this package only consumes the key from `secrets.Vault`.
- **OpenAI account management.** Same as above.
- **Caching.** No response cache. Identification results are user-content-specific and idempotency isn't valuable enough for V1.
- **Multi-tenancy.** All requests use the same upstream Plant.id / OpenAI account.

### 1.3 Inputs

| Function | Input |
|---|---|
| `Identify(ctx, imageBytes, contentType)` | raw image bytes + MIME (`image/jpeg` or `image/png`); ≤8 MB |
| `Chat(ctx, plantName, plantScientificName, question)` | typed string fields; max lengths per §3 |
| HTTP `POST /v1/identify` | multipart/form-data, `image` field (file), required headers `X-Device-Install-Id` + `X-App-Version`, optional `X-AppAttest-KeyID` / `X-AppAttest-Assertion` / `X-AppAttest-Challenge` |
| HTTP `POST /v1/ai-chat` | JSON body `{plant_name, plant_scientific_name?, question}`, same required + optional headers |

All HTTP body parsing and header extraction happens in `handlers.go` (HTTP layer); the `proxy` package's `Identify` / `Chat` functions take already-validated typed arguments.

### 1.4 Outputs

| Function | Output | Error cases |
|---|---|---|
| `Identify(...)` | `*IdentifyResult` (suggestions list + is_plant flag) | `ErrPlantIDBadResponse`, `ErrPlantIDUnauthorized`, `ErrPlantIDUnavailable`, `ErrPlantIDRateLimit` |
| `Chat(...)` | `*ChatResult` (single `Answer` string) | `ErrOpenAIBadResponse`, `ErrOpenAIUnauthorized`, `ErrOpenAIUnavailable`, `ErrOpenAIRateLimit` |
| HTTP `/v1/identify` | 200 JSON (see §2.1) | 4xx/5xx per §3 |
| HTTP `/v1/ai-chat` | 200 JSON (see §2.2) | 4xx/5xx per §3 |

### 1.5 External dependencies

- **Plant.id v3** — `POST https://plant.id/api/v3/identification` (multipart with `images` field, header `Api-Key`). [Docs](https://github.com/flowerchecker/Plant-id-API)
- **OpenAI Chat Completions** — `POST https://api.openai.com/v1/chat/completions` (JSON, header `Authorization: Bearer ...`).
- `github.com/yaochen1125/yardmate-api/secrets` — for `PLANT_ID_API_KEY` and `OPENAI_API_KEY` at startup (key never returned to clients).
- `github.com/yaochen1125/yardmate-api/ratelimit` — extended with per-deviceInstallId token bucket.
- Standard library only for HTTP / JSON / multipart / context (no SDK).

---

## 2. Endpoint contracts

### 2.1 `POST /v1/identify`

**Request:**

- Content-Type: `multipart/form-data; boundary=...`
- Form fields:
  - `image` (file, required) — JPEG or PNG, ≤8 MB
- Required headers:
  - `X-Device-Install-Id: <RFC4122 UUID>`
  - `X-App-Version: <semver>` (e.g. `1.1.1`)
- Optional headers (logged, not enforced V1):
  - `X-AppAttest-KeyID: <base64-std>`
  - `X-AppAttest-Assertion: <base64-std>`
  - `X-AppAttest-Challenge: <base64-std>` (challenge previously fetched from `/v1/secrets/challenge` or future risk-signal challenge endpoint)

**Response 200:**

```json
{
  "is_plant": true,
  "is_plant_confidence": 0.99,
  "suggestions": [
    {
      "name": "Monstera deliciosa",
      "scientific_name": "Monstera deliciosa",
      "common_names": ["Swiss cheese plant", "Split-leaf philodendron"],
      "confidence": 0.94
    },
    { /* up to 3 entries total */ }
  ]
}
```

Server returns **top 3 suggestions max** to keep payload bounded.

If `is_plant_confidence < 0.5` server still returns the top suggestions (UI decides what to show); it does not 4xx on "not a plant".

### 2.2 `POST /v1/ai-chat`

**Request:**

- Content-Type: `application/json`
- Body:

  ```json
  {
    "plant_name": "Monstera deliciosa",
    "plant_scientific_name": "Monstera deliciosa",
    "question": "How often should I water it indoors in winter?"
  }
  ```

  - `plant_name`: required, ≤100 chars
  - `plant_scientific_name`: optional, ≤100 chars
  - `question`: required, ≤500 chars

- Required headers + optional headers: same as `/v1/identify`.

**Response 200:**

```json
{
  "answer": "Monstera deliciosa prefers..."
}
```

Server-side OpenAI request: `model=gpt-4o-mini`, `max_tokens=500`, `temperature=0.7`, system prompt is server-controlled (not client-settable) — see §4.

---

## 3. Error code matrix

All errors return:

```json
{ "error": "<machine_code>", "message": "<human_readable_en>" }
```

`<machine_code>` is stable for client-side branching.

| Code | HTTP | Meaning | Client action |
|---|---|---|---|
| `bad_json` | 400 | malformed JSON body (`/v1/ai-chat`) | bug fix client; never retry as-is |
| `bad_multipart` | 400 | malformed multipart body (`/v1/identify`) | bug fix client |
| `missing_image` | 400 | no `image` form field | bug fix client |
| `missing_device_id` | 400 | `X-Device-Install-Id` absent or not a UUID | bug fix client |
| `missing_app_version` | 400 | `X-App-Version` absent | bug fix client |
| `missing_field` | 400 | required JSON field absent / empty (`plant_name` or `question`) | user input UX (not a bug) |
| `plant_name_too_long` | 400 | >100 chars | trim |
| `question_too_long` | 400 | >500 chars | trim |
| `bad_image` | 400 | wrong MIME type, or upstream Plant.id rejected as not an image | user UX: retake photo |
| `image_too_large` | 413 | >8 MB | resize + retry |
| `body_too_large` | 413 | overall body cap exceeded (`/v1/ai-chat` >64 KB) | bug fix client |
| `rate_limit_ip` | 429 | per-IP bucket exhausted; `Retry-After` header set | back off; user UX message |
| `rate_limit_device` | 429 | per-device bucket exhausted; `Retry-After` set | back off; user UX message |
| `plant_id_unavailable` | 502 | Plant.id 5xx / timeout / transient | retry with backoff |
| `openai_unavailable` | 502 | OpenAI 5xx / timeout / transient | retry with backoff |
| `plant_id_unauthorized` | 502 | Plant.id 401/403 — server config issue, NOT a client problem | client shows generic "service issue" |
| `openai_unauthorized` | 502 | OpenAI 401 — server config issue | same |
| `internal` | 500 | unmapped upstream error | retry; alert backend |

---

## 4. Rate limit + body caps

### 4.1 Buckets (token bucket, initial defaults; tunable via env)

| Scope | Endpoint | Rate |
|---|---|---|
| per-IP | `/v1/identify` | 30 req / hour |
| per-deviceInstallId | `/v1/identify` | 100 req / day |
| per-IP | `/v1/ai-chat` | 60 req / hour |
| per-deviceInstallId | `/v1/ai-chat` | 200 req / day |

Per-deviceInstallId is enforced **after** per-IP (cheap reject first), and **after** request body parse (so a malformed request doesn't burn quota).

Storage: extend `ratelimit/` with a `DeviceBucket` keyed by `(deviceInstallId, route)`. Persistence shares the existing BoltDB at `/var/lib/yardmate-api/credentials.db` but in a new bucket `rate_limits_device`.

Env override knobs:

- `YARDMATE_API_RL_IDENTIFY_IP` (default `30`, unit: req/hour)
- `YARDMATE_API_RL_IDENTIFY_DEVICE` (default `100`, req/day)
- `YARDMATE_API_RL_AICHAT_IP` (default `60`, req/hour)
- `YARDMATE_API_RL_AICHAT_DEVICE` (default `200`, req/day)

### 4.2 Body caps

| Cap | Value | Source |
|---|---|---|
| `/v1/identify` total body | 9 MB (8 MB image + multipart overhead headroom) | `http.MaxBytesReader` |
| `/v1/ai-chat` total body | 64 KB | `http.MaxBytesReader` |
| Upstream upload to Plant.id | streamed via `io.Pipe`, max 8 MB image | enforced server-side |

### 4.3 Server-side OpenAI prompt template (locked, not client-settable)

```
SYSTEM:
You are a plant care assistant for YardMate. Provide concise, practical advice
in 2-3 short paragraphs. Always answer in English. Use the plant context the
client provides; if the question is unrelated to plant care, briefly redirect.
Never include external links or step-by-step instructions for unsafe pesticide
use.

USER:
Plant: {plant_name}{, scientific name: {plant_scientific_name}, if present}
Question: {question}
```

`max_tokens=500`, `temperature=0.7`.

---

## 5. Security model + threat model

### 5.1 Threat model assumptions

- Endpoints are public; anyone knowing the URL can hit them.
- App Attest assertion verify is **broken on iOS 26** (see memory `option_d_progress.md`). V1 does not enforce App Attest. Even if it worked, App Attest only proves "an instance of this iOS app" — it doesn't prevent abuse from a real installed app.
- `X-Device-Install-Id` is client-generated and **trivially spoofable**. It's only used for rate-limit scoping.

### 5.2 Defense layers (defense in depth, V1)

| Layer | What it prevents |
|---|---|
| TLS via Let's Encrypt at nginx | passive eavesdropping + MITM |
| `http.MaxBytesReader` body cap | OOM via huge upload |
| Hard timeout on upstream calls (30 s connect+read) | slow-loris / Plant.id hang exhausting our goroutines |
| Per-IP rate limit | single-host abuse |
| Per-deviceInstallId rate limit | spoofed-IP abuse (still rate-limited per device unless attacker rotates IDs) |
| OpenAI `max_tokens` cap + server-controlled system prompt | prompt-injection cost amplification |
| Sanitized response (curated fields only) | upstream Plant.id / OpenAI internal-detail leak |

### 5.3 Compromise scenarios (and what V1 accepts)

- **Attacker rotates device IDs + IPs to drain Plant.id quota.** V1 accepts this risk; mitigation is per-IP rate limit + monthly Plant.id budget alarm (operator-side, not in this package).
- **Attacker sends a non-image as `image` field.** Plant.id returns 400 / `not_a_plant`; we return `bad_image` to client. No harm beyond a wasted Plant.id call.
- **Attacker sends prompt-injection in `question`.** OpenAI's own moderation + our `max_tokens=500` cap + server system prompt limit damage. No user content stored, so prompt-injection has no persistent target.
- **Attacker forges App Attest headers.** V1 logs the forged values; doesn't enforce. No additional capability granted.
- **iOS 26 App Attest behavior changes (Apple fixes the bug).** Add `attest.SoftVerifyAssertion()` shim, call from proxy handlers, populate a `risk_score` field in the log line. Eventually use the score to deny / soft-deny / require-CAPTCHA. Not in V1.

### 5.4 Not a security boundary in V1

- App Attest verification (broken upstream)
- Device install ID validity (client-generated)
- App version freshness (only logged; no version gating yet)

When V1.1+ adds adaptive risk scoring or per-user identity (sign-in), these become enforceable.

---

## 6. Pitfalls (don't re-rediscover)

1. **Plant.id `images` field name.** v3 uses `images` (plural) form-data field name, even for a single image. Do not name it `image`.
2. **Plant.id returns `is_plant.binary` AND `is_plant.probability`** — we want `is_plant_confidence = .probability`. Don't confuse with the `is_plant.binary` boolean.
3. **OpenAI requires `Content-Type: application/json` exactly** — `application/json; charset=utf-8` works but make sure no extra bytes.
4. **Do not buffer the full image to memory before Plant.id upload.** Use `multipart.NewReader` on the request body + `io.Pipe` to stream to the upstream request. Otherwise an 8 MB image × N concurrent requests will OOM.
5. **Do not leak upstream errors verbatim.** Plant.id error bodies sometimes include internal IDs / quotas; OpenAI errors sometimes include partial prompt echoes. Map to fixed error codes per §3.
6. **`Retry-After` header on 429** is mandatory for client UX. Set to the bucket's refill time in seconds.
7. **OpenAI rate limits are org-wide, not request-wide.** Our per-IP / per-device limits are well below OpenAI's free-tier org cap (~10K TPM for `gpt-4o-mini`) so we won't trip OpenAI's throttle in V1. Monitor anyway.
8. **Image MIME validation must happen on actual bytes**, not just the multipart `Content-Type` header. Use `http.DetectContentType` on first 512 bytes after multipart parse.
9. **Plant.id v3 vs v2.** v3 is required (better identification). Endpoint differs from v2 — use exactly `/api/v3/identification`.
10. **OpenAI prompt injection — `question` field must NOT be interpolated into system prompt.** Always pass user input as a separate `user` message, never concatenated into the `system` message. (Templated in §4.3 — verify implementation matches.)
11. **Plant.id v3 returns HTTP 201 Created on success, not 200 OK.** Each identification creates a server-side resource (with `access_token`), so 201 is semantically correct per Plant.id's API. Server must accept **both 200 and 201** as the success path. Verified in production smoke test on 2026-05-14 — initial implementation only accepted 200 and rejected every real identification as `bad_response: status 201`. Regression covered by `TestPlantIDClient_Identify_Accepts201Created`.

---

## 7. Resolved decisions (don't re-debate)

- **Plant.id v3, not v2.** Better quality + maintained.
- **OpenAI `gpt-4o-mini`, not `gpt-4o`.** Cost: ~$0.15/1M input tokens vs $5; for plant care advice (typically <500 tokens out) the quality delta is acceptable.
- **No conversation history server-side in V1.** Stateless. iOS client manages multi-turn history if/when needed.
- **No retry of upstream calls in V1.** Plant.id / OpenAI 5xx → return 502 immediately. Client retries with user-driven backoff. Server-side retry adds complexity for marginal value at V1 scale.
- **App Attest stays in V1 binary but unused in proxy code path.** Removing it would conflict with V1.1 revival plan + memory `option_d_progress.md` deprecation note. Keep `/v1/attest/challenge` + `/v1/attest/register` + `/v1/secrets/challenge` + `/v1/app-secrets` registered (existing tests stay green). V1 iOS client doesn't call them.
- **Headers `X-AppAttest-*` (optional) accepted for forward compat.** Server logs them; doesn't act on them. V1.1 + revival of App Attest can wire `attest.SoftVerifyAssertion()` here without changing the wire contract.
- **Per-deviceInstallId rate limit storage in BoltDB.** Same DB as `attest` credentials. Separate top-level bucket (`rate_limits_device`). Sweep stale entries weekly (a small admin task; not blocking V1).
- **Image MIME accepted: jpeg, png only.** No HEIC (iOS auto-converts on share-sheet; iOS app should ensure jpeg/png at upload time).

---

## 8. Out-of-scope (V1.1+ candidates)

- HEIC support (iOS native format) — requires server-side conversion.
- Health assessment (`/api/v3/health_assessment` on Plant.id) — separate endpoint.
- Multi-turn chat with server-side history — needs auth + storage.
- Adaptive risk scoring using App Attest + behavior signals.
- Image preview / thumbnail caching for client (would require image storage on our side, decided against for V1).
- Short-TTL token vending (D-Server revival, replaces `/v1/app-secrets`).
- Internationalization of `answer` text (V1 English-only per `app_language` memory).
