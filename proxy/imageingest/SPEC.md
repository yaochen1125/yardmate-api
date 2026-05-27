# `proxy/imageingest` package — plant hero-image ingest (V1 image self-hosting)

> Status: **draft — SPEC for review; implementation in a follow-up commit/PR.**
> Companion: parent `proxy/SPEC.md` (`/v1/identify`, `/v1/diagnose`) + `proxy/enrichment/SPEC.md` (`/v1/plants/enrichment`). This package is a **new, independent domain**. It does NOT hang off enrichment: enrichment SPEC §1.2 explicitly states "**Image storage. No R2 writes; text in / JSON out.**" — that boundary stays. This package is the *only* writer of plant imagery to R2 from the server.
> Background: out-of-catalog plants (identification results / species outside the curated 1522) render a hero image from R2 at `plant_images/{slug}/hero.png`, where `slug` is derived from the scientific name (iOS `PlantImageURL.slug`, shipped PR #227). Until something fills that R2 key, iOS shows a placeholder. This package fetches a license-clean photo from Wikimedia Commons and uploads it to that exact key, so the placeholder fills in. iOS needs **no change** — it already reads the key.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- **Seed discovery (read-only):** read the list of out-of-catalog scientific names the server has already seen, from the enrichment-owned Supabase table `plants_pending` (`SELECT scientific_name WHERE status IN ('pending','approved')`). These are exactly the plants whose iOS detail pages currently show a hero placeholder. No iOS trigger, no new client call — the table *already* accumulates these names because iOS calls `/v1/plants/enrichment` on every out-of-catalog detail-page mount.
- **Per-species hero ingest:** for one scientific name →
  1. compute the R2 `slug` (byte-identical to iOS `PlantImageURL.slug` — §2.3, the central invariant),
  2. skip if already done (idempotency ledger — §6.1),
  3. search Wikimedia Commons for candidate File: pages for that species,
  4. read each candidate's `imageinfo.extmetadata` license, **keep only CC0 / Public-domain / CC-BY / CC-BY-SA** (reject NC / ND / ARR / unknown — §2.4),
  5. pick the best candidate (license tier, then a photo-likeness/size heuristic — §2.5),
  6. download the bytes of Wikimedia's 1600px scaled rendition, stored verbatim (§7 D2),
  7. upload to R2 `yardmate-static/plant_images/{slug}/hero.png` with the real `Content-Type` (§2.6),
  8. record the outcome + attribution in the ledger (§6.1).
- **Batch orchestration:** a bounded pass that ingests up to N un-done seeds, serially, honoring Wikimedia rate-limit etiquette (UA + min interval + `maxlag` + `Retry-After` — §4).
- **Self-driven invocation:** a background ticker (interval/N env-configurable; disable-able) AND a manual internal HTTP trigger for on-demand runs + single-slug testing (§2.1). No public surface.
- **Attribution capture:** persist author / license / source-file-page per ingested image so a future iOS Settings → Credits page (CC §3a2 collected attribution) can be built. Capturing at ingest is mandatory — the data is needed for license compliance and is awkward to reconstruct after the fact.

### 1.2 What this package is NOT responsible for

- **The enrichment endpoint / its logic.** This package only *reads* `plants_pending.scientific_name`. It never writes that table, never imports the `enrichment` or `proxy` Go packages, never generates plant detail JSON. (Data-level read coupling only — §1.5, pitfall §9 #4.)
- **Identification / diagnosis / detail-text generation.** No image *upload* endpoint (that's `/v1/identify` / `/v1/diagnose`); no LLM.
- **In-catalog (1522) imagery.** Curated plants use `plant_images/{AAA-id}/{1_whole|2_closeup|3_state|4_scene}.png`, uploaded by separate offline tooling (the `scripts/` pipeline in `yardmate-swiftui`). This package only fills the **slug**-keyed `hero.png` for **out-of-catalog** plants. It never touches AAA-id keys.
- **Image transformation.** No re-encode, no crop, no resize-by-us, no AI enhancement. Bytes are stored verbatim (§7 D2 — CC-BY-SA ShareAlike is only triggered by *adaptations*; a verbatim copy is not one). The one server-side decode is a read-only sniff for MIME/dimensions; the stored bytes are the downloaded bytes.
- **Genus-level fallback fill.** V1 fills the **species** slug only (`PlantImageURL.slug`), because iOS PR #227 currently reads only the species slug (genus fallback is iOS P2, not yet shipped). The ingest core is **slug-parameterized**, so genus fill (`PlantImageURL.genusSlug`) reuses the same code once iOS reads it — §8.
- **Serving / surfacing the credits to iOS.** V1 captures attribution in the ledger; building the iOS Credits page (or a `/credits` export) is iOS-side + V1.1 (§8). No regression in compliance: attribution is recorded now, just not yet displayed.
- **Promotion of out-of-catalog plants into the curated catalog**, or any change to `plants_detail.json`.
- **iNaturalist / USDA / GBIF sources.** V1 is Wikimedia Commons only. Other sources are §8 candidates; the ledger's `source` column is forward-compatible.

### 1.3 Inputs

| Layer | Input |
|---|---|
| Background ticker | none (self-driven). Env: `IMAGEINGEST_TICK_INTERVAL` (Go duration; `0`/unset → ticker disabled, manual-only), `IMAGEINGEST_BATCH_LIMIT` (int, default 25). |
| Internal HTTP `POST /internal/imageingest/run` | header `X-Ingest-Admin-Token: <token>` (matched against secret `IMAGEINGEST_ADMIN_TOKEN`). Optional query: `?limit=<int>` (cap this run), `?slug=<slug>&name=<scientificName>` (ingest one specific species, for testing — bypasses the seed query). |
| `Ingestor.RunBatch(ctx, limit)` | already-validated; processes up to `limit` un-done seeds; returns a `BatchSummary`. |
| `Ingestor.IngestOne(ctx, slug, searchTerm)` | one species; `slug` is the R2 key segment (caller-derived, parameterized for future genus reuse), `searchTerm` is the Wikimedia query (the scientific name). Returns an `IngestOutcome`. |
| Server config (`secrets.Vault`) | `SUPABASE_DB_URL` (same Session-Pooler DSN as enrichment — §9 #15 there), `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` (`yardmate-static`), optional `R2_ENDPOINT` (default `https://<account>.r2.cloudflarestorage.com`), `IMAGEINGEST_ADMIN_TOKEN`. Optional UA override `IMAGEINGEST_USER_AGENT`. |

### 1.4 Outputs

| Function | Output | Error / outcome cases |
|---|---|---|
| `Ingestor.IngestOne(...)` | `IngestOutcome{Status, R2Key, License, Author, FilePage, Mime, Bytes, Width, Height}` where `Status ∈ {ingested, skipped_exists, no_acceptable_image, source_error, upload_error}` | typed errors collapse into the `Status` + a logged cause; never panics on a single bad species |
| `Ingestor.RunBatch(...)` | `BatchSummary{Seen, Attempted, Ingested, NoImage, Skipped, Errors}` counts | partial failure is normal — one species failing does not abort the pass |
| Internal HTTP `POST /internal/imageingest/run` | 202 JSON `BatchSummary` (or single `IngestOutcome` for `?slug=`); `{"error":"..."}` on auth / config errors (§3) | — |
| R2 side effect | object `plant_images/{slug}/hero.png` (verbatim bytes, real Content-Type) created when `Status==ingested` | — |
| Supabase side effect | one upserted row in `plant_image_ingest` ledger (§6.1) per attempted species | — |

The HTTP trigger returns **counts + per-slug outcomes for forensics only**; it is an ops surface, not a client contract. There is no public/iOS-facing response.

### 1.5 External dependencies

- **Cloudflare R2** (S3-compatible) — bucket `yardmate-static`, public custom domain `images.yardmate.ai`. Accessed via the AWS S3 Go SDK v2 (`github.com/aws/aws-sdk-go-v2/service/s3` + `config` + `credentials`) pointed at the R2 endpoint with region `auto` (Cloudflare's documented R2 access path; correct SigV4). Operations used: `HeadObject` (idempotency double-check) + `PutObject` (upload). **New go.mod dependency** (resolved §7 D5: aws-sdk-go-v2) — yardmate-api currently has no S3/AWS code. Credentials are an R2 **API token** scoped to *Object Read & Write* on `yardmate-static` only.
- **Wikimedia Commons API** — `https://commons.wikimedia.org/w/api.php` (`action=query&generator=search&gsrnamespace=6&prop=imageinfo&iiprop=url|mime|size|extmetadata`) for search + license, and `https://upload.wikimedia.org/...` (the `imageinfo.url`) for the bytes. stdlib `net/http`. Hard requirement: descriptive `User-Agent` with contact (else 403), polite serial pacing + `maxlag=5` + `Retry-After` honoring (§4). No API key (anonymous read).
- **Supabase Postgres** — `plants_pending` (read-only seed; enrichment-owned) + `plant_image_ingest` (this package's own ledger; §6.1). Own `pgx` pool, small (`MaxConns=2`; this is a low-frequency batch worker, not a request hot path). DSN from `SUPABASE_DB_URL` (same secret enrichment uses). Does **not** share enrichment's pool or import its package — decoupled at the Go level, coupled only on the table name (pitfall §9 #4).
- **`secrets.Vault`** — config loader (already exists). R2 creds + admin token are **server-only**; they MUST NOT be added to `main.vendedKeys` (which vends keys to authenticated clients — `secrets/SPEC.md` §4.1). Pitfall §9 #7.
- stdlib only otherwise (`net/http`, `encoding/json`, `context`, `time`, `strings`, `image` for dimension sniff). Third-party limited to pgx (shared) + the S3 SDK.

---

## 2. Contract

### 2.1 Invocation model (Q1 = batch, seeded from the enrichment table)

Two entry points, both wrapping the same `Ingestor` core. **No public/iOS surface** (Q1: iOS PR #227 has no "trigger ingest" call and gets none; choosing batch-from-seed means no follow-up iOS PR).

1. **Background ticker (self-driven).** Started in `main.go` iff R2 + DB are configured (else WARN + disabled, mirroring `buildEnrichmentService`). On each tick (`IMAGEINGEST_TICK_INTERVAL`), runs `RunBatch(ctx, IMAGEINGEST_BATCH_LIMIT)`. Interval `0`/unset ⇒ ticker off (manual-only). **Recommended V1 rollout:** ship with the ticker **disabled**, validate via the manual endpoint on a handful of slugs, then enable a conservative interval (e.g. `6h`) once trusted (§7 D1).
2. **Manual internal trigger.** `POST /internal/imageingest/run`, admin-token gated, bound to the internal listener and mounted **outside `/v1`** so it is excluded from the public nginx vhost (defense in depth on top of the token — §5). `?slug=&name=` ingests one species synchronously (ideal for smoke-testing the slug↔R2 round-trip); no args runs a bounded batch and returns the summary. Long batches return 202 and continue in a goroutine bounded by `limit`; a single-slug call returns its `IngestOutcome` directly.

`RunBatch(ctx, limit)` flow:
```
1. seeds := SELECT scientific_name FROM plants_pending
            WHERE status IN ('pending','approved')           -- enrichment-owned, read-only
            -- (optionally LEFT JOIN plant_image_ingest to filter done rows in SQL)
2. n := 0
   for each seed.scientific_name (stop when n == limit):
     slug := Slug(scientific_name)                            -- §2.3, byte-exact iOS port
     if slug == "" { ledger upsert status=no_acceptable_image, note="empty slug"; continue }
     if ledger says slug already ingested OR no_acceptable_image (and not stale) { skip; continue }
     out := IngestOne(ctx, slug, scientific_name)
     ledger upsert(out)
     n++
     sleep(minInterval)                                       -- §4 Wikimedia etiquette
3. return BatchSummary{...}
```

`IngestOne(ctx, slug, searchTerm)` flow:
```
1. if HeadObject(plant_images/{slug}/hero.png) exists → return {Status: skipped_exists}
   (defensive double-check vs the ledger; R2 is the source of truth for "image present")
2. candidates := WikimediaSearch(searchTerm, limit=10)        -- §2.4 (UA + maxlag)
3. acceptable := filter(candidates, licenseAllowed)           -- §2.4 (CC0/PD/BY/BY-SA only)
   if empty → return {Status: no_acceptable_image}
4. pick := selectBest(acceptable)                             -- §2.5 (tier → photo-likeness/size)
5. bytes, mime := download(pick.ThumbURL)                     -- §2.6 1600px rendition (UA; size guard §4)
   if too large / wrong mime / download fail → next candidate, else {Status: source_error}
6. PutObject(plant_images/{slug}/hero.png, bytes, Content-Type=mime)
   if fail → return {Status: upload_error}
7. return {Status: ingested, R2Key, License, Author, FilePage, Mime, Bytes, Width, Height}
```

### 2.2 R2 layout

| | |
|---|---|
| Bucket | `yardmate-static` |
| Object key | `plant_images/{slug}/hero.png` (out-of-catalog species hero) |
| Public URL | `https://images.yardmate.ai/plant_images/{slug}/hero.png` |
| Content-Type | the **real** MIME of the stored bytes (`image/jpeg` \| `image/png` \| `image/webp`), NOT forced to `image/png` |
| In-catalog (NOT ours) | `plant_images/{AAA-id}/{1_whole,2_closeup,3_state,4_scene,a_history}.png` — separate offline tooling |

The `.png` in the key is a **logical name**, not a format assertion (Q2). iOS `CachedAsyncImage` decodes via `UIImage(data:)` by content, ignoring the extension; browsers/`URLSession` honor the stored `Content-Type`. Storing JPEG bytes under a `hero.png` key with `Content-Type: image/jpeg` is correct and avoids any re-encode (which would be an adaptation → CC-BY-SA ShareAlike — §7 D2).

### 2.3 `slug` — byte-exact Go port of iOS `PlantImageURL.slug` (THE central invariant)

iOS (`app/YardMate/YardMate/RemoteContent/PlantImageURL.swift`, shipped PR #227):
- lowercase the whole string;
- iterate characters; keep `[a-z0-9]`; any run of non-`[a-z0-9]` collapses to a **single** `-`; **no leading dash** (separators before the first kept char are dropped), **no trailing dash** (a pending separator at end is never flushed).
- `Rosa regina sueciae` → `rosa-regina-sueciae`.

Go port (single source of truth on the server side; **must produce the identical string for the identical input**):
```go
// Slug mirrors iOS PlantImageURL.slug byte-for-byte. Do NOT add unidecode /
// NFD / NFC / transliteration — iOS treats every non-[a-z0-9] code point as a
// separator (it is NOT in the allowed set), so "é" / "×" / spaces all collapse
// to a single "-". Transliterating "é"→"e" here would produce a DIFFERENT slug
// than iOS → a permanent 404 on the hero (pitfall §9 #1).
func Slug(scientificName string) string {
    out := make([]byte, 0, len(scientificName))
    pendingDash := false
    for _, r := range strings.ToLower(scientificName) {
        if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
            if pendingDash && len(out) > 0 {
                out = append(out, '-')
            }
            out = append(out, byte(r))
            pendingDash = false
        } else if len(out) > 0 {
            pendingDash = true // collapse separators; never flush at end
        }
    }
    return string(out)
}
```
`GenusSlug(name)` = `Slug(firstSpaceDelimitedToken(name))` (iOS `genusSlug`: split on `" "`, omit empty leading tokens, slug the first). Implemented for completeness + future genus fill, **not invoked** by V1 batch (Q3).

The invariant chain (must hold or the whole feature 404s): iOS sends scientific name `X` to `/v1/plants/enrichment` → enrichment stores `plants_pending.scientific_name = X` *verbatim* (enrichment SPEC §2.1: path-2/3 = "original un-normalized user input") → ingest reads `X`, computes `Slug(X)` → iOS renders the same plant and computes `Slug(X)`. Same function, same input ⇒ same key. **Neither side may pre-process the name before slugging** (pitfall §9 #2). Locked with a Go unit test mirroring iOS test vectors (§10).

### 2.4 Wikimedia search + license filter

Request (one HTTP GET, JSON):
```
GET https://commons.wikimedia.org/w/api.php
  ?action=query&format=json&maxlag=5
  &generator=search&gsrsearch=<scientificName>&gsrnamespace=6&gsrlimit=10
  &prop=imageinfo&iiprop=url|mime|size|extmetadata&iiurlwidth=1600
User-Agent: <IMAGEINGEST_USER_AGENT or "YardMate-ImageIngest/1.0 (https://yardmate.ai; contact@yardmate.ai)">
```
`gsrnamespace=6` = the File: namespace. `iiurlwidth=1600` makes Wikimedia render a **1600px scaled rendition** and return its `thumburl` (D2 resolved → scaled). Each returned page carries `imageinfo[0]` with `url` (the original — used ONLY for the size/photo heuristic via `width`/`height`), `thumburl` (the 1600px rendition we actually **download + store**), `thumbmime`, `mime`, `width`, `height`, and `extmetadata`.

**License classification** — read `extmetadata.License.value` (machine code, e.g. `cc-by-sa-4.0`, `cc0`, `cc-by-2.0`, `pd`) as primary; fall back to `extmetadata.LicenseShortName.value` when `License` is absent (older files). Rule (robust, token-based per Q-pitfall "含 nc/nd 子串一律排除"):
```
code := lower(extmetadata.License.value)        // hyphen-delimited tokens
tokens := split(code, "-")
if "nc" in tokens or "nd" in tokens             → REJECT (non-commercial / no-derivatives)
allow if  tokens[0..] indicate one of:
            cc0                                  → CC0       (no attribution)
            pd  / "public" in shortName          → PD        (no attribution)
            cc-by-sa-*                           → CC-BY-SA  (attribution + ShareAlike)
            cc-by-*  (and NOT nc/nd)             → CC-BY     (attribution)
else                                            → REJECT (ARR / GFDL-only / unknown → conservative)
```
Notes:
- Tokenizing on `-` (not raw substring) avoids false hits; `"public domain"` lowercased has no `nd`/`nc` token, `cc-by-sa-4.0` → `[cc,by,sa,4.0]` (allowed), `cc-by-nc-nd-4.0` → has `nc`/`nd` (rejected).
- Also reject when `extmetadata.Copyrighted.value == "True"` **and** no allowed CC/PD code resolved (catches "All rights reserved" stragglers — Commons is mostly free but PD-art / fair-use edge files exist).
- `GFDL-only` (no CC dual-license) → reject in V1 (copyleft + attribution but awkward; rare for photos — most are dual CC-BY-SA which we accept). §7 D3.
- **Conservative default:** if the license can't be positively classified into the 4 allowed families, SKIP the candidate. A placeholder is acceptable (memory: "占位是常态"); a license violation is not.

### 2.5 Candidate selection (which photo becomes the hero)

Rank acceptable candidates by:
1. **License tier** (least restrictive first, per Q-pitfall): `CC0 = PD > CC-BY > CC-BY-SA`. Fewer downstream attribution obligations.
2. **Photo-likeness / size** (tiebreak within a tier): prefer raster photos over diagrams/maps. Heuristics (best-effort, tunable §7 D4):
   - reject `image/svg+xml`, `image/gif`, `image/tiff` outright (not photos / not web-friendly);
   - deprioritize titles containing `map|range|distribution|herbarium|illustration|diagram|chart|locator`;
   - prefer larger pixel area (`width*height`) but cap absurd originals (§4 size guard).
3. First candidate passing download + MIME re-check wins; on download failure, fall through to the next acceptable candidate before declaring `source_error`.

"Photo-likeness" is heuristic, not guaranteed — V1 accepts an occasional non-ideal hero over a placeholder. Refinement (e.g. Commons category `incategory:` precision, ML photo-detection) is §8.

### 2.6 Upload

`PutObject(Bucket=yardmate-static, Key=plant_images/{slug}/hero.png, Body=bytes, ContentType=<real mime>)`. Bytes are the **downloaded bytes, verbatim** (§7 D2). No `ACL` param (R2 ignores S3 ACLs; public read is configured at the bucket/custom-domain level, already serving the AAA images). Set `CacheControl` to a long max-age (e.g. `public, max-age=31536000, immutable`) since a given slug's hero is effectively immutable once chosen (re-ingest only via explicit ledger reset — §6.1).

---

## 3. Error / outcome matrix

Per-species outcomes are **not HTTP errors** — they are `IngestOutcome.Status` values, logged + balanced into the ledger so a single bad species never aborts a batch:

| Status | Meaning | Ledger | Retry behavior |
|---|---|---|---|
| `ingested` | uploaded to R2 + attribution recorded | `status=ingested` | terminal (until manual reset) |
| `skipped_exists` | R2 already has the key (ledger/HEAD agree) | `status=ingested` (backfilled if missing) | terminal |
| `no_acceptable_image` | search returned nothing, or nothing CC0/PD/BY/BY-SA | `status=no_acceptable_image` | negative-cached; re-attempt only after `IMAGEINGEST_NOIMAGE_TTL` (default 30d) or manual reset |
| `source_error` | Wikimedia 5xx / network / all candidate downloads failed | `status=failed`, `attempts++` | retried next pass with backoff; cap `attempts` (default 5) → then treated as negative-cached |
| `upload_error` | R2 PutObject failed | `status=failed`, `attempts++` | retried next pass |

The **internal HTTP trigger** maps only auth/config problems to HTTP errors `{"error":"<code>"}`:

| Code | HTTP | Meaning |
|---|---|---|
| `missing_admin_token` / `bad_admin_token` | 401 | `X-Ingest-Admin-Token` absent / mismatched `IMAGEINGEST_ADMIN_TOKEN` |
| `ingest_disabled` | 503 | R2 or DB not configured (service nil) |
| `bad_request` | 400 | malformed `?limit` / `?slug` without `?name` |
| `internal` | 500 | unmapped |

Wikimedia 429 / `maxlag` are handled internally (back off + honor `Retry-After`, §4), never surfaced as HTTP errors.

---

## 4. Wikimedia rate-limit etiquette + size caps

This is the inverse of the proxy/enrichment rate limit (we are the *client* of Wikimedia, the polite party):

- **User-Agent is mandatory.** Anonymous requests without a descriptive UA get 403. Format: `YardMate-ImageIngest/1.0 (https://yardmate.ai; contact@yardmate.ai)` (override via `IMAGEINGEST_USER_AGENT`). Applied to BOTH the api.php query and the upload.wikimedia.org byte download.
- **Serial, paced.** No concurrency across species; `minInterval` (default `1s`, env `IMAGEINGEST_MIN_INTERVAL`) between Wikimedia HTTP calls. The bounded batch size keeps total load low.
- **`maxlag=5`** on api.php queries — Wikimedia replies 503 + `Retry-After` when replication lag exceeds 5s; honor it (good-citizen back-off).
- **`Retry-After` / 429 / 503:** exponential backoff honoring the header, capped (e.g. 3 retries, max 30s wait) per call; on exhaustion → `source_error` (retried next pass).
- **Download size guard:** cap the byte download (`IMAGEINGEST_MAX_BYTES`, default 25 MB) via a `LimitReader`. We download the 1600px rendition (~200–600 KB typical, D2), so this rarely fires — it's a backstop against an unexpectedly large thumb. This is *selection*, not transformation.
- **MIME re-check on downloaded bytes** (`http.DetectContentType` on first 512 B) — the `extmetadata.mime` is advisory; the stored `Content-Type` comes from the actual bytes. Reject if not `image/jpeg|png|webp`.

---

## 5. Security model

- **No public surface.** Neither the ticker nor the internal endpoint is reachable by iOS or the public internet. `/internal/...` is mounted outside the `/v1` group and is **not** added to the nginx public vhost (which proxies only `/v1/*` + `/healthz`). The server already binds `127.0.0.1:8080` behind nginx, so `/internal/*` is unreachable externally even before the token check.
- **Admin token.** `POST /internal/imageingest/run` requires `X-Ingest-Admin-Token == IMAGEINGEST_ADMIN_TOKEN` (constant-time compare). This is a **new** auth mechanism — yardmate-api today has no admin-token pattern (identify/diagnose only log App Attest headers; `/v1/app-secrets` uses the attest handshake). The route is registered only when the token secret is set. The token is **server-only**; it MUST NOT enter `main.vendedKeys` (pitfall §9 #7).
- **R2 credentials are write-scoped + server-only.** The R2 API token is scoped to *Object Read & Write* on `yardmate-static` only (no account-wide / no other buckets). Like the Supabase DSN, the R2 secret never leaves the server and is never vended, never logged.
- **Wikimedia input is low-risk.** We *read* public Commons data; the only injection surface is the search term (a scientific name) placed in a query param (properly URL-encoded). Downloaded bytes are treated as opaque image data (sniffed for MIME, never executed); we never parse Commons HTML beyond extracting `extmetadata` string fields (HTML-stripped for the `Author` field before storage).
- **Attribution integrity.** For CC-BY / CC-BY-SA we store author + license + file-page so the (future) Credits page satisfies CC §3a. CC0 / PD need no attribution but we still record provenance for audit.
- **DSN secrecy** inherits enrichment SPEC §9 #14 — `SUPABASE_DB_URL` carries the DB password; never log/echo it.

---

## 6. Schema

### 6.1 Supabase ledger `plant_image_ingest` (this package owns it)

Serves three jobs at once: **idempotency** (skip done slugs), **negative cache** (don't re-search species with no free image every pass), and **attribution ledger** (feed the future Credits page). DDL in `proxy/imageingest/migrations/001_plant_image_ingest.sql`.

```sql
CREATE TABLE plant_image_ingest (
  slug                TEXT PRIMARY KEY,            -- == Slug(scientific_name); == iOS R2 key segment
  scientific_name     TEXT NOT NULL,               -- the searched name (audit; not re-slugged on read)
  status              TEXT NOT NULL                -- 'ingested' | 'no_acceptable_image' | 'failed'
                        CHECK (status IN ('ingested','no_acceptable_image','failed')),
  r2_key              TEXT,                         -- 'plant_images/{slug}/hero.png' when ingested
  source              TEXT NOT NULL DEFAULT 'wikimedia_commons',
  source_file_page    TEXT,                         -- Commons File: page URL (re-derive attribution anytime)
  license_code        TEXT,                         -- machine, e.g. 'cc-by-sa-4.0' / 'cc0' / 'pd'
  license_short       TEXT,                         -- display, e.g. 'CC BY-SA 4.0'
  license_url         TEXT,
  attribution_author  TEXT,                         -- HTML-stripped Artist (NULL for CC0/PD)
  attribution_required BOOLEAN NOT NULL DEFAULT FALSE,
  mime                TEXT,
  bytes               BIGINT,
  width               INT,
  height              INT,
  attempts            INT NOT NULL DEFAULT 0,        -- failed-attempt counter for backoff/cap
  last_error          TEXT,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_plant_image_ingest_status ON plant_image_ingest (status);
```

- **PK = `slug`** (not scientific name): the slug is what both R2 and iOS key on, and distinct names that slug identically (rare) intentionally share one hero — consistent with how iOS would render them.
- Upsert: `INSERT ... ON CONFLICT (slug) DO UPDATE SET ...` (status/attribution/attempts/updated_at). Re-ingest a species = manually `DELETE` its row (and the R2 object) → next pass re-attempts. Mirrors enrichment's "delete-to-regenerate" Dashboard pattern.
- Negative cache: `no_acceptable_image` rows are skipped until `now() - updated_at > IMAGEINGEST_NOIMAGE_TTL` (default 30d) — Commons gains photos over time, so periodic re-check is worthwhile but not every pass.

### 6.2 Seed read (enrichment-owned `plants_pending`, read-only)

`SELECT scientific_name FROM plants_pending WHERE status IN ('pending','approved')`. This package **never** writes `plants_pending`. The coupling is the table+column name only (pitfall §9 #4). Optionally `LEFT JOIN plant_image_ingest ON slug = Slug(scientific_name)` — but `Slug` is a Go function, not SQL, so V1 filters done-rows in Go after the SELECT (simpler; the seed set is small at V1 scale). Pushing the slug into SQL (a Postgres function mirroring `Slug`) is a §8 optimization, explicitly NOT done in V1 to keep `Slug` single-sourced in Go.

---

## 7. Resolved decisions (don't re-debate)

**From the Q&A before SPEC drafting:**

- **D-seed (Q1): batch, seeded from `plants_pending`.** Not a per-request iOS-triggered ingest. iOS PR #227 has no trigger call and needs none — the enrichment table already records every out-of-catalog name iOS views. Backend self-drives (ticker + manual). Per-request on-demand (iOS asks the server to ingest a specific species when it sees a placeholder) is §8 / V1.1 and would require an iOS change; explicitly deferred.
- **D-genus (Q3): species slug only; genus is P2.** iOS reads only the species slug today; filling genus now = wasted Wikimedia/R2 work iOS won't read. The core is slug-parameterized so genus fill (`GenusSlug`) is a trivial future addition (feed genus slugs into the same `IngestOne`).
- **D-format (Q2 + D2): store the fetched bytes verbatim, real Content-Type, NEVER re-encode.** We download Wikimedia's **server-side 1600px scaled rendition** (`iiurlwidth=1600` → `imageinfo.thumburl`; D2 resolved → scaled) and store those bytes *as-is*. Re-encoding **by us** (even JPEG→PNG to match the `.png` key) would be an *adaptation* → CC-BY-SA ShareAlike; we never do it. Downloading Wikimedia's own downscale is "reproduction in another size", **not** an adaptation by us, so SA does not attach, attribution is unchanged, and the "不加工" intent (no transformation *by us*) holds. The key's `.png` is a logical name; iOS decodes by content. Allowed MIMEs: `image/jpeg|png|webp`.
- **D-license-conservative:** classify into CC0/PD/CC-BY/CC-BY-SA or SKIP. Token-based NC/ND rejection. Unknown/ARR/GFDL-only → skip. A placeholder beats a license violation.
- **D-auth (Q4): admin token + internal-only path.** New `IMAGEINGEST_ADMIN_TOKEN`; `/internal/...` excluded from public nginx + server binds localhost. Not under `/v1`, no per-IP/per-device middleware (those are for public client traffic).
- **D-ledger:** a `plant_image_ingest` table is required (not optional) — it is the idempotency record AND the negative cache AND the attribution store (B-档 compliance needs author/license captured at ingest). HEAD-R2-only can't negative-cache "no free image exists".

**Resolved at SPEC review (Yao, 2026-05-27):**

- **D2 (original vs scaled) → SCALED 1600px.** Download Wikimedia's server-side **1600px** scaled rendition (`iiurlwidth=1600` → `imageinfo.thumburl`), not the raw original. Rationale: Commons originals are 4000–6000 px / 5–20 MB while curated AAA heroes are 1200×900 / ~600 KB — a 15 MB phone hero is poor UX. A pure downscale generated *by Wikimedia* is "reproduction in another size", **not** an Adaptation under CC 4.0 §1(a), so CC-BY-SA ShareAlike does not attach, attribution is unchanged, and "不加工" (no transformation *by us*) still holds. **This consciously overrides the `v1_image_self_hosting` memory's "转存原图" line** — the memory was updated to record this correction. The §4 size guard stays as a backstop. (Implementation: store `thumburl` bytes verbatim; the heuristic in §2.5 still uses the original `width`/`height` as a source-quality signal.)
- **D5 (R2 client library) → aws-sdk-go-v2.** `github.com/aws/aws-sdk-go-v2/service/s3` + `config` + `credentials`, with `BaseEndpoint` = the R2 endpoint, `Region("auto")`, and `credentials.NewStaticCredentialsProvider(...)` (pitfall §9 #12). Cloudflare's documented R2 path; correct SigV4. The ~15 transitive modules are accepted — the codebase already tolerates third-party deps (pgx, chi, lru, cbor, bbolt).

---

## 8. Out-of-scope (V1.1+ candidates)

- **On-demand per-species ingest** (iOS/enrichment signals "fill this slug now" when a placeholder is shown). Needs an iOS change + a public/auth'd trigger; V1 batch-from-seed covers the common case (memory: common species are ~98% covered, cold species are the placeholder-risk tail).
- **Genus-level fallback fill** — turn on once iOS reads the genus slug (iOS P2). Core already parameterized.
- **iOS Settings → Credits page** + a `/credits` export endpoint built from `plant_image_ingest` attribution rows.
- **Additional sources** (iNaturalist CC0/CC-BY, USDA PD, GBIF) — ledger `source` column is ready; add a source-cascade.
- **Better candidate selection** — Commons `incategory:` precision, ML photo-vs-diagram detection, multiple variants per slug (closeup/scene like the AAA layout).
- **Scaled-rendition / WebP normalization** beyond D2-OPEN, if a perf pass wants uniform hero sizes.
- **SQL-side slug filter** (Postgres function mirroring `Slug`) to push done-row filtering into the seed query at larger scale.
- **Re-ingest automation** (refresh stale/low-quality heroes) beyond manual ledger delete.

---

## 9. Pitfalls (don't re-rediscover)

1. **`Slug` must be byte-exact to iOS — NO transliteration/normalization.** unidecode (`é`→`e`) or NFD/NFC would yield a different slug than iOS → permanent 404. iOS treats every non-`[a-z0-9]` code point as a separator; the Go port iterates runes and does the same. Caveat: a *decomposed* base-ASCII + combining-mark sequence (e.g. `e`+U+0301) diverges (Swift `Character` = 1 grapheme → separator; Go rune = `e` kept + mark separator) — not expected in romanized botanical Latin, but documented; do not "fix" it with normalization (that would break the common ASCII case's guarantee). Lock with a unit test mirroring iOS vectors.
2. **Neither side may pre-process the name before slugging.** The invariant holds only because iOS slugs the same string it sent to enrichment, and enrichment stores it verbatim. If iOS ever strips author citations / trims differently before slugging, or enrichment stores a normalized form, the keys diverge. Verify against iOS `PlantDetailViewModel.composeHeroImages` (confirmed PR #227: it slugs the VM `scientificName`, which is the enrichment input).
3. **`.png` key ≠ PNG bytes.** Store JPEG/WebP bytes under `hero.png` with the real `Content-Type`. Do not force-encode to PNG (D-format / D2).
4. **`plants_pending` is enrichment-owned; read-only here.** Don't import the `enrichment` Go package and don't write the table. If enrichment renames the table/column, the seed query breaks — pin the column name in one place + a smoke check. (Sanctioned data coupling per Q1.)
5. **Negative cache or you hammer Wikimedia.** Without the `no_acceptable_image` ledger state, every pass re-searches every species that has no free image. Always upsert the ledger, even on "no image".
6. **R2 creds / admin token are server-only — keep them OUT of `vendedKeys`.** `secrets/SPEC.md` §4.1: anything in `vendedKeys` is handed to every authenticated client. The R2 write token + admin token there would be a credential leak.
7. **Constant-time admin-token compare** (`subtle.ConstantTimeCompare`) — a naive `==` on the token is a timing oracle. Low-stakes (internal-only) but trivial to do right.
8. **HEAD before PUT for idempotency, but R2 is truth, not the ledger.** A ledger row can exist while the R2 object was deleted (or vice versa). `skipped_exists` is decided by `HeadObject`, then the ledger is reconciled. Don't trust the ledger alone to mean "the image is live".
9. **Wikimedia UA is mandatory** — missing/empty UA → 403, not 429. Don't debug it as rate-limiting.
10. **Don't log image bytes or full Commons JSON at INFO.** Log `slug`, `scientific_name`, `status`, `license_code`, `bytes`, latency. Never the DSN / R2 secret / admin token.
11. **Ticker must not pile up** — if a pass outruns the interval (slow Wikimedia), guard with a single-flight mutex so two passes don't run concurrently (double Wikimedia load + ledger races).
12. **`config.LoadDefaultConfig` for aws-sdk-go-v2 reads env/`~/.aws` by default** — pass explicit static credentials (`credentials.NewStaticCredentialsProvider`) + `BaseEndpoint` for R2 so it never accidentally picks up ambient AWS creds on the box.

---

## 10. Implementation outline (not part of the contract)

```
proxy/imageingest/
├── SPEC.md                       (this file)
├── slug.go                       Slug + GenusSlug (byte-exact iOS port)
├── slug_test.go                  iOS-mirrored vectors (incl. ×, accents, leading/trailing/multi-sep, empty)
├── license.go                    extmetadata → {allowed bool, code, short, url, author, attributionRequired}
├── license_test.go               table: cc0/pd/cc-by/cc-by-sa allow; nc/nd/arr/gfdl/unknown reject
├── commons.go                    Wikimedia search + imageinfo (UA, maxlag, Retry-After backoff, size-cap download)
├── commons_test.go               httptest-served fixtures (search hit / 403-no-UA / 429-retry / no results)
├── r2.go                         S3 client wrapper: HeadObject + PutObject (R2 endpoint, static creds, region=auto)
├── r2_test.go                    against a stub S3 (or interface + mock)
├── ledger.go                     pgx: Lookup(slug) + Upsert(outcome) against plant_image_ingest
├── ledger_test.go                hermetic
├── seed.go                       pgx: SELECT scientific_name FROM plants_pending (read-only)
├── ingestor.go                   Ingestor.IngestOne + RunBatch + selectBest + single-flight ticker
├── ingestor_test.go              table-driven: end-to-end with commons/r2/ledger mocks; all Status values
├── handlers.go                   POST /internal/imageingest/run (admin-token, ?slug/?name/?limit)
├── handlers_test.go              auth (401), disabled (503), single-slug + batch
└── migrations/
    └── 001_plant_image_ingest.sql
```

`main.go` wiring (mirrors `buildEnrichmentService`):
```go
ingestSvc := buildImageIngestService(vault, /* shares SUPABASE_DB_URL */) // nil + WARN if R2/DB/token missing
// in newServer: register /internal/imageingest/run only if ingestSvc != nil (outside the /v1 group)
// start ticker only if ingestSvc != nil && IMAGEINGEST_TICK_INTERVAL > 0
```

`secrets.env` additions (local `~/.config/yardmate-api/secrets.env.prod` → deploy.sh ships; `secrets.env.example` updated):
```
R2_ACCOUNT_ID=...
R2_ACCESS_KEY_ID=...
R2_SECRET_ACCESS_KEY=...
R2_BUCKET=yardmate-static
# R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com   (optional; derived from R2_ACCOUNT_ID if unset)
IMAGEINGEST_ADMIN_TOKEN=...        # internal manual-trigger gate; server-only, NOT vended
# IMAGEINGEST_TICK_INTERVAL=0      # 0/unset = ticker off (manual-only); e.g. 6h to enable
# IMAGEINGEST_BATCH_LIMIT=25
```
`SUPABASE_DB_URL` already present (enrichment). **R2 creds + `IMAGEINGEST_ADMIN_TOKEN` must NOT be added to `main.vendedKeys`** (pitfall §6/§9 #6). Deploy via the standard `YARDMATE_DEPLOY_STAGE=dev ./deploy/deploy.sh` flow (App-Attest dev-gate caveat per the `yardmate_api_deploy` memory).

Estimated effort: ~2 day implementation (slug/license/commons/r2/ledger/ingestor) + ~0.5 day tests + ~0.5 day deploy + smoke (single-slug via the internal endpoint, then enable ticker) ≈ 3 days.
