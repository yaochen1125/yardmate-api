# `proxy/imageingest` package — plant image ingest (V1 image self-hosting)

> Status: **v2 — on-demand cascade pivot (2026-05-28); supersedes v1 batch-from-seed**. v1 (Wikimedia-only / batch-from-`plants_pending` / single `hero.png`) shipped (PR #22 SPEC + #23 impl), was validated on a single slug (`Monstera adansonii`) and withdrawn before opening to traffic — see PIVOT memory `v1_image_self_hosting`. v2 keeps the R2-ownership + B-档 license + `credits.json` invariants, replaces the trigger model + source cascade + R2 layout + ledger schema, and requires iOS-side changes (companion SPEC `yardmate-swiftui/docs/releases/v1/shared/plant-images/plant-images.md`).
> **Hero pass-through REVIVED for slot 1 (2026-05-29 realign, §7 D-hero-passthrough — supersedes the 2026-05-28 D-cascade-only drop).** The 2026-05-28 draft dropped pass-through because the only candidate source was identify's **PlantNet-host** `image_url` (TOS-blocked + no `license_code`). The realign fixes the premise from a different angle: the hero source is **iNaturalist, not PlantNet** — iOS Search already shows an iNat photo, and iOS will capture its `license_code` + `attribution` (the §8 "minimal unlock"). So there now IS a usable hero. iOS forwards an OPTIONAL `hero_image` carrying the **iNat photo id** it displayed; the backend matches that id **within its own §2.4 cascade results** (iNat-API-authoritative license/attribution) and stores THAT candidate as slot 1 — guaranteeing "search image == detail hero == stored R2 image" (no visual jump when the hero switches from the transient iNat URL to R2). Slots `2..N` still cascade. **The backend never fetches a client-supplied URL** (it downloads the matched candidate from the iNat-API URL), so `hero_image` adds no SSRF surface and the client cannot lie about license/attribution (§2.1.1, §5, §9 #15–16). PlantNet remains ineligible (TOS-blocked + not iNat). Cascade-only remains the behavior when `hero_image` is absent (cold-start without a forwarded id, or the internal endpoint).
> Companion: parent `proxy/SPEC.md` (`/v1/identify`, `/v1/diagnose`) + `proxy/enrichment/SPEC.md` (`/v1/plants/enrichment`). This package is a **new, independent domain**. It does NOT hang off enrichment: enrichment SPEC §1.2 explicitly states "**Image storage. No R2 writes; text in / JSON out.**" — that boundary stays. This package is the *only* writer of plant imagery to R2 from the server.
> Background: out-of-catalog plants (identification results / species outside the curated 1522) render a **gallery of images** from R2 at `plant_images/{slug}/{i}.png` (i ∈ 1..N, default N=4), where `slug` is derived from the scientific name (iOS `PlantImageURL.slug`, shipped PR #227, **trinomial — never folded to binomial** per PIVOT). iOS triggers ingest on-demand when an out-of-catalog detail page mounts: it POSTs the scientific name and the backend walks a multi-source cascade (iNat → Wikimedia, → GBIF / USDA P2) to fill the slots. Until ingest fills the keys, iOS shows a placeholder.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- **On-demand species ingest (HTTP-triggered).** Public `POST /v1/plants/imageingest` (App Attest gated, same envelope as `/v1/identify`). Request: `{scientific_name, image_count?, hero_image?}`. iOS fires it when an out-of-catalog detail page mounts (companion SPEC §3); the backend cascades to fill `image_count` (default 4) gallery slots.
- **Hero pass-through — slot 1 = the image iOS already showed (§2.1.1, D-hero-passthrough).** When the request carries an optional `hero_image` whose **`photo_id`** is the iNat photo the user saw (Search thumbnail / cold-start live-fetch), the backend finds that photo **within its own §2.4 cascade results** and stores THAT candidate as slot 1 (instead of the cascade's own slot-1 pick). This makes "search image == detail hero == stored R2 image" (no visual jump). The backend **never fetches a client URL** — it downloads the matched candidate from the iNat-API URL, with iNat-authoritative license/attribution; the request's `url`/`license_code`/`attribution` are advisory (iOS display only). The matched candidate obeys the same gate (CC0/PD now, BY/SA `deferred_attribution` while OFF, NC/ND/ARR never eligible). No match, or `hero_image` absent → slot 1 cascades like the rest.
- **Multi-source candidate cascade (§2.4):** query sources in order — iNaturalist (CC0/CC-BY taxa default photo + observations) → Wikimedia Commons (license-filtered). Each candidate classified into CC0/PD/CC-BY/CC-BY-SA (reject NC/ND/ARR — §2.4); BY/SA gated behind `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES` until the iOS Credits page lives (§7 D-attribution-gate). V2 implements iNat + Wikimedia; GBIF / USDA = §8.
- **Multi-image gallery upload:** pick top-N candidates (source tier → photo-likeness, license-blind — §2.5), download each at a source-generated downscale (~1024–1600px, §7 D-format), upload to R2 keys `plant_images/{slug}/{1..N}.png` with the real Content-Type (§2.6). Within-gallery dedup prevents the same photo populating multiple slots.
- **Idempotency + per-image ledger (§6.1):** before each candidate download, HEAD-check the target R2 key (R2 = truth) + consult the `plant_image_files` ledger row keyed `(slug, image_index)`. Skip already-uploaded slots; partial-fail (e.g. slots 1/2 succeed, 3/4 fail) is normal — fail-soft, retry failed slots on next trigger.
- **Internal admin endpoint (ops only, retained from v1):** `POST /internal/imageingest/run?slug=&name=&image_count=` for single-slug re-ingest, or `?names=Name+one,Name+two` for batch reseed during incidents (e.g. R2 bucket restore). Batch is keyed by scientific NAME (the cascade needs a name to search; the slug is derived) — not by slug. Same admin-token gate as v1; internal-only (mounted outside `/v1`, behind nginx). NOT used by iOS, NOT a public route. Background ticker removed in v2.
- **Attribution capture + `credits.json` export (§2.7):** persist author / license / source-file-page per ingested image in the ledger; regenerate `plant_images/credits.json` on R2 after each ingest. One entry per **(slug, image_index)** so multi-image gallery attribution is row-distinct. This CDN file is the data source the iOS Settings → Credits page reads (CC §3a2 collected attribution).
- **License-compliance gate (Codex #22, retained verbatim from v1):** CC-BY / CC-BY-SA upload requires `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES=true`. Default OFF until the iOS Credits page is live. While OFF, only CC0 / PD pass; a (slug, image_index) whose only acceptable candidates are BY / SA records `deferred_attribution` (§3, §9 #13).

### 1.2 What this package is NOT responsible for

- **The enrichment endpoint / its logic.** v2 **does not read `plants_pending`** (v1 did; v2 deprecates that seed). enrichment + imageingest are now fully decoupled at the data layer — only the `/v1` HTTP prefix is shared. No imports of the `enrichment` Go package.
- **Identification / diagnosis / detail-text generation.** No image *upload* endpoint (that's `/v1/identify` / `/v1/diagnose`); no LLM. `/v1/plants/imageingest` is request-driven from iOS — it does not itself call identify / diagnose / enrichment, and it does NOT consume **identify/diagnose** image URLs (PlantNet-host / license-less). The only thing it takes from the client about the hero is a **`photo_id`** (§2.1.1) — used to MATCH within its own cascade results, not to fetch. It never sources images from identify/diagnose.
- **In-catalog (1522) curated/generated imagery.** The four semantic images `plant_images/{AAA-id}/{1_whole|2_closeup|3_state|4_scene}.png` are uploaded by separate offline tooling (the `scripts/` pipeline in `yardmate-swiftui`); this package never touches them. **NEW exception — the `external/` subfolder (§2.8):** this package DOES now fill `plant_images/{AAA-id}/external/{i}.png` with third-party *supplementary* gallery images for in-catalog plants — a physically disjoint subfolder, so the offline-generated semantic images are never collided with or overwritten. The slug-keyed out-of-catalog gallery (§2.1) remains the primary path; the curated semantic images remain the `scripts/` pipeline's exclusive domain.
- **Image transformation.** No re-encode, no crop, no resize-by-us, no AI enhancement. Bytes stored verbatim (§7 D-format). The one server-side decode is a read-only sniff for MIME / dimensions; the stored bytes are the downloaded bytes.
- **Genus-level fallback fill.** v2 fills the **species** slug only (`PlantImageURL.slug`), because iOS reads only the species slug today (genus fallback is iOS P2). Core is slug-parameterized, so genus fill (`PlantImageURL.genusSlug`) reuses the same code once iOS reads it — §8.
- **The iOS Credits *page* (UI).** This package generates `credits.json` (§2.7) but does not render it. The Settings → Credits *page* is iOS-side (companion SPEC §6, separate `yardmate-swiftui` PR) and is the compliance prerequisite for flipping the BY/SA gate ON (§7 D-attribution-gate, §8).
- **Promotion of out-of-catalog plants into the curated catalog**, or any change to `plants_detail.json`.
- **GBIF / USDA sources.** v2 implements **iNaturalist + Wikimedia Commons** only. GBIF (`api.gbif.org/v1/species/match` → `mediaSpecies`) and USDA Plants (no public API; HTML scrape) are §8 candidates; the ledger `source` column accommodates them.
- **Background ticker / batch-from-seed.** Both removed in v2. The internal admin endpoint is retained for ops only.
- **Fetching ANY client-supplied image URL.** The backend never does this. Hero pass-through forwards a `photo_id`, not a URL-to-fetch (§2.1.1): the backend downloads only from URLs in its OWN iNat/Wikimedia API responses. `hero_image.url` (if sent) is advisory for iOS display and is ignored by the backend. So there is no client-controlled fetch target — no SSRF surface, no host allowlist needed (§5).

### 1.3 Inputs

| Layer | Input |
|---|---|
| Public HTTP `POST /v1/plants/imageingest` | App Attest envelope (header `X-App-Attest-*`, see `proxy/SPEC.md` §4 — same as `/v1/identify`). JSON body: `{"scientific_name": "Monstera adansonii", "image_count": 4?, "hero_image": {"photo_id": "123", "url"?, "license_code"?, "attribution"?}?}`. `image_count` optional (default 4, clamp 1–6). `hero_image` optional — only `photo_id` is acted on (slot-1 match within the cascade, §2.1.1); `url`/`license_code`/`attribution` are advisory (iOS display) and ignored by the backend. Returns 202 immediately (fire-and-forget). **(NEW — `publicRequest` must add `hero_image` parsing; shipped struct is name+count only.)** |
| Internal HTTP `POST /internal/imageingest/run` | header `X-Ingest-Admin-Token: <token>` (matched against secret `IMAGEINGEST_ADMIN_TOKEN`, constant-time). Query: `?slug=<slug>&name=<scientificName>&image_count=<N>` (single, synchronous) or `?names=<sciName1,sciName2>&image_count=<N>` (batch reseed by scientific name, ops only). NOT a public route — internal-only, behind nginx. |
| `Ingestor.IngestSpecies(ctx, req)` | `req = {Slug, ScientificName, ImageCount int, HeroPhotoID string}` (**NEW field** — shipped struct is `{Slug, ScientificName, ImageCount}`). `HeroPhotoID==""` ⇒ no pass-through, cascade-only. The backend uses only this id (matched against cascade `DedupKey "inat-photo-"+id`); advisory client url/license/attribution are not threaded into the ingestor. Returns `IngestOutcome{Slug, PerImage []ImageOutcome}`. |
| Server config (`secrets.Vault`) | `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` (`yardmate-static`), optional `R2_ENDPOINT` (default `https://<account>.r2.cloudflarestorage.com`), `IMAGEINGEST_ADMIN_TOKEN`, `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES`, optional `IMAGEINGEST_USER_AGENT`, optional `INATURALIST_USER_AGENT`, optional `IMAGEINGEST_PUBLIC_RATE` (default 100). Supabase DSN `SUPABASE_DB_URL` (shared with enrichment; **only for this package's own ledger tables now, not `plants_pending`**). |

### 1.4 Outputs

| Function | Output | Error / outcome cases |
|---|---|---|
| `Ingestor.IngestSpecies(ctx, req)` | `IngestOutcome{Slug, PerImage []ImageOutcome}` where each `ImageOutcome = {Index, Status, R2Key, Source, License, Author, FilePage, Mime, Bytes, Width, Height}` and `Status ∈ {ingested, skipped_exists, no_acceptable_image, deferred_attribution, source_error, upload_error}`. Partial fill is normal: a 4-image gallery can record 2× `ingested` + 1× `deferred_attribution` + 1× `source_error`. |
| Public HTTP `POST /v1/plants/imageingest` | 202 JSON `{"accepted": true, "slug": "...", "image_count_requested": 4}`. Fire-and-forget — actual work runs in a goroutine bounded by single-flight per slug (§4.2). Single 202 response only; **no progress endpoint in v2** — iOS retries naturally on next page mount; idempotency handles dedup. |
| Internal HTTP `POST /internal/imageingest/run` | 200 JSON `IngestOutcome` (single-slug) or `[]IngestOutcome` (batch). `{"error":"<code>"}` on auth / config errors (§3). |
| R2 side effect | objects `plant_images/{slug}/{i}.png` (verbatim bytes, real Content-Type) created for each `Status==ingested` image_index. |
| Supabase side effect | upserted rows in `plant_image_files` ledger (§6.1) per attempted `(slug, image_index)`. One `plant_image_species` parent row tracks species-level aggregate (count filled, last trigger). |

### 1.5 External dependencies

- **Cloudflare R2** (S3-compatible) — bucket `yardmate-static`, public custom domain `images.yardmate.ai`. Accessed via the AWS S3 Go SDK v2 (`github.com/aws/aws-sdk-go-v2/service/s3` + `config` + `credentials`) pointed at the R2 endpoint with region `auto`. Operations used: `HeadObject` (idempotency double-check) + `PutObject` (upload). Credentials are an R2 **API token** scoped to *Object Read & Write* on `yardmate-static` only.
- **iNaturalist API (v2 primary source)** — `https://api.inaturalist.org/v1/taxa?q=<name>&rank=species` returns matched taxa with `default_photo: {medium_url, large_url?, license_code, attribution}` + `id`; `https://api.inaturalist.org/v1/observations?taxon_id=<id>&photo_license=cc0,cc-by,cc-by-sa&per_page=12&order_by=votes` for additional CC0 / CC-BY / CC-BY-SA observation photos when the default photo is missing or non-free. stdlib `net/http`. Hard requirement: descriptive UA (`YardMate-ImageIngest/2.0 (https://yardmate.ai; contact@yardmate.ai)`). iNat per-IP cap is 100 req/min, 10000/day — well-bounded by our per-trigger usage (~3 calls).
- **Wikimedia Commons API (cascade fallback)** — `commons.wikimedia.org/w/api.php` for search + license metadata, `upload.wikimedia.org` for bytes. Same UA + `maxlag=5` + `Retry-After` etiquette as v1 (§4.1). Reached only when iNat returned `< N` eligible candidates.
- **Supabase Postgres** — `plant_image_species` + `plant_image_files` (this package's own; §6.1). **No more `plants_pending` read.** Own `pgx` pool, small (`MaxConns=2`; per-request worker, not a hot path). DSN from `SUPABASE_DB_URL` (shared with enrichment). Does NOT import enrichment package.
- **`secrets.Vault`** — config loader (already exists). R2 creds + admin token + attribution-gate flag are **server-only**; they MUST NOT be added to `main.vendedKeys` (`secrets/SPEC.md` §4.1). Pitfall §9 #6.
- stdlib only otherwise (`net/http`, `encoding/json`, `context`, `time`, `strings`, `image` for dimension sniff, `crypto/subtle`). Third-party limited to pgx (shared) + aws-sdk-go-v2.

---

## 2. Contract

### 2.1 Invocation model (v2 on-demand cascade)

Two entry points wrapping the same `Ingestor` core:

1. **Public HTTP `POST /v1/plants/imageingest`** (the only client surface; App Attest gated like `/v1/identify`).
   ```
   POST /v1/plants/imageingest
   X-App-Attest-...: <envelope>
   Content-Type: application/json

   {
     "scientific_name": "Monstera adansonii",
     "image_count": 4,                                 // OPTIONAL: default 4, clamp 1–6
     "hero_image": {                                   // OPTIONAL: slot-1 pass-through (§2.1.1)
       "photo_id": "12345",                            // the ONLY field the backend acts on
       "url": "...", "license_code": "...", "attribution": "..."  // advisory (iOS display); backend ignores
     }
   }
   ```
   - If `hero_image.photo_id` is present, the backend tries to MATCH it within its own §2.4 cascade results and store THAT candidate as slot 1 (§2.1.1) — it never fetches a client URL. No match → slot 1 cascades like the rest.
   - Backend walks the §2.4 cascade (iNat → Wikimedia) to pick top-N candidates for all slots (slot 1 = the matched hero candidate when present, else the cascade's own pick).
   - Returns 202 `{accepted: true, slug, image_count_requested}` synchronously. Actual work continues in a background goroutine bounded by single-flight per slug (§4.2). Concurrent duplicate requests for the same slug coalesce.

2. **Internal `POST /internal/imageingest/run`** (admin-token gated, internal-only, ops). For single-slug re-ingest / batch reseed during incidents (e.g. R2 bucket restore). `?slug=&name=&image_count=` for single (returns one `IngestOutcome`); `?names=sciName1,sciName2&image_count=` for batch (returns `[]IngestOutcome`; keyed by scientific name since the cascade searches by name). Synchronous. Mounted outside `/v1` (excluded from public nginx vhost).

`IngestSpecies(ctx, req)` flow:
```
1. slug := Slug(req.ScientificName)                    -- §2.3, byte-exact iOS port
   if slug == "" → return {PerImage: nil}              -- invalid input (e.g. all-whitespace name)
2. acquireSingleFlight(slug)                            -- §4.2 dedup; returns "coalesced" if another goroutine owns it
3. upsert plant_image_species(slug, scientific_name, last_triggered_at=NOW)
4. N := clamp(req.ImageCount, 1, 6)  [default 4]
   -- PASS 1: decide which slots need filling using only the ledger + an R2 HEAD
   -- (NO source calls). A fully-filled gallery makes zero outbound calls here.
   plan := []
   for i := 1..N:
     existing := lookup plant_image_files(slug, i)
     if existing has NO R2 object (no_acceptable_image | deferred-while-gate-off)
        → record skip; continue                                       -- HEAD would always miss; skip it
     if HeadObject(plant_images/{slug}/{i}.png) → record skipped_exists (PRESERVE prior attribution); continue
        -- R2 = truth (§9 #8): an `ingested` ledger row is HEAD-verified here, so a
        -- deleted / restored-away object falls into `plan` and re-fills (self-heal),
        -- never reports a phantom skipped_exists for an absent object.
     plan.add(i)                                                      -- object absent → needs fill
   -- PASS 2: only when plan is non-empty do we touch the sources.
   if plan not empty:
     eligible, gated := gatherCandidates(req.ScientificName, len(plan))  -- §2.4 (iNat → Wikimedia; dedup on DedupKey; over-provision to len(plan)*2)
                                                                          -- (window already top-12 obs by votes in shipped inat.go = superset of iOS top-1; no-jump needs iOS to pin the SAME query — §2.1.1)
     -- Slot-1 hero pass-through (§2.1.1, Option Y): if slot 1 ∈ plan AND
     -- req.HeroPhotoID != "", look for the candidate with
     -- DedupKey == "inat-photo-"+HeroPhotoID among `eligible` (authoritative,
     -- already license-classified). If found → make it slot 1's candidate and
     -- remove it from the pool so slots 2..N don't repeat it (§2.5 #4). If only
     -- present among `gated` (BY/SA, gate off) → slot 1 = deferred. If absent
     -- from both → no pass-through, slot 1 takes the cascade's own top pick.
     -- The backend NEVER fetches HeroImage.url (it isn't even passed in).
     for i in plan:
       candidate := (i==1 && heroMatch != nil) ? heroMatch : nextEligible(eligible)  -- §2.5
       if download+upload succeeds → upsert plant_image_files(status=ingested, source, license, author, source_url, ...); continue
       if eligible pool was depleted by download failures → record source_error (retryable, NOT terminal §9 #13)
       else if gated BY/SA available (or i==1 heroMatch was gated) → record deferred_attribution(store pending_url=candidate.DownloadURL)
       else → record no_acceptable_image
       sleep(minInterval) between source calls                        -- §4.1 source etiquette
     -- (NEW code vs shipped: `gatherCandidates` gains the no-jump window; the
     --  i==1 hero-match branch + DedupKey lookup are added in ingestor.go.)
5. recompute plant_image_species.image_count_filled                   -- COUNT(status=ingested)
6. regenerate plant_images/credits.json (§2.7)                        -- full rebuild
7. releaseSingleFlight(slug)
8. return IngestOutcome{Slug, PerImage}
```

### 2.1.1 Hero pass-through (slot 1) — `hero_image` (photo_id match; the backend never fetches a client URL)

**Why.** iOS shows an iNat photo as the Search-list thumbnail (and, on a cold-start detail open, fetches one live from iNat). When the user opens the detail page the hero must be **that same image, immediately**: iOS hot-link-displays the iNat photo right away (transient, with an inline attribution caption — companion SPEC), and we want the eventually-*stored* R2 slot 1 to be the SAME photo, so the hero does **not visually jump** when it switches from the transient URL to R2. Invariant target: **search image == detail hero == R2 slot 1**.

**Mechanism (why photo_id, not "fetch the forwarded URL").** iOS forwards the **iNaturalist photo id** it displayed; the backend does **NOT** fetch any client-supplied URL. It instead identifies the matching photo **inside its own §2.4 cascade results** — which carry iNat-API-authoritative license + attribution + rendition URL — and stores THAT as slot 1, downloading from the iNat-API URL. This is the deliberate design choice (§7 D-hero-passthrough); its consequences are the whole point:
- The backend **never fetches a client-supplied URL** ⇒ `hero_image` adds **no SSRF surface** (no host allowlist / redirect / IP-pin needed — §5).
- `license`, `attribution`, `source_url`, and the download rendition are all **iNat-API-authoritative**, never trusted from the client ⇒ a malicious attested client cannot relabel an NC/ARR photo as CC0 to get it stored to the public bucket, nor poison `credits.json` with attacker-controlled attribution text (§9 #15–16).
- `hero_image.url` / `license_code` / `attribution` are **advisory only** (iOS uses them for its own transient display); the backend acts **only on `photo_id`**.

**Request shape.** `hero_image: { "photo_id": <iNat photo id, required>, "url"?, "license_code"?, "attribution"? }`. `photo_id` is the integer in `inaturalist-open-data.s3.amazonaws.com/photos/<id>/...`. Optional fields are advisory; the backend reads only `photo_id`.

**Slot-1 handling (in IngestSpecies, after the §2.4 cascade gather).** When `hero_image.photo_id` is present AND slot 1 ∈ `plan` (slot 1 has no R2 object yet — see flow): find the gathered candidate whose iNat photo id == `photo_id`. Match it by the cascade DedupKey **`"inat-photo-" + photo_id`** — but the key must be **byte-identical** to what `sources.INatClient` builds, which is `"inat-photo-" + strconv.FormatInt(photo.ID, 10)` over an **int64** (inat.go). So the handler MUST canonicalize the incoming `photo_id` through int64 before forming the key: parse it as a base-10 integer (accept the JSON value as string or number; reject / ignore non-numeric, leading-zero, signed, or whitespace-padded values → treated as "no hero"), then `"inat-photo-" + strconv.FormatInt(n, 10)`. A raw string concat of an un-normalized client value risks a silent non-match (→ no pass-through → the 74% jump). This is the same byte-exactness discipline as the §2.3 Slug invariant.
- **Match + eligible** (CC0/PD now, or BY/SA after the gate): store it as slot 1 using its **authoritative** license/attribution/source_url; **exclude it from the pool for slots 2..N**.
- **Match but BY/SA while gate OFF**: record slot 1 `deferred_attribution` (store its authoritative rendition URL as `pending_url`); R2 slot 1 stays empty (iOS keeps displaying the transient URL).
- **Match but NC/ARR**: §2.4.3 already dropped it, so it's absent from BOTH `eligible` and `gated` ⇒ no match ⇒ no pass-through (and iOS should never have displayed it — §5 compliance note).
- **No match** (the displayed photo isn't in the cascade pool — rare): no pass-through; slot 1 falls to the cascade's own top pick. **This is the only "visual jump" case**, bounded by the no-jump rule below.
- **`hero_image` absent** (cold-start with no forwarded id, internal admin endpoint): cascade-only, exactly as v2. Pass-through is a pure optimization over the cascade, never a hard dependency.
- Slot 1 already has an R2 object ⇒ slot 1 ∉ `plan` ⇒ pass-through is **skipped, not an overwrite** (R2 immutable, §2.2). A slot 1 parked `deferred_attribution` / `no_acceptable_image` while gated stays short-circuited (§2.1 PASS 1) — the forwarded hero is not re-evaluated until that row clears.
- Bytes stored **verbatim**, real Content-Type (D-format) — the matched candidate is an ordinary cascade candidate, downloaded + stored like any other.

**No-jump rule (THE contract for "search == hero").** A jump occurs only when iOS's displayed photo is not in the backend's candidate pool. Measured reality (30-species sampling): the iNat `default_photo` is **NC/ARR ~74% of the time**, so for most species the displayed hero is a **free observation photo**, not the default. To guarantee the match, **iOS and the backend MUST select from the identical iNat free-photo set**:
- Same query: iNat observations with `photo_license=cc0,cc-by,cc-by-sa`, `order_by=votes&order=desc` (plus the taxon `default_photo` only when it is itself free).
- iOS displays + forwards the **top** such photo; the backend gathers a **superset window** (≥ top-12 by votes), so iOS's top-1 is always within the backend's window ⇒ matched ⇒ no jump.
- **The BY/SA gate is now ON** (`IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES=true`, after the iOS Credits page went live), so the *storable-now* set is **CC0 / CC-BY / CC-BY-SA** and SA joins the pinned free set on **both** sides: iOS and the backend select from the identical `cc0,cc-by,cc-by-sa` window, so an SA top photo is in the backend pool ⇒ ingested ⇒ no jump. *Historically, while the gate was OFF, both sides pinned only cc0,cc-by; forwarding an SA photo then deferred → R2 slot 1 empty → placeholder jump.* (Companion SPEC owns the iOS filter.)
- Companion SPEC pins iOS to this exact selection. Divergent query params (different order / license filter / window) reintroduce jumps. This is the dominant path (74%), not an edge case.

**Hero is a pointer, not a fixed key (companion SPEC).** Slot 1 is the *default* hero. iOS treats "which image is the hero" as a selectable pointer (default = gallery slot 1) so a future feature can let the user pick a different gallery image — or one of their own diary photos — as the hero, without changing this backend contract.

### 2.2 R2 layout (v2 multi-image)

| | |
|---|---|
| Bucket | `yardmate-static` |
| Object keys (v2 out-of-catalog) | `plant_images/{slug}/{i}.png` where `i ∈ 1..N` (default N=4, clamp 1–6) |
| Public URLs | `https://images.yardmate.ai/plant_images/{slug}/{i}.png` |
| Index semantics | `1` = primary (iOS detail page hero); `2..N` = gallery slots (carousel). **No "whole / closeup / state / scene" naming for out-of-catalog** — sources can't reliably tag photo intent (§7 D-multi-image-naming). |
| Content-Type | the **real** MIME of the stored bytes (`image/jpeg` \| `image/png` \| `image/webp`), NOT forced to `image/png` |
| CacheControl | `public, max-age=31536000, immutable` — a given `(slug, image_index)` is effectively immutable once chosen (re-ingest only via explicit ledger delete — §6.1) |
| In-catalog (NOT ours) | `plant_images/{AAA-id}/{1_whole, 2_closeup, 3_state, 4_scene, a_history}.png` — separate offline tooling. **Layout intentionally diverges** between in-catalog (semantic naming, curated by humans) and out-of-catalog (positional naming, sourced from APIs without photo-intent metadata). iOS handles both via separate URL builders (companion SPEC §5.1). |
| Legacy v1 key | `plant_images/{slug}/hero.png` — only `monstera-adansonii` + `monstera-adansonii-blanchetii` populated in v1 smoke; v2 ingest writes only the new `{i}.png` keys. The legacy keys are orphan; iOS no longer reads them. Delete in ops cleanup (§10 v1-cleanup tasks). |

The `.png` extension is a **logical name**, not a format assertion. iOS `CachedAsyncImage` decodes via `UIImage(data:)` by content, ignoring the extension; browsers / `URLSession` honor the stored `Content-Type`. Storing JPEG bytes under a `1.png` key with `Content-Type: image/jpeg` is correct and avoids any re-encode (which would be an adaptation → CC-BY-SA ShareAlike — §7 D-format).

### 2.3 `slug` — byte-exact Go port of iOS `PlantImageURL.slug` (THE central invariant)

iOS (`app/YardMate/YardMate/RemoteContent/PlantImageURL.swift`, shipped PR #227, **trinomial — PR #27 binomial folding was reverted in PR #28 / #242 per PIVOT**):
- lowercase the whole string;
- iterate characters; keep `[a-z0-9]`; any run of non-`[a-z0-9]` collapses to a **single** `-`; **no leading dash**, **no trailing dash**.
- `Rosa regina sueciae` → `rosa-regina-sueciae`. Subspecies are NOT folded to species; the trinomial is preserved.

Go port (single source of truth on the server side; **must produce the identical string for the identical input**):
```go
// Slug mirrors iOS PlantImageURL.slug byte-for-byte. Do NOT add unidecode /
// NFD / NFC / transliteration — iOS treats every non-[a-z0-9] code point as a
// separator (it is NOT in the allowed set), so "é" / "×" / spaces all collapse
// to a single "-". Transliterating "é"→"e" here would produce a DIFFERENT slug
// than iOS → a permanent 404 on the image (pitfall §9 #1).
//
// Also do NOT fold trinomial → binomial. PR #27 (backend) + PR #241 (iOS) tried
// that and were reverted (PR #28 / #242) per 2026-05-28 PIVOT: subspecies must
// keep their own gallery, since search + detail must show the same images.
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
`GenusSlug(name)` = `Slug(firstSpaceDelimitedToken(name))` (iOS `genusSlug`). Implemented for completeness + future genus fill, **not invoked** by V2 (genus fallback = §8).

The invariant chain (must hold or the whole feature 404s): iOS sends scientific name `X` to `POST /v1/plants/imageingest` → backend computes `Slug(X)` → iOS later renders the same plant and computes `Slug(X)` to fetch from R2. Same function, same input ⇒ same key. **Neither side may pre-process the name before slugging** (pitfall §9 #2). Locked with a Go unit test mirroring iOS test vectors (§10).

### 2.4 Multi-source cascade + license filter

**Cascade order** (V2: iNat → Wikimedia; GBIF / USDA = §8):

```
accumulator := []
for source in [iNat, Wikimedia]:
   candidates := source.search(scientific_name, limit=12, exclude=alreadyUsedURLs)
   classified := classify(candidates)                    -- per-source license schema → §2.4.3
   eligible := gateFilter(classified)                    -- BY/SA dropped when flag OFF
   accumulator.append(eligible)
   if len(accumulator) >= N*2 → break early              -- enough; stop probing further sources
selectTopN(accumulator, N=image_count)                   -- §2.5 ranking + within-gallery dedup
```

#### 2.4.1 iNaturalist (primary)

**Taxa search** (one HTTP GET, JSON):
```
GET https://api.inaturalist.org/v1/taxa?q=<scientificName>&rank=species&per_page=5
UA: YardMate-ImageIngest/2.0 (https://yardmate.ai; contact@yardmate.ai)
```
Response: matched taxa with `default_photo: {medium_url, large_url?, license_code, attribution, url}` + `id`. License codes: `cc0`, `cc-by`, `cc-by-nc`, `cc-by-nc-nd`, etc. (lowercase hyphen-delimited — same tokenization as §2.4.3).

**Observations photos** (when default_photo is missing or non-free): **two** HTTP GETs per matched taxon (limit 1, first match) — the full set (below) **plus** a `cc0,cc-by`-only query whose results are added first, so a votes-window full of high-vote CC-BY-SA can't crowd out preferred-license photos before ranking (critical while the BY/SA gate is OFF — those SA are gated and the slot would wrongly fall through; Codex #42/#43):
```
GET https://api.inaturalist.org/v1/observations?taxon_id=<id>&photo_license=cc0,cc-by,cc-by-sa&per_page=12&order_by=votes
UA: <same>
```
Returns observations with `photos: [{url, large_url, original_url, license_code, attribution}, ...]`. Use `large_url` (≈1024px CDN rendition) as the download URL — comparable to Wikimedia's 1600px thumb in actual hero quality.

iNat advantages: photos are real wild specimens (vs Wikimedia's mix of herbarium plates / botanical illustrations / locality maps), license metadata is structured (no `extmetadata` HTML parsing), and the API is faster than Commons search.

#### 2.4.2 Wikimedia Commons (cascade fallback)

Identical to v1: `commons.wikimedia.org/w/api.php?generator=search&gsrnamespace=6&prop=imageinfo&iiprop=url|mime|size|extmetadata&iiurlwidth=1600`. Reached only when iNat returned `< N` eligible candidates. Same UA + `maxlag=5` + `Retry-After` etiquette (§4.1). License classification via `extmetadata.License.value` (machine code) with `LicenseShortName` fallback for older files.

#### 2.4.3 License classification (shared across sources)

```
code := lower(license_code_or_extmetadata_License)    // hyphen-delimited tokens
tokens := split(code, "-")                            // version token (4.0 / 3.0 / absent) is IRRELEVANT
if "nc" in tokens or "nd" in tokens             → REJECT (non-commercial / no-derivatives)
allow if (token-MEMBERSHIP, NOT glob — covers BOTH iNat's unversioned codes
          `cc-by`/`cc-by-sa`/`cc0` AND Wikimedia's versioned `cc-by-4.0`/`cc-by-sa-3.0`;
          a literal `cc-by-*` glob would wrongly reject bare `cc-by` → silently filter
          out the iNat primary source once the gate is on — Codex #29-B):
   "cc0" in tokens                              → CC0       (no attribution required)
   "pd" in tokens / "public" in shortName       → PD        (no attribution required)
   "cc" & "by" & "sa" in tokens                 → CC-BY-SA  (attribution + ShareAlike)
   "cc" & "by" in tokens (no sa/nc/nd)          → CC-BY     (attribution)
else                                            → REJECT (ARR / GFDL-only / unknown → conservative)
```
Plus the v1 safeguards: `extmetadata.Copyrighted == True` and no allowed CC/PD code → reject; GFDL-only → reject; unclassifiable → skip.

**Attribution gate (unchanged from v1, §7 D-attribution-gate):** CC-BY / CC-BY-SA upload eligible only when `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES=true`. V2 default OFF. While OFF, BY / SA candidates produce `deferred_attribution` per image_index (§3, §9 #13). CC0 / PD always eligible.

### 2.5 Candidate selection (which photo becomes each image_index)

V2 picks top-N candidates (N = `image_count`, default 4). Rank acceptable candidates by:
1. **License = eligibility only, NOT a rank key**: among allowed licenses (CC0/PD/CC-BY/CC-BY-SA — NC/ND/ARR rejected by §2.4.3) ranking is **license-blind / quality-first** — pick the best image regardless of license, so SA competes equally (attribution auto-recorded; product decision to match the iOS display's votes/quality-first selection). The §2.4.3 gate is still an *eligibility* filter: candidates gated out (BY/SA while the flag is OFF) are excluded before ranking, and a slot whose only acceptable candidates are gated → `deferred_attribution` (§3, per image_index). *(Was: license tier `CC0=PD > CC-BY > CC-BY-SA` — removed.)*
2. **Source tier (V2)**: prefer iNat over Wikimedia (real specimen photos vs Commons mixed bag). Matches "visual continuity" intent (D-source-cascade).
3. **Photo-likeness / size** (tiebreak): prefer raster photos over diagrams / maps. Heuristics:
   - reject `image/svg+xml`, `image/gif`, `image/tiff` outright;
   - deprioritize Wikimedia titles containing `map | range | distribution | herbarium | illustration | diagram | chart | locator`;
   - prefer larger pixel area but cap absurd originals (§4.1 size guard).
4. **Within-gallery dedup (V2)**: after picking image_index `i`, exclude the chosen canonical source URL from the pool for `i+1..N`. Compare on iNat photo `id` / Wikimedia `File:` page — different scaled renditions of the same source = duplicate.
5. First candidate passing download + MIME re-check wins for that slot; on failure, fall through to the next acceptable candidate before declaring `source_error`.

"Photo-likeness" is heuristic, not guaranteed. Refinement (Commons `incategory:` precision, ML photo-vs-diagram) is §8.

### 2.6 Upload

`PutObject(Bucket=yardmate-static, Key=plant_images/{slug}/{image_index}.png, Body=bytes, ContentType=<real mime>, CacheControl="public, max-age=31536000, immutable")`. Bytes are the **downloaded bytes, verbatim** (§7 D-format). No `ACL` param (R2 ignores S3 ACLs; public read is configured at the bucket / custom-domain level, already serving the AAA images).

### 2.7 `credits.json` export (the iOS Credits page's data source)

After every ingest trigger, rebuild the full credits manifest from `plant_image_files` JOIN `plant_image_species` and upload to R2 — a static CDN file iOS reads (no new endpoint / no auth; same read-from-CDN pattern the app uses for catalog JSON).

| | |
|---|---|
| R2 key | `plant_images/credits.json` |
| Public URL | `https://images.yardmate.ai/plant_images/credits.json` |
| Content-Type | `application/json` |
| CacheControl | `public, max-age=3600` (short — changes as ingest runs, unlike the immutable images) |

Shape (V2 — per `(slug, image_index)`):
```json
{
  "generated_at": "2026-05-28T12:00:00Z",
  "entries": [
    {
      "slug": "monstera-adansonii",
      "image_index": 1,
      "scientific_name": "Monstera adansonii",
      "source": "inaturalist",
      "license_short": "CC BY 4.0",
      "license_url": "https://creativecommons.org/licenses/by/4.0/",
      "author": "Jane Doe (jdoe)",
      "source_url": "https://www.inaturalist.org/photos/12345678"
    },
    {
      "slug": "monstera-adansonii",
      "image_index": 2,
      "scientific_name": "Monstera adansonii",
      "source": "wikimedia_commons",
      "license_short": "CC BY-SA 4.0",
      "license_url": "https://creativecommons.org/licenses/by-sa/4.0/",
      "author": "Walter Hood Fitch",
      "source_url": "https://commons.wikimedia.org/wiki/File:Example.jpg"
    }
  ]
}
```
- Built from `plant_image_files WHERE status='ingested'` JOIN `plant_image_species` — credits.json always matches what is live in R2.
- One entry per **(slug, image_index)** in V2 (vs v1 one-per-slug). A multi-image gallery may carry different author + license per slot.
- **Full rebuild per ingest, never diff-append** (pitfall §9 #18) — deleted / re-ingested rows must drop / refresh cleanly.
- While the BY/SA gate is OFF only CC0 / PD rows are `ingested`, so credits.json carries those. When the gate flips ON, the next ingest's rebuild adds BY / SA rows automatically — the iOS page re-fetches, no code change.

### 2.8 In-catalog supplementary images (`external/` subfolder, zero-DB)

**Purpose.** In-catalog (curated 1522) plants render a gallery of 4 offline-generated semantic images (`{AAA}/{1_whole..4_scene}.png`, the `scripts/` pipeline's domain). This endpoint appends **up to 4 third-party supplementary images** AFTER those 4, sourced from the SAME iNat → Wikimedia cascade as the out-of-catalog path, so a curated detail page shows `4 curated + N supplementary`. Lazy-triggered: iOS fires it the first time a curated detail page mounts with an empty `external/`. The first viewer sees the 4 curated + a transient live-iNat fallback while R2 fills; subsequent viewers get the stored R2 supplementary images (companion `yardmate-swiftui` SPEC owns the iOS side).

**Entry point.** Public `POST /v1/plants/catalog-images` (App Attest log-only + per-IP/per-device rate-limited, same group as `/v1/plants/imageingest`). Body `{catalog_id, scientific_name, image_count?}`:
- `catalog_id` — the AAA id, validated `^[A-Z]{3}[0-9]{4}$`. This is a **security boundary**, not mere validation: it is interpolated into the R2 key, so the strict charset (no `/`, no `.`) forecloses path traversal / curated-image clobbering by an attested client.
- `scientific_name` — drives the cascade search (same as out-of-catalog).
- `image_count` — optional, default 4, clamp [1,6].

Returns 202 fire-and-forget; work runs in a goroutine bounded by single-flight keyed `"catalog:"+catalog_id` (concurrent duplicate opens coalesce). `IngestCatalogSpecies` is the core (`proxy/imageingest/catalog.go`).

**R2 layout.**

| | |
|---|---|
| Image keys | `plant_images/{AAA}/external/{i}.png`, `i ∈ 1..N` (1-based, matching the out-of-catalog convention) |
| Manifest | `plant_images/{AAA}/external/index.json` — `{count, images:[{index, source, license_short, license_url, author, source_url}]}` |
| Image CacheControl | `public, max-age=31536000, immutable` (same as the out-of-catalog images) |
| Manifest CacheControl | `public, max-age=3600` (short — refreshable on re-ingest, like credits.json) |

**Zero-DB design (the key divergence from §2.1).** The in-catalog path writes **NO** `plant_image_*` ledger rows and does **NOT** rebuild the global `credits.json`. The curated set is large and these images are best-effort enrichment; a per-species `index.json` built from the in-memory ingest outcome carries all the attribution iOS needs (the per-species analogue of credits.json, fully decoupled). Consequences:
- **Attribution** lives in each species' `external/index.json` (built by `BuildExternalIndex` from the `ImageOutcome` slice), never in the global credits.json — so this path can never wipe the out-of-catalog credits.
- **Idempotency** is the existence of `index.json`, written **LAST** (after every image upload) as a commit marker: a species whose `index.json` already exists is skipped wholesale (`already_done`). There is no per-slot ledger, so a partial fill (images uploaded, `index.json` not yet written) is re-done in full on the next trigger rather than resumed — the manifest is the single source of "done". `index.json` is written only when ≥1 image was stored, so a species iNat has no free photos for is retried later (never marked done-with-zero).
- **License gate** reuses §2.4.3: with the BY/SA gate ON (prod) `eligible` already includes CC-BY/-SA. The in-catalog path ignores `gated`/`deferred_attribution` (no ledger to park deferrals in) — a curated plant simply shows fewer supplementary images if a gate is OFF.

**Reuse vs divergence.** Candidate gathering (`gatherCandidates`), ranking (§2.5), license classification (§2.4.3), download + MIME sniff (§4.1), and R2 upload (§2.6) are shared **verbatim** with the out-of-catalog path. Only three things differ: the key scheme (`catalogKey`), the manifest writer (`BuildExternalIndex` instead of the ledger-backed credits rebuild), and the zero-ledger slot fill (`fillCatalogSlot`). It never writes the 4 curated semantic images, never the slug-keyed gallery, never the global credits.json.

---

## 3. Error / outcome matrix (v2: per image_index)

Outcomes are **per (slug, image_index)** in v2, not per-species. A single `/v1/plants/imageingest` request can produce a mixed-status ingest (e.g. 4-image gallery: 2× `ingested` + 1× `deferred_attribution` + 1× `source_error`). Per-image status drives the ledger (§6.1 `plant_image_files`); species-level state (`plant_image_species`) is aggregated.

| Per-image Status | Meaning | `plant_image_files` row | Retry behavior |
|---|---|---|---|
| `ingested` | uploaded to R2 + attribution recorded | `status=ingested` | terminal (until manual delete) |
| `skipped_exists` | R2 already has the key (ledger / HEAD agree) | `status=ingested` (backfilled if missing) | terminal |
| `no_acceptable_image` | cascade exhausted (no CC0/PD/BY/SA candidate) | `status=no_acceptable_image` | negative-cached per `(slug, image_index)`; re-attempt only after `IMAGEINGEST_NOIMAGE_TTL` (default 30d) or manual delete |
| `deferred_attribution` | best/only acceptable candidate is CC-BY / CC-BY-SA but gate is OFF | `status=deferred_attribution` + chosen candidate URL / license / author stored in `pending_url` | skipped until gate flips ON; v2 re-cascades (NOT permanent negative cache — §9 #13) |
| `source_error` | source API 5xx / network failure / all candidate downloads failed | `status=failed`, `attempts++` | retried on every subsequent trigger (transient / infra errors self-heal); `attempts` observability only — no TTL re-check, so an outage never parks a healthy slot forever |
| `upload_error` | R2 PutObject failed | `status=failed`, `attempts++` | retried on next trigger |

Species-level aggregate in `plant_image_species`:
- `image_count_filled` = count of `image_index` rows with `status=ingested`
- `last_triggered_at` = most recent ingest invocation
- A species is "done" when `image_count_filled >= image_count_requested`; otherwise next trigger fills remaining slots (idempotent — §6.1).

**HTTP errors** (auth / config / rate problems surfaced as `{"error":"<code>"}`):

| Code | HTTP | Meaning |
|---|---|---|
| `missing_device_id` / `missing_app_version` | 400 | `/v1/plants/imageingest` required headers absent. **App Attest is log-only in V1** — same as `/v1/identify`, the attest envelope is read + logged but NOT verified (iOS 26 issue, §5 / option_d_progress); there is no `missing_attest` / `bad_attest` 401 in V1. |
| `missing_admin_token` / `bad_admin_token` | 401 | `/internal/imageingest/run` admin token absent / mismatched |
| `ingest_disabled` | 503 | R2 / DB not configured (service nil) |
| `bad_request` | 400 | empty / slug-empty `scientific_name`; `image_count` out of range; malformed JSON; internal run with neither `slug`+`name` nor `names` |
| `rate_limited` | 429 | per-IP / per-device cap exceeded (§4.2; surfaced by the rate-limit middleware) |
| `internal` | 500 | unmapped |

Source 429 / `maxlag` from iNat / Wikimedia are handled internally (back off + honor `Retry-After`, §4.1), never surfaced to the client.

---

## 4. Source rate-limit etiquette + public endpoint cap + size caps

V2 has two rate-limit dimensions: outbound (we are the *client* of iNat / Wikimedia, the polite party) + inbound (we receive `/v1/plants/imageingest` from arbitrary attested iOS devices, must cap to prevent abuse).

### 4.1 Outbound: iNat + Wikimedia etiquette

- **User-Agent is mandatory** for both. Anonymous requests without descriptive UA → 403 (Wikimedia) or rate-clamped (iNat). Format: `YardMate-ImageIngest/2.0 (https://yardmate.ai; contact@yardmate.ai)` (override via `IMAGEINGEST_USER_AGENT` / `INATURALIST_USER_AGENT`). Applied to ALL outbound HTTP (search + byte download).
- **Serial, paced.** No concurrency across image_index within a single ingest; `minInterval` (default `1s`, env `IMAGEINGEST_MIN_INTERVAL`) between source HTTP calls. Single-flight per slug — concurrent requests for the same slug coalesce (§4.2).
- **iNat limits:** 100 req/min per IP, 10000/day. Per-trigger usage ~3 outbound calls (taxa + observations + optional photo metadata) keeps us well below. Honor 429 `Retry-After`.
- **Wikimedia `maxlag=5`** on api.php queries — Wikimedia replies 503 + `Retry-After` when replication lag exceeds 5s; honor it.
- **`Retry-After` / 429 / 503:** exponential backoff honoring the header, capped (3 retries, max 30s wait) per call; on exhaustion → `source_error` for that image_index.
- **Download size guard:** cap byte downloads (`IMAGEINGEST_MAX_BYTES`, default 25 MB) via `LimitReader`. iNat `large` ≈ 1024px / 100–300 KB typical; Wikimedia 1600px thumb 200–600 KB — the cap is a backstop. The download client targets only a source-returned `Candidate.DownloadURL` — an iNat / Wikimedia CDN URL from a trusted API response (incl. the hero candidate, which is matched within those API results, §2.1.1). It **never** fetches a client-supplied URL.
- **MIME re-check on downloaded bytes** (`http.DetectContentType` on first 512 B) — source-declared mime is advisory; stored `Content-Type` comes from actual bytes. Reject if not `image/jpeg|png|webp`.

### 4.2 Inbound: `/v1/plants/imageingest` cap + single-flight

- **Per-attested-device rate limit** (same middleware as `/v1/identify` — see `proxy/SPEC.md` §3): default 100/min, env `IMAGEINGEST_PUBLIC_RATE`. Returns 429 with `Retry-After` on exceed.
- **Single-flight per slug**: concurrent requests for the same `Slug(scientific_name)` — from same OR different devices — coalesce. Second request returns 202 immediately, no additional outbound work queued. Implemented via in-memory mutex keyed by slug (process-local; OK for V2 single-instance — pitfall §9 #19 if scaled).
- **Anti-abuse**: per-trigger outbound work is naturally bounded (~3–5 source calls per ingest, regardless of `image_count` slots — searches return many candidates at once) so even at the per-device cap, total outbound pressure is manageable. Cap is anti-abuse, not anti-runaway.

---

## 5. Security model

- **Public surface: `POST /v1/plants/imageingest`** (App Attest gated, same envelope as `/v1/identify`). Attest failure → 401. Per-attested-device rate limit (§4.2). Single-flight per slug coalesces duplicates (anti-abuse via dedup, not just rate). The request body is `scientific_name` + `image_count` + an OPTIONAL `hero_image` (§2.1.1). **No client-supplied URL is ever fetched** — the backend acts only on `hero_image.photo_id` (an identifier it MATCHES within its own iNat API results), and downloads exclusively from iNat / Wikimedia API-returned URLs (§4.1). So `hero_image` carries **no SSRF surface** and needs **no host allowlist** (the Option-Y choice — §7 D-hero-passthrough; §2.1.1).
- **Hero metadata is never trusted from the client (§9 #15–16).** `hero_image.license_code` / `attribution` / `url` are advisory (iOS display only). The license, attribution, source_url, and rendition the backend STORES + writes into `credits.json` all come from the **iNat API record** of the matched `photo_id` (i.e. the cascade candidate), never from the request body. A malicious attested client therefore cannot (a) relabel an NC/ARR photo as CC0 to get it stored to the public bucket, nor (b) poison `credits.json` with attacker-controlled author text. If `photo_id` matches no eligible cascade candidate, there is no pass-through — slot 1 cascades.
- **iOS-display compliance (companion-SPEC constraint, surfaced here).** Because ~74% of iNat `default_photo`s are NC/ARR (measured), iOS MUST NOT hot-link-display the default photo unconditionally — it shows only free (CC0/CC-BY) photos (the same free-observation selection the backend uses — §2.1.1 no-jump rule), else the *app itself* serves NC imagery commercially. This is an iOS obligation; the backend can't enforce it, but the contract depends on it.
- **Internal surface: `POST /internal/imageingest/run`** (admin-token gated, internal-only). Mounted outside the `/v1` group → not in nginx public vhost. Server binds `127.0.0.1:8080` behind nginx → `/internal/*` unreachable externally even before the token check.
- **Admin token.** `X-Ingest-Admin-Token == IMAGEINGEST_ADMIN_TOKEN`, **constant-time compare** (`subtle.ConstantTimeCompare`). Route registered only when secret is set. **Server-only**; NOT in `main.vendedKeys` (pitfall §9 #6).
- **R2 credentials are write-scoped + server-only.** R2 API token scoped to *Object Read & Write* on `yardmate-static` only.
- **Source input (iNat / Wikimedia public APIs) is low-risk.** Search term URL-encoded; downloaded bytes treated as opaque image data (sniffed for MIME, never executed); `extmetadata` / iNat `attribution` HTML-stripped before storage. ALL download URLs (including the matched hero candidate) come from the source API responses (iNat / Wikimedia), never from clients — so there is no SSRF surface from user input at all. The only user-supplied datum acted on is `hero_image.photo_id`, used as a lookup key, never as a fetch target.
- **Attribution integrity.** CC-BY / CC-BY-SA store author + license + file-page so the iOS Credits page satisfies CC §3(a). CC0 / PD recorded for audit.
- **DSN secrecy** inherits enrichment SPEC §9 #14 — `SUPABASE_DB_URL` carries the DB password; never log / echo it.

---

## 6. Schema

### 6.1 Supabase ledger — two tables (v2)

V2 splits v1's single `plant_image_ingest` table into:
1. **`plant_image_species`** — one row per slug (species-level state for gallery aggregation + trigger dedup);
2. **`plant_image_files`** — one row per `(slug, image_index)` (per-image idempotency + attribution).

DDL is split across two migrations for build-green expand/contract delivery: `002_plant_image_v2.sql` (Slice 1) **CREATEs** the two tables alongside the still-read v1 `plant_image_ingest`; `003_drop_plant_image_ingest.sql` (Slice 3) **drops** v1 once every caller moved to the two-table API. The single manual v1 row carries no production data; loss is benign.

```sql
CREATE TABLE plant_image_species (
  slug                    TEXT PRIMARY KEY,
  scientific_name         TEXT NOT NULL,
  image_count_requested   INT  NOT NULL DEFAULT 4,
  image_count_filled      INT  NOT NULL DEFAULT 0,
  last_triggered_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_plant_image_species_last_triggered ON plant_image_species (last_triggered_at);

CREATE TABLE plant_image_files (
  slug                    TEXT NOT NULL REFERENCES plant_image_species(slug) ON DELETE CASCADE,
  image_index             INT  NOT NULL,
  status                  TEXT NOT NULL
                            CHECK (status IN ('ingested','no_acceptable_image','failed','deferred_attribution')),
  r2_key                  TEXT,                                    -- 'plant_images/{slug}/{i}.png' when ingested
  source                  TEXT NOT NULL                            -- 'inaturalist' | 'wikimedia_commons' | future 'gbif' | 'usda'
                            DEFAULT 'wikimedia_commons',
  source_url              TEXT,                                    -- upstream URL (iNat photo page / Wikimedia File: page)
  pending_url             TEXT,                                    -- chosen BY/SA candidate URL stored while deferred_attribution (write-only; v2 re-cascades on flip — fast-path P2)
  license_code            TEXT,                                    -- 'cc-by-sa-4.0' | 'cc0' | 'pd' | ...
  license_short           TEXT,                                    -- 'CC BY-SA 4.0' for display
  license_url             TEXT,
  attribution_author      TEXT,                                    -- HTML-stripped Artist (Wikimedia) / iNat user real name + login
  attribution_required    BOOLEAN NOT NULL DEFAULT FALSE,
  mime                    TEXT,
  bytes                   BIGINT,
  width                   INT,
  height                  INT,
  attempts                INT NOT NULL DEFAULT 0,
  last_error              TEXT,
  created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (slug, image_index)
);
CREATE INDEX idx_plant_image_files_status ON plant_image_files (status);
CREATE INDEX idx_plant_image_files_source ON plant_image_files (source);
```

- **`plant_image_species` PK = `slug`** — species-level. Upsert on every trigger; `image_count_filled` recomputed from `plant_image_files` after each ingest pass.
- **`plant_image_files` PK = `(slug, image_index)`** — image-level. `ON DELETE CASCADE` from species: deleting a species drops all its file rows.
- **Re-ingest a slot:** manually `DELETE FROM plant_image_files WHERE slug=... AND image_index=...` (and the R2 object) → next trigger re-fills that slot.
- **Re-ingest a whole species:** `DELETE FROM plant_image_species WHERE slug=...` (cascades to files; manually delete R2 prefix `plant_images/{slug}/*`) → next trigger starts fresh.
- **Negative cache** (`no_acceptable_image`): per `(slug, image_index)`; skipped until `now() - updated_at > IMAGEINGEST_NOIMAGE_TTL` (default 30d) or manual delete.

### 6.2 No more seed read (v1 deprecation)

V1's `SELECT scientific_name FROM plants_pending` is **removed**. V2 is purely on-demand. The `plants_pending` table is still owned by enrichment; imageingest no longer reads it — the coupling is broken.

If ops ever need to bulk-fill (e.g. R2 bucket restore), they use the internal endpoint with `?names=SciName1,SciName2` (manually-curated scientific-name list) — not the enrichment table.

---

## 7. Resolved decisions (don't re-debate)

**V2 pivot decisions (2026-05-28, Yao):**

- **D-pivot-2026-05-28: V1 batch-from-seed withdrawn.** V1 (`POST /internal/imageingest/run` + ticker, Wikimedia-only, single `hero.png`, `plants_pending` seed) shipped (PR #22 / #23 / #24) and was validated on `Monstera adansonii`. Withdrawn pre-traffic because (a) batch trigger violated "no batch scraping" intent, (b) Wikimedia-only diverged from in-app iNat photos, (c) single hero couldn't match the in-catalog 4-image gallery, (d) PR #27 binomial slug folding broke "different subspecies must show different images" (reverted in PR #28). Memory `v1_image_self_hosting` records the full PIVOT. V2 redesigns trigger + source + layout while preserving R2 ownership + B-档 license + `credits.json`.
- **D-cascade-only (2026-05-28) — SUPERSEDED by D-hero-passthrough (2026-05-29).** *Historical:* an earlier v2 draft had a pass-through path forwarding image URLs from `/v1/identify` / `/v1/diagnose`. Verifying the contracts killed THAT premise: identify returns **PlantNet-host** `image_url` (TOS-blocked, no `license_code`); Plant.id / AI return `null`; iOS Search didn't capture iNat `license_code`. So at the time no flow had a usable `{URL, license}` pair → v2 shipped cascade-only. **What changed (see D-hero-passthrough):** the realign sources the hero from **iNaturalist (not identify/PlantNet)** and has iOS capture the iNat `license_code`, which IS the unblock this decision itself named. Cascade-only remains the *fallback* (no `hero_image`), but it is no longer the only mode.
- **D-hero-passthrough (2026-05-29 realign, Yao).** The shipped cascade-only behavior didn't meet the UX bar: an out-of-catalog detail page showed a **placeholder on first open** (image only appeared on a later visit, after async ingest), and the backend's own cascade pick for slot 1 could **differ from the iNat photo the user saw in Search** → a visual "jump" when the hero switched to R2. Decision: iOS forwards the **iNat `photo_id`** of the photo it displayed; the backend MATCHES that id **within its own §2.4 cascade results** and stores that candidate as slot 1 (§2.1.1). This makes **search image == detail hero == stored R2 slot 1**.
  - **Why `photo_id`-match and NOT "fetch the forwarded URL" (chose Option Y over Option X after review).** A first draft had iOS forward the image URL for the backend to fetch + transcode, gated by an iNat-CDN host allowlist. Review (internal + Codex on the doc PR) showed that path keeps two holes: (1) the forwarded `license_code`/`attribution` are client-supplied — re-classifying a client string doesn't stop a client lying (relabel an NC photo "cc0" → store it to a public bucket; poison `credits.json`); (2) fetching a client URL is an SSRF surface needing allowlist + redirect-off + IP-pin. Option Y removes both at the root: the backend fetches nothing client-supplied and derives license/attribution/rendition from the **iNat-authoritative** cascade record of `photo_id`. Client `url`/`license_code`/`attribution` become advisory (iOS display only).
  - **Distinctions from the 2026-05-28 dropped draft:** source is **iNat, not PlantNet** (redistributable + iOS captures license — the §8 unblock); **display ≠ storage** (iOS hot-link-displays the transient iNat URL for first view, ungated, inline caption satisfies attribution; *storage* to R2 still obeys the BY/SA gate).
  - **No-jump cost.** The only jump case is `photo_id` not present in the backend's candidate pool. Sampling showed the displayed hero is a free *observation* photo for ~74% of species (default_photo is NC/ARR), so iOS and the backend MUST pin the identical iNat free-photo selection and the backend window must be a superset of iOS's pick (§2.1.1 no-jump rule). Slots 2..N still cascade. Pass-through is a pure optimization, never a hard dependency.
  - **Hero is an iOS-side selectable pointer** (default slot 1; future: user picks another gallery image or a diary photo) — the backend contract is unaffected.
- **D-trigger-public-endpoint: `POST /v1/plants/imageingest` is the iOS-facing trigger.** Not `/v1/identify` (would couple identify to image ingest), not `/v1/plants/enrichment` (SPEC §1.2 reaffirmed — text in / JSON out, no R2 writes). New independent endpoint, App Attest gated (same envelope as `/v1/identify`). iOS fires it on out-of-catalog detail-page mount (companion SPEC §3). Internal `/internal/imageingest/run` retained for ops (single-slug retry, bulk reseed during incidents) — NOT iOS-facing.
- **D-multi-image-naming: positional `{i}.png`, not semantic `whole/closeup/...`.** Out-of-catalog sources don't carry photo-intent metadata. Inferring "whole vs closeup" from EXIF / heuristics is unreliable. Positional is honest; iOS handles in-catalog (semantic) + out-of-catalog (positional) via separate `PlantImageURL` builders.
- **D-source-cascade: iNat primary → Wikimedia fallback.** iNat photos are real wild specimens with structured license metadata (faster than Wikimedia `extmetadata` HTML parsing) and match what Search shows users (visual continuity — Search list iNat photo → detail hero from the same source family). Wikimedia kept as fallback because Commons catalogs many species iNat lacks. GBIF / USDA = §8 (USDA has no public API; GBIF mediaSpecies has lower per-species coverage).
- **D-public-rate: 100 req/min per attested device** (same default as `/v1/identify`), env `IMAGEINGEST_PUBLIC_RATE`. Single-flight per slug coalesces duplicates. Per-trigger outbound work is small (~3–5 source calls regardless of image_count) so cap is anti-abuse not anti-runaway.
- **D-ledger-two-table: `plant_image_species` + `plant_image_files`.** V1's single-table schema can't represent multi-image gallery state (one species → N image rows). Two-table separates species-level aggregate (count filled, last trigger) from per-image facts. The v1 `plant_image_ingest` table is **dropped** in migration 003 (002 only creates, for expand/contract build-green delivery); the single manual v1 row carries no production data.

**Retained from v1 (Yao 2026-05-27):**

- **D-genus: species slug only; genus is P2.** iOS reads only the species slug today; filling genus now = wasted source / R2 work iOS won't read. Core is slug-parameterized.
- **D-format: store bytes verbatim, real Content-Type, NEVER re-encode.** Downloading sources' own scaled / CDN renditions ("reproduction in another size") is NOT an Adaptation under CC 4.0 §1(a), so CC-BY-SA does not attach. Re-encoding by us (even JPEG→PNG to match `.png` extension) would be an adaptation → SA attaches → "不加工" intent broken. The `.png` extension is logical; iOS decodes by content. Allowed MIMEs: `image/jpeg|png|webp`. iNat `large_url` ≈ 1024px; Wikimedia 1600px thumb — both source-generated downscales.
- **D-license-conservative:** classify into CC0 / PD / CC-BY / CC-BY-SA or SKIP. Token-based NC / ND rejection. Unknown / ARR / GFDL-only → skip. Placeholder beats license violation.
- **D-attribution-gate (Codex #22, retained verbatim):** BY/SA gated behind iOS Credits page; flag `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES` defaults OFF. V2 ships with OFF → uploads CC0 / PD only (~20% coverage per `v1_image_self_hosting` sampling). Once iOS Settings → Credits page is live (companion SPEC §6), ops flips ON → BY / SA upload → ~98% coverage. The flag is **release coordination**, NOT a permanent BY/SA-off switch.
- **D-auth: admin-token (retained) + App Attest (new for public).** `IMAGEINGEST_ADMIN_TOKEN` unchanged for `/internal/imageingest/run`; new App Attest envelope for `POST /v1/plants/imageingest` (same as `/v1/identify`).
- **D5 (R2 client): aws-sdk-go-v2** unchanged.

---

## 8. Out-of-scope (V1.1+ candidates)

- **~~Pass-through reuse of already-shown images.~~ MOVED INTO V1** as hero pass-through (§2.1.1, D-hero-passthrough). iOS forwards the displayed photo's `photo_id`; the backend matches it within its own iNat cascade results and stores that candidate as slot 1 (no client-URL fetch, iNat-authoritative license — §5). Remaining V1.1 extensions: forwarding `photo_id`s for **gallery slots 2..N** too (not just the hero), and a `pending_url` fast-path on gate flip (below). identify/diagnose URLs remain pass-through-ineligible (PlantNet host / no license).
- **Genus-level fallback fill** — turn on once iOS reads the genus slug (iOS P2, companion SPEC §10). Core already parameterized.
- **GBIF mediaSpecies + USDA Plants** as additional cascade sources — ledger `source` column ready, license schemas to map.
- **Progress endpoint for `/v1/plants/imageingest`** — V2 returns 202 only; iOS retries on next page mount, idempotency handles dedup. A `GET /v1/plants/imageingest/status?slug=` could expose `image_count_filled / image_count_requested` for UI progress indicators.
- **Image variants (low-res 200px thumbnails for list views)** — V2 stores one rendition per image_index (~1024–1600px). A `{i}_thumb.webp` per slot would help list-view perf. iNat `medium_url` ≈ 500px works as source.
- **Re-ingest automation** (refresh stale / low-quality images) beyond manual ledger delete.
- **Photo-intent classification** — ML model to label "habit / leaf / flower / fruit" so positional `{i}.png` could be presented in a structured order. Currently positions are cascade output order.
- **iOS Settings → Credits page** — IN SCOPE for this initiative (companion SPEC §6), implemented in `yardmate-swiftui`. Sequenced **after** this backend SPEC ships + impl + dev-deployed; flip BY/SA gate ON only after the iOS page is in TestFlight.
- **Multi-instance single-flight** — V2 single-flight is process-local mutex (single-instance OK). Scaling to multiple backend instances would need redis / DB advisory lock for cross-instance dedup (pitfall §9 #19).
- **`pending_url` fast-path on gate flip** — when `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES` flips ON, V2 re-cascades for `deferred_attribution` rows. A fast-path that uploads the stored `pending_url` without re-search would be cheaper (write-only field already populated, P2).

---

## 9. Pitfalls (don't re-rediscover)

1. **`Slug` must be byte-exact to iOS — NO transliteration / normalization.** unidecode (`é`→`e`) or NFD / NFC would yield a different slug than iOS → permanent 404. iOS treats every non-`[a-z0-9]` code point as a separator; the Go port iterates runes and does the same. Lock with a unit test mirroring iOS vectors.
2. **Neither side may pre-process the name before slugging.** iOS sends the same string it received from identify / diagnose / user-typed; backend does NOT canonicalize / trim / lowercase-then-uppercase. Verify against iOS `PlantImageURL.slug` byte-vectors.
3. **`.png` key ≠ PNG bytes.** Store JPEG / WebP bytes under `{i}.png` with the real `Content-Type`. Do not force-encode (D-format).
4. **`plants_pending` is enrichment-owned, NOT read here in v2.** v1 read it; v2 deprecates. Do NOT re-add the seed coupling — V2 explicitly broke it.
5. **Negative cache or you hammer sources.** Without `no_acceptable_image` per `(slug, image_index)`, every retrigger re-cascades every empty slot. Always upsert the ledger, even on "no image".
6. **R2 creds / admin token are server-only — keep them OUT of `vendedKeys`.** `secrets/SPEC.md` §4.1: anything in `vendedKeys` is handed to every authenticated client. R2 write token + admin token there would be a credential leak.
7. **Constant-time admin-token compare** (`subtle.ConstantTimeCompare`) — naive `==` on the token is a timing oracle. Low-stakes (internal-only) but trivial to do right.
8. **HEAD before PUT for idempotency, but R2 is truth, not the ledger.** A `plant_image_files` row can exist while the R2 object was deleted (or vice versa). `skipped_exists` decided by `HeadObject`, then ledger reconciled.
9. **Wikimedia + iNat UAs are mandatory** — missing / empty UA → 403 (Wikimedia) or rate-clamped (iNat). Don't debug as rate-limiting.
10. **Don't log image bytes, full source JSON, or attest envelopes at INFO.** Log `slug`, `image_index`, `scientific_name`, `status`, `source`, `license_code`, `bytes`, latency. Never DSN / R2 secret / admin token / attest header.
11. **Single-flight per slug** — concurrent requests for the same slug must coalesce (don't double-trigger outbound source calls + ledger races). Mutex keyed by slug.
12. **`config.LoadDefaultConfig` for aws-sdk-go-v2 reads env / `~/.aws` by default** — pass explicit `credentials.NewStaticCredentialsProvider` + `BaseEndpoint` for R2 so it never accidentally picks up ambient AWS creds on the box.
13. **`deferred_attribution` is NOT permanent negative cache.** BY/SA gated rows are re-processed when `IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES` flips ON (`shouldSkip` returns false for deferred rows once the gate is on). V2 re-cascades; `pending_url` reserved for §8 fast-path.
14. **Do NOT flip the gate ON before the iOS Credits page is live.** R2 is public and iOS renders directly — uploading BY/SA makes them user-visible immediately. Flipping early = serving BY/SA without visible attribution = license violation (the exact Codex #22 issue).
15. **(hero pass-through) NEVER fetch `hero_image.url`, and NEVER trust client `license_code`/`attribution`.** The backend acts only on `hero_image.photo_id` — it MATCHES that id within its own iNat API cascade results and uses that record's authoritative license/attribution/rendition (§2.1.1). Fetching the client URL would be SSRF; trusting the client license would let a malicious attested client relabel an NC/ARR photo "cc0" → stored to a PUBLIC bucket = license violation, or poison `credits.json` with attacker author text. Advisory client fields are iOS-display-only.
16. **(hero pass-through) the hero is just a cascade candidate, so the gate applies unchanged.** Because the matched hero is an ordinary `eligible`/`gated` candidate from §2.4 (already license-classified from iNat-authoritative data), the BY/SA storage gate applies exactly as for any slot: CC0/PD → ingested, BY/SA → `deferred_attribution` (store its rendition as `pending_url`, mirroring `recordDeferred`), NC/ARR → never eligible (no match). Pass-through changes WHICH candidate lands in slot 1, never WHETHER the gate applies.
17. **(hero pass-through) match + exclude on the EXACT DedupKey, and use the no-jump window.** (a) Match `photo_id` as `"inat-photo-"+strconv.FormatInt(id,10)` — **canonicalize the client `photo_id` through int64 first** (inat.go builds the key from an int64); a raw string concat of an un-normalized value (leading zero / whitespace / sign / the URL) silently never matches → no pass-through → the 74% jump. (b) After slot 1 takes the matched candidate, remove it from the pool so slots 2..N don't repeat it (§2.5 #4). (c) The match only works if the displayed photo is IN the gathered pool: iOS and backend must use the identical iNat free-photo selection and the backend window must be a superset of iOS's pick (§2.1.1 no-jump rule) — otherwise ~74% of species (NC default → free-observation hero) can jump.
18. **`credits.json` is a full rebuild from the ledger, never a diff-append.** Rebuild from current `status='ingested'` rows every ingest so deletions / re-ingests stay consistent. V2 rebuild JOINs species + files for per-image entries.
19. **(V2) Single-flight is process-local.** In-memory mutex keyed by slug works for single-instance backend. Multi-instance scaling (§8) needs redis / DB advisory lock or a thundering herd resumes for trending species.
20. **(V2) Two-table ledger consistency.** Always upsert `plant_image_species` BEFORE inserting `plant_image_files` rows (FK + cascade). Always recompute `image_count_filled` from `plant_image_files` after each ingest — never trust a separately-incremented counter.
21. **(V2) v1 orphan R2 objects.** `plant_images/monstera-adansonii/hero.png` and `plant_images/monstera-adansonii-blanchetii/hero.png` exist from v1 smoke; iOS no longer reads `hero.png`. Delete in ops cleanup (§10).

---

## 10. Implementation outline (not part of the contract)

```
proxy/imageingest/
├── SPEC.md                          (this file, v2 cascade + hero pass-through §2.1.1)
├── slug.go                          Slug + GenusSlug (byte-exact iOS port — unchanged from v1)
├── slug_test.go                     iOS-mirrored vectors (unchanged from v1)
├── license.go                       extmetadata + iNat license_code → {allowed, code, short, url, author, attributionRequired} — token-membership (§2.4.3); covers iNat unversioned + Wikimedia versioned
├── license_test.go                  table: cc0/pd/cc-by/cc-by-sa allow (versioned AND unversioned); nc/nd/arr/gfdl/unknown reject
├── sources/                         multi-source cascade
│   ├── inat.go                      iNat /v1/taxa + /v1/observations (UA, 429 backoff)
│   ├── inat_test.go                 httptest fixtures
│   ├── wikimedia.go                 Wikimedia search + imageinfo (UA, maxlag, Retry-After — extracted from v1 commons.go)
│   └── wikimedia_test.go            httptest fixtures (search hit / 403-no-UA / 429-retry / no results)
├── r2.go                            S3 client wrapper (unchanged from v1)
├── r2_test.go                       unchanged from v1
├── credits.go                       build credits.json from plant_image_files JOIN species (per (slug, image_index))
├── credits_test.go                  JSON shape + full rebuild + gate-state coverage
├── ledger.go                        pgx: upsert plant_image_species + plant_image_files + Lookup(slug, i) + RecomputeCount(slug)
├── ledger_test.go                   hermetic two-table consistency
├── singleflight.go                  in-memory mutex map keyed by slug (anti-coalesce, §4.2)
├── singleflight_test.go             concurrent slug requests
├── ingestor.go                      Ingestor.IngestSpecies + gatherCandidates + selectBest + within-gallery dedup + slot-1 hero match (§2.1.1: NEW — `HeroPhotoID` on IngestRequest; PASS-2 i==1 looks up DedupKey "inat-photo-"+HeroPhotoID in eligible/gated, fills slot 1 + removes from pool; NO client-URL fetch, NO allowlist file. The obs window is already top-12 by votes in shipped inat.go — no change needed there; the no-jump constraint is iOS-side pinning.)
├── ingestor_test.go                 table-driven: all Status values + hero match (CC0 stored / BY deferred / NC=no-match→cascade / id absent from pool→cascade / matched hero excluded from 2..N)
├── handlers.go                      POST /v1/plants/imageingest (attest middleware, {scientific_name, image_count, hero_image?} — parse hero_image.photo_id, ignore advisory url/license/attribution) + POST /internal/imageingest/run (admin-token, outside /v1, no hero)
├── handlers_test.go                 attest 401 / admin 401 / disabled 503 / rate 429 / single + batch + hero_image.photo_id parse (advisory fields ignored)
└── migrations/
    ├── 001_plant_image_ingest.sql   (v1 history, dropped by 003)
    ├── 002_plant_image_v2.sql       (CREATE plant_image_species + plant_image_files; expand step)
    └── 003_drop_plant_image_ingest.sql (DROP plant_image_ingest; contract step)
```

`main.go` wiring (mirrors `buildEnrichmentService`):
```go
ingestSvc := buildImageIngestService(vault) // nil + WARN if R2 / DB / admin-token missing
// in newServer:
//   if ingestSvc != nil:
//     v1Group.Post("/plants/imageingest", attestMiddleware(ingestSvc.PublicHandler))
//     adminMux.Post("/internal/imageingest/run", ingestSvc.AdminHandler) // outside /v1, internal-only
//   NO ticker (v2 removed)
```

`secrets.env.prod` additions (`~/.config/yardmate-api/secrets.env.prod` → `deploy.sh` ships; `secrets.env.example` updated):
```
# v1 keys retained:
R2_ACCOUNT_ID=...
R2_ACCESS_KEY_ID=...
R2_SECRET_ACCESS_KEY=...
R2_BUCKET=yardmate-static
# R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com   (optional)
IMAGEINGEST_ADMIN_TOKEN=...               # internal manual-trigger gate; server-only
IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES=false   # release-coordination gate (Codex #22)

# v2 new:
INATURALIST_USER_AGENT=YardMate-ImageIngest/2.0 (https://yardmate.ai; contact@yardmate.ai)
# IMAGEINGEST_PUBLIC_RATE=100             # req/min per attested device for POST /v1/plants/imageingest
# IMAGEINGEST_MIN_INTERVAL=1s             # outbound pacing
# IMAGEINGEST_MAX_BYTES=26214400          # 25 MB download cap

# v1 keys no longer read (removed):
# IMAGEINGEST_TICK_INTERVAL                (ticker removed)
# IMAGEINGEST_BATCH_LIMIT                  (batch-from-seed removed)
```
`SUPABASE_DB_URL` already present. R2 creds + `IMAGEINGEST_ADMIN_TOKEN` continue to **NOT be in `main.vendedKeys`** (pitfall §9 #6).

Estimated effort: ~2 day impl (sources/inat + ingestor + handlers public + two-table migration + ledger) + ~0.5 day tests + ~0.5 day deploy + smoke (single-slug via internal endpoint, then real `POST /v1/plants/imageingest` from iOS dev build) ≈ 3 days.

**v1 cleanup tasks** (separate ops PR after v2 ships):
- Delete legacy R2 objects `plant_images/monstera-adansonii/hero.png` and `plant_images/monstera-adansonii-blanchetii/hero.png` (orphan; iOS no longer reads `hero.png`).
- Migration 003 drops `plant_image_ingest` table (no prod data lost; 002 only created the v2 tables).
- Delete `IMAGEINGEST_TICK_INTERVAL` / `IMAGEINGEST_BATCH_LIMIT` lines from `secrets.env.example` (they're no longer read).
