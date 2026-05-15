# `proxy` package — server-side proxy for Plant.id + OpenAI vision (V1)

> Status: **shipped** (initial proxy `f4e4f35` 2026-05-14, deployed to `api.yardmate.ai`; diagnose + ai_enhance + per-device rate limit added 2026-05-15).
> Companion (client): `yardmate-swiftui/app/YardMate/YardMate/Identify/SPEC.md` and the camera / recognition / disease-detail Feature docs under `docs/releases/v1/main-navigation/snap/`.
> Background: this package replaces the D-Server "key vending" flow (`/v1/app-secrets`) for V1, because the iOS 26 App Attest assertion verification path is blocked by an upstream issue (see memory `option_d_progress.md`). The `secrets` package + `/v1/app-secrets` endpoint stay compiled in and tested but are marked **deprecated for V1**. Revival when Apple addresses the iOS 26 assertion behavior.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Accept image upload from the iOS client, forward to Plant.id v3 for plant identification, sanitize the response, return to client (`POST /v1/identify`).
- Accept image upload from the iOS client, forward to Plant.id v3 with `health=all`, cross-reference Plant.id's disease names against the YardMate catalog (1522 plants × 70 disease entries embedded at build time), and return a normalized `DiagnoseResult` with `plantId` / `catalogId` mapping (`POST /v1/diagnose`).
- Apply optional LLM post-processing when the operator-configured OpenAI key is present: rerank Plant.id top-N candidates on `/v1/identify?ai_enhance=true`, and disambiguate Plant.id disease names that don't match the YardMate catalog on `/v1/diagnose`.
- Enforce two-layer rate limit (per-IP and per-device, both at the /v1 router scope; see §4.1) and a hard 8 MB image size cap on every endpoint.
- Log every request's risk signals (deviceInstallId, IP, app version, App Attest assertion presence if header is set, outcome, latency) for forensics + future adaptive risk scoring.
- Map upstream Plant.id and OpenAI errors to stable client-facing error codes; never leak upstream raw error bodies.

### 1.2 What this package is NOT responsible for

- **Storing images.** The endpoint is stream-through and stateless — no R2, no disk, no DB write of user-content bytes.
- **App Attest verification.** That's the `attest` package. V1 reads the (optional) assertion headers from the request and logs the keyID + assertion presence; it does **not** call `attest.VerifyAssertion()` (the iOS 26 issue would only produce noise). When Apple addresses iOS 26, a `attest.SoftVerifyAssertion()` shim can be added and called from this package as a risk signal.
- **Secret vending.** The `secrets` package and `/v1/app-secrets` route still exist but are deprecated for V1 and not consumed by V1 clients. See `secrets/SPEC.md` §"Deprecation for V1".
- **User accounts / identity / billing.** YardMate V1 has no user accounts; the only client identifier is `X-Device-Install-Id` (a client-generated UUID stored in Keychain). It is **not** an authenticated identity — only a rate-limit / risk-signal scope.
- **Plant.id account management.** API key + billing are operator concerns; this package only consumes the key from `secrets.Vault`.
- **Caching.** No response cache. Identification results are user-content-specific and idempotency isn't valuable enough for V1.
- **Multi-tenancy.** All requests use the same upstream Plant.id account.
- **Chat / care advice / generative description enrichment.** The vision client in this package only does (a) candidate reranking and (b) catalog-name disambiguation. Plant-detail enrichment (descriptions, watering tips, full detail-page data for plants outside the 1522 catalog) is the sibling `proxy/enrichment` package, invoked from the iOS plant-detail flow on detail-page mount, not from `/v1/identify` or `/v1/diagnose`. The two packages share the OpenAI HTTP transport (`VisionClient.post`) but have separate prompt paths. See `proxy/enrichment/SPEC.md`.

### 1.3 Inputs

| Function | Input |
|---|---|
| `PlantIDClient.Identify(ctx, image io.Reader, mime)` | image stream + MIME (`image/jpeg` or `image/png`); ≤8 MB |
| `PlantIDClient.Diagnose(ctx, image []byte, mime)` | image bytes + MIME; bytes because the upstream needs a base64 JSON body |
| `VisionClient.RerankIdentify(ctx, image, mime, candidates)` | image bytes + Plant.id top-N |
| `VisionClient.DisambiguateDiseaseName(ctx, name, refs)` | text-only |
| HTTP `POST /v1/identify` | multipart/form-data: `image` (file, required) + `ai_enhance` (text "true"/"1"/"yes", optional, default false). Required headers `X-Device-Install-Id` + `X-App-Version`, optional `X-AppAttest-*` |
| HTTP `POST /v1/diagnose` | multipart/form-data: `image` (file, required). Same header set as `/v1/identify` |

All HTTP body parsing and header extraction happens in `handlers.go` (HTTP layer); the typed package functions take already-validated arguments.

### 1.4 Outputs

| Function | Output | Error cases |
|---|---|---|
| `Identify(...)` | `*IdentifyResult` (suggestions list + is_plant flag + `ai_enhanced_at`) | `ErrPlantIDImageRejected`, `ErrPlantIDUnauthorized`, `ErrPlantIDUnavailable`, `ErrPlantIDRateLimit`, `ErrPlantIDBadResponse` |
| `Diagnose(...)` | `*plantIDDiagnoseResponse` (raw upstream shape, sanitized in handler into `DiagnoseResult`) | same set as `Identify` |
| `VisionClient.RerankIdentify(...)` | picked candidate name or `error` (handler keeps Plant.id ordering on error) | network / non-200 / decode / hallucinated pick |
| HTTP `/v1/identify` | 200 JSON (see §2.1) | 4xx/5xx per §3 |
| HTTP `/v1/diagnose` | 200 JSON (see §2.2) | 4xx/5xx per §3 |

### 1.5 External dependencies

- **Plant.id v3** — `POST https://plant.id/api/v3/identification` (multipart with `images` field for Identify; JSON body with base64 data URLs + `health=all` for Diagnose; header `Api-Key`). [Docs](https://github.com/flowerchecker/Plant-id-API)
- **OpenAI chat-completions (vision)** — `POST https://api.openai.com/v1/chat/completions` with `gpt-4o-2024-08-06`. Used for `ai_enhance` rerank (multimodal) and `/v1/diagnose` catalog-id disambiguation (text-only). 8 s client timeout.
- `github.com/yaochen1125/yardmate-api/secrets` — for `PLANT_ID_API_KEY` and `OPENAI_API_KEY` at startup (keys never returned to clients).
- `github.com/yaochen1125/yardmate-api/ratelimit` — per-IP middleware on the `/v1` scope plus per-device middleware on the proxy endpoint group (`/v1/identify`, `/v1/diagnose`).
- **Embedded catalog JSON** (`proxy/data/{plants_index,plants_detail,diseases}.json`) — built into the binary via `//go:embed`. ~10 MB binary footprint, lookup map built once at startup.
- Standard library only for HTTP / JSON / multipart / context (no third-party SDK).

---

## 2. Endpoint contract

### 2.1 `POST /v1/identify`

**Request:**

- Content-Type: `multipart/form-data; boundary=...`
- Form fields:
  - `image` (file, required) — JPEG or PNG, ≤8 MB
  - `ai_enhance` (text, optional) — `true` / `1` / `yes` opts into the LLM rerank pass. Any other value (or the field's absence) is treated as false.
- Required headers:
  - `X-Device-Install-Id: <RFC4122 UUID>`
  - `X-App-Version: <semver>` (e.g. `1.1.1`)
- Optional headers (logged, not enforced V1):
  - `X-AppAttest-KeyID: <base64-std>`
  - `X-AppAttest-Assertion: <base64-std>`
  - `X-AppAttest-Challenge: <base64-std>`

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
    }
  ],
  "ai_enhanced_at": "2026-05-15T12:34:56Z"
}
```

Server returns **top 3 suggestions max** to keep payload bounded.

If `is_plant_confidence < 0.5` server still returns the top suggestions (UI decides what to show); it does not 4xx on "not a plant".

**`ai_enhanced_at` semantics:**

- `null` when `ai_enhance` was absent / false / unknown value;
- `null` when `ai_enhance=true` but the OpenAI key is not configured server-side (degraded; warned in logs);
- `null` when `ai_enhance=true` but the LLM call failed / timed out / returned a name outside the candidate list — the original Plant.id ranking is preserved untouched;
- an RFC 3339 UTC timestamp marking when the rerank completed iff the picked candidate has been promoted to `suggestions[0]`. Clients can ignore this field entirely; it's a forensics / debugging signal.

**Rerank behavior:**

The handler passes the same image bytes plus the Plant.id top-3 names + scientific names to `gpt-4o-2024-08-06` (vision). The model is constrained to reply with one of the candidate names verbatim. If the model picks a name that matches a candidate (exact-name or case-insensitive contains), that candidate is moved to index 0 of `suggestions` — its `confidence` is **not** rewritten (it remains the Plant.id-assigned probability).

### 2.2 `POST /v1/diagnose`

Combined identification + health assessment. Same image upload pattern as `/v1/identify`; upstream Plant.id call uses `health=all` plus the catalog details query (`?details=local_name,description,treatment,cause&language=en`).

**Request:**

- Content-Type: `multipart/form-data; boundary=...`
- Form fields:
  - `image` (file, required) — JPEG or PNG, ≤8 MB
- Same required + optional headers as `/v1/identify` (`X-Device-Install-Id`, `X-App-Version`, optional `X-AppAttest-*`).

**Response 200 (healthy):**

```json
{
  "identifiedName": "Abelia chinensis",
  "plantId": "AAA0001",
  "isHealthy": true,
  "healthProbability": 0.92,
  "top": {
    "name": "Abelia chinensis",
    "scientific_name": "Abelia chinensis",
    "common_names": ["Chinese Abelia"],
    "confidence": 0.94
  },
  "issues": []
}
```

The iOS client routes a healthy result into the plant-detail page and surfaces a toast confirming "this plant is healthy". Server does **not** manufacture a fake disease card.

**Response 200 (unhealthy):**

```json
{
  "identifiedName": "Rosa chinensis",
  "plantId": "AAB0234",
  "isHealthy": false,
  "healthProbability": 0.21,
  "top": { "name": "Rosa chinensis", "...": "..." },
  "issues": [
    {
      "name": "Powdery mildew",
      "catalogId": "L20",
      "probability": 0.76,
      "description": "white powdery coating on leaves",
      "cause": "high humidity + poor airflow",
      "isFallback": false,
      "treatment": {
        "biological": ["neem oil spray"],
        "chemical":   ["copper fungicide"],
        "prevention": ["increase airflow"]
      }
    }
  ]
}
```

Top-3 issues max. Issues are guaranteed non-empty when `isHealthy=false`.

**`plantId` mapping** (`scientific_name` → YardMate `plantId`):

1. Exact match in the embedded `plants_index.json` (1522 entries today). Match is case-insensitive.
2. Fuzzy match with normalization: lowercased, trimmed, hybrid markers (`×` / stand-alone `x`) dropped, variety / cultivar / subspecies suffixes stripped (`var. X`, `cv. X`, `subsp. X`, `ssp. X`, `f. X`, `forma X`).
3. `plantId: null` on miss — iOS detail page must tolerate this and render with Plant.id-only data.

**`catalogId` mapping** (Plant.id disease `name` → YardMate catalog id):

1. Exact / fuzzy match against the 70 catalog names in `diseases.json`. Fuzzy normalization lowercases, trims, and strips `disease` / `infection` suffixes.
2. LLM disambiguation: GPT-4o text-only is fed the input name + the full (id, name) list and asked to reply with a single catalog id or `NONE`. Hallucinated ids (not in the catalog) are treated as misses. ~70-entry prompt is small enough that we don't cache the catalog list payload.
3. `catalogId: null` on miss.

**Issues fallback (F-option-2, "honest fallback"):**

- `isHealthy=true` → `issues=[]`. iOS shows the healthy toast on the detail page.
- `isHealthy=false` AND Plant.id returned at least one suggestion → top-3 suggestions are mapped through the catalogId logic above and shipped. `isFallback=false`.
- `isHealthy=false` AND Plant.id returned **zero** disease suggestions → server constructs one `isFallback=true` issue. Source order: (a) the plant's `common_diseases_list[0]` from `plants_detail.json` if `plantId` is known; (b) generic L06 "Leaf spot"; (c) a minimal hard-coded leaf-spot shape if even L06 is unavailable (defensive — should not happen with the embedded catalog).

The server **never** ships `isHealthy=false` with an empty `issues` array.

---

## 3. Error code matrix

All errors return:

```json
{ "error": "<machine_code>" }
```

`<machine_code>` is stable for client-side branching.

All codes apply to both `/v1/identify` and `/v1/diagnose`.

| Code | HTTP | Meaning | Client action |
|---|---|---|---|
| `bad_multipart` | 400 | malformed multipart body | bug fix client |
| `missing_image` | 400 | no `image` form field | bug fix client |
| `missing_device_id` | 400 | `X-Device-Install-Id` absent or not a UUID | bug fix client |
| `missing_app_version` | 400 | `X-App-Version` absent | bug fix client |
| `bad_image` | 400 | wrong MIME type by byte sniff, or upstream Plant.id rejected as not an image | user UX: retake photo |
| `image_too_large` | 413 | >8 MB | resize + retry |
| `rate_limit_ip` | 429 | per-IP bucket exhausted; `Retry-After` header set | back off; user UX message |
| `rate_limit_device` | 429 | per-device bucket exhausted (`X-Device-Install-Id` key); `Retry-After` header set | back off; user UX message |
| `plant_id_unavailable` | 502 | Plant.id 5xx / timeout / transient (including 429 from Plant.id) | retry with backoff |
| `plant_id_unauthorized` | 502 | Plant.id 401/403 — server config issue, NOT a client problem | client shows generic "service issue" |
| `internal` | 500 | unmapped upstream error | retry; alert backend |

Note: OpenAI vision failures are **never** surfaced to the client. `ai_enhance` rerank failures leave `ai_enhanced_at: null` in a 200 response; `/v1/diagnose` catalog disambiguation failures leave `catalogId: null` on the issue. Both are warn-logged server-side.

---

## 4. Rate limit + body cap

### 4.1 Two-layer rate limit

Both layers are applied as chi middleware in `server.go`:

| Layer | Scope | Key | Default | Env override | Error code |
|---|---|---|---|---|---|
| Per-IP | All `/v1/*` routes | client IP (chi `middleware.RealIP`) | 100 / hour | `YARDMATE_API_RL_IP_LIMIT` / `_WINDOW` | `rate_limit_ip` |
| Per-device | Proxy endpoint group only (`/v1/identify`, `/v1/diagnose`) | `X-Device-Install-Id` UUID | 100 / hour | `YARDMATE_API_RL_DEVICE_LIMIT` / `_WINDOW` | `rate_limit_device` |

Both return 429 with `Retry-After` header on exhaustion. The two layers compose multiplicatively against the IP-rotation-but-reused-device attack: an attacker who rotates outbound IPs (defeating per-IP) still gets blocked once their install hits the per-device bucket.

**Per-device behaviour on missing / malformed `X-Device-Install-Id`:** the middleware passes through (no rate-limit decision recorded) and the handler 400s with `missing_device_id`. This intentionally avoids a "global empty-string bucket" shared by every malformed request.

**Storage:** in-memory fixed-window per `ratelimit/SPEC §2`. Process restart resets counters. BoltDB persistence is **V1.1**: when added, both per-IP and per-device buckets will be persisted; the keyID bucket stays in-memory (its assertion-verify guard already covers the cold-start case).

### 4.2 Body cap

| Cap | Value | Source |
|---|---|---|
| `/v1/identify` total body | 9 MB (8 MB image + multipart overhead headroom) | `http.MaxBytesReader` |
| `/v1/diagnose` total body | 9 MB (same as identify) | `http.MaxBytesReader` |
| Upstream upload to Plant.id (Identify) | buffered in `bytes.Buffer`, bounded by the 9 MB cap above | `proxy/plant_id.go` |
| Upstream upload to Plant.id (Diagnose) | buffered + base64-encoded into JSON body (~33% inflation), bounded by 9 MB raw cap | `proxy/plant_id.go` |
| LLM vision payload | image bytes embedded as `data:image/...;base64,...` URL in chat-completion `image_url` | `proxy/openai_vision.go` |

Streaming via `io.Pipe` to the upstream is a V1.1 optimization (SPEC §6 pitfall 4); at V1 scale (per-IP + per-device caps × bounded concurrency) the memory cost is acceptable.

---

## 5. Security model + threat model

### 5.1 Threat model assumptions

- The endpoint is public; anyone knowing the URL can hit it.
- App Attest assertion verify is **broken on iOS 26** (see memory `option_d_progress.md`). V1 does not enforce App Attest. Even if it worked, App Attest only proves "an instance of this iOS app" — it doesn't prevent abuse from a real installed app.
- `X-Device-Install-Id` is client-generated and **trivially spoofable**. It's only used for forensics scoping (and V1.1+ per-device rate limit).

### 5.2 Defense layers (defense in depth, V1)

| Layer | What it prevents |
|---|---|
| TLS via Let's Encrypt at nginx | passive eavesdropping + MITM |
| `http.MaxBytesReader` body cap | OOM via huge upload |
| Hard 30 s timeout on the upstream Plant.id call; 8 s on OpenAI vision | slow-loris / upstream hang exhausting our goroutines |
| Per-IP rate limit at `/v1` scope | single-host abuse |
| Per-device rate limit on proxy endpoint group | abuse from a single install (rotated IPs) |
| Sanitized response (curated fields only) | upstream Plant.id / OpenAI internal-detail leak |
| MIME byte-sniff on first 512 bytes (not just multipart `Content-Type`) | upload of non-image payloads disguised as JPEG/PNG |
| Catalog-id whitelisting on LLM disambiguation reply | LLM-hallucinated id leaking to the client |

### 5.3 Compromise scenarios (and what V1 accepts)

- **Attacker rotates IPs to drain Plant.id / OpenAI quota.** Per-IP alone wouldn't block this. Per-device (V1, this PR) raises the cost — an attacker now needs to rotate both IP and device install id. V1.1+ may add adaptive scoring on App Attest signals when iOS 26 lands the fix.
- **Attacker sends a non-image as `image` field.** MIME byte-sniff rejects most cases with `bad_image` (400). If somehow it passes our sniff but fails Plant.id, Plant.id returns 400 and we map to `bad_image` (400). No harm beyond a wasted Plant.id call.
- **Attacker forges App Attest headers.** V1 logs the forged values; doesn't enforce. No additional capability granted.
- **iOS 26 App Attest behavior changes (Apple fixes the bug).** Add `attest.SoftVerifyAssertion()` shim, call from this handler, populate a `risk_score` field in the log line. Eventually use the score to deny / soft-deny / require-CAPTCHA. Not in V1.

### 5.4 Not a security boundary in V1

- App Attest verification (broken upstream)
- Device install ID validity (client-generated)
- App version freshness (only logged; no version gating yet)

When V1.1+ adds adaptive risk scoring or per-user identity (sign-in), these become enforceable.

---

## 6. Pitfalls (don't re-rediscover)

1. **Plant.id `images` field name.** v3 uses `images` (plural) form-data field name, even for a single image. Do not name it `image`. (Identify multipart path.)
2. **Plant.id returns `is_plant.binary` AND `is_plant.probability`** — we want `is_plant_confidence = .probability`. Don't confuse with the `is_plant.binary` boolean.
3. **Do not buffer the full image to memory before Plant.id upload (V1.1 optimization).** Currently V1 does buffer because the 8 MB cap × bounded concurrency makes it acceptable, but switching to `io.Pipe` from `multipart.NewReader` is the right architecture once concurrency grows.
4. **Do not leak upstream errors verbatim.** Plant.id error bodies sometimes include internal IDs / quotas; OpenAI error bodies include model id + request id. Map both to fixed error codes per §3 (or to "ai_enhanced_at: null" / "catalogId: null" for the soft-degrade paths).
5. **`Retry-After` header on 429** is mandatory for client UX. Both `PerIPMiddleware` and `PerDeviceMiddleware` set this via `ratelimit.Write429`.
6. **Image MIME validation must happen on actual bytes**, not just the multipart `Content-Type` header. Use `http.DetectContentType` on first 512 bytes after multipart parse.
7. **Plant.id v3 vs v2.** v3 is required (better identification). Endpoint differs from v2 — use exactly `/api/v3/identification`.
8. **Plant.id v3 returns HTTP 201 Created on success, not 200 OK.** Each identification creates a server-side resource (with `access_token`), so 201 is semantically correct per Plant.id's API. Server must accept **both 200 and 201** as the success path. Regression covered by `TestPlantIDClient_Identify_Accepts201Created` and `TestPlantIDClient_Diagnose_Accepts201Created`.
9. **Diagnose uses JSON body + base64**, not multipart. Plant.id v3 accepts both, but the JSON shape is required for the sibling `health=all` flag. Pay the ~33% base64 inflation cost in the upstream POST.
10. **`description` may be a string OR an object.** Plant.id occasionally returns `disease.suggestions[*].details.description` as `{value, citations}` instead of a plain string. The struct types it as `any` and `diagnoseDescriptionString` flattens both shapes.
11. **LLM disambiguation reply must be whitelisted.** GPT-4o sometimes adds trailing prose ("L20 — Powdery mildew") or hallucinates ids ("ZZ99"). Strip the leading token, validate against the catalog, treat anything else as a miss. Never blind-trust the reply.
12. **`ai_enhance` form field is text, not boolean.** Multipart text parts arrive as strings; the handler accepts `true` / `1` / `yes` and treats everything else as false. The server NEVER returns a 4xx for an unknown ai_enhance value — silent default.
13. **Hybrid marker normalization.** `Abelia × grandiflora` (Unicode ×) and `Abelia x grandiflora` (ASCII x as a stand-alone token) must hash to the same key. `normalizeScientificName` drops both forms; stand-alone `x` as a field is removed, but `x` as a substring of a real epithet (e.g. `Buxus`) survives.

---

## 7. Resolved decisions (don't re-debate)

- **Plant.id v3, not v2.** Better quality + maintained.
- **No retry of upstream calls in V1.** Plant.id 5xx → return 502 immediately. Client retries with user-driven backoff. Server-side retry adds complexity for marginal value at V1 scale.
- **App Attest stays in V1 binary but unused in proxy code path.** Removing it would conflict with V1.1 revival plan + memory `option_d_progress.md` deprecation note. Keep `/v1/attest/challenge` + `/v1/attest/register` + `/v1/secrets/challenge` + `/v1/app-secrets` registered (existing tests stay green). V1 iOS client doesn't call them.
- **Headers `X-AppAttest-*` (optional) accepted for forward compat.** Server logs them; doesn't act on them. V1.1 + revival of App Attest can wire `attest.SoftVerifyAssertion()` here without changing the wire contract.
- **Image MIME accepted: jpeg, png only.** No HEIC (iOS auto-converts on share-sheet; iOS app should ensure jpeg/png at upload time).
- **OpenAI over Anthropic for the V1 vision client.** GPT-4o-2024-08-06 is ~20% cheaper per call for our short prompt + short output workload; prompt-caching wouldn't help (candidate names are per-request). Anthropic Sonnet 4.6 is a fine option if we ever standardize on a single text-generation provider, but for the rerank / disambiguate pattern OpenAI is the V1 choice.
- **`ai_enhance` is a form field, not a query param.** Keeps the call-site symmetric (clients already do multipart for the image) and avoids cache-key surface on any future CDN. Truthy values: `true` / `1` / `yes`.
- **Embedded catalog over CDN fetch at runtime.** `proxy/data/*.json` ships in the binary (~10 MB). Runtime CDN dependency would make `/v1/diagnose` unable to serve when jsDelivr is degraded; we'd rather redeploy when the catalog grows.
- **F-option-2 (honest fallback).** Healthy plants get `issues: []`; iOS handles the "this plant is healthy" UX. Server never manufactures a fake disease for a healthy plant. Only the unhealthy-but-empty-suggestions case synthesizes a fallback (and marks `isFallback: true`).
- **No LLM rerank `confidence` rewrite.** When the rerank promotes a candidate, its Plant.id-assigned `confidence` stays. We trust Plant.id's probability calibration more than the LLM's.

---

## 8. Out-of-scope (V1.1+ candidates)

- HEIC support (iOS native format) — requires server-side conversion.
- AI care advice / chat (originally drafted as `/v1/ai-chat` with OpenAI gpt-4o-mini; removed in commit after `f4e4f35` because V1 has no chat feature in scope). Re-add as its own feature PR if/when product needs it; the OpenAI proxy pattern is recoverable from git history (current `proxy/openai_vision.go` provides the `post` helper that can be reused).
- Adaptive risk scoring using App Attest + behavior signals.
- Image preview / thumbnail caching for client (would require image storage on our side, decided against for V1).
- Short-TTL token vending (D-Server revival, replaces `/v1/app-secrets`).
- BoltDB persistence for per-IP + per-device rate-limit buckets (process restart currently resets both).
- Plant.id monthly quota / alarm — operator concern, not in this package.
- `io.Pipe` streaming for the upstream image upload (V1 buffers to memory for both identify + diagnose).
- Internationalization of suggestion `common_names` and disease descriptions — V1 requests Plant.id with `language=en` per `app_language` memory.
- Catalog versioning + delta-update channel — today a catalog bump requires a server redeploy. V1.1+ may add a fetch-at-startup pattern with build-time fallback.
