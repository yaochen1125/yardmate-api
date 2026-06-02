# `proxy/rosererank` package — rose cultivar rerank (V1, behind a test flag)

> Status: **draft — SPEC for review; implementation in a follow-up commit.**
> Companion: parent `proxy/SPEC.md` §2.1 `POST /v1/identify`. This package is **not a new endpoint** — it is an optional in-line step inside the identify handler, after the cascade settles and before plant_id resolution.
> Background: PlantNet (primary) + Plant.id (fallback) only resolve roses to a **species** (`Rosa chinensis` → "China Rose", `Rosa rugosa`, …). Garden roses are overwhelmingly **named cultivars** (`Rosa 'Peace'`, `Rosa 'About Face'`, …), so every rose photo comes back as the generic "China Rose". The curated catalog already carries **110 `Rosa` entries** (9 species + 101 cultivars) with flower colour / habit / description fields. This package re-ranks the user's photo against those 110 candidates with a vision model and, when the photo is distinguishable, replaces the species result with the best-matching cultivars. When it can't tell, it leaves the species result untouched.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Given the uploaded image + a settled species-level identify result whose **genus is `Rosa`**, re-rank against the 110 embedded `Rosa` catalog candidates and return up to **3** best-matching cultivars (or "can't tell").
- Build the static **rose candidate index** once at startup from `ContentIndex.fullPlantByID` (entries where `genus == "Rosa"`). Each candidate = `{plantId, scientificName, commonName, flowerColor[], description}` — the visual-discriminative subset.
- Own the **pure decision logic** (`Decide`) + the candidate/result **types** (`RoseCandidate` / `RoseRerankResult` / `RoseMatch`) in a `rosererank` package that imports **nothing** from `proxy` (dependency inversion — §1.5, resolves the import cycle Codex flagged). The vision IO (`VisionClient.RerankRose`: `gpt-4o` + strict `json_schema`, 18 s client) and candidate building live in the `proxy` package, which imports `rosererank` one-way.
- Decide rewrite-vs-fallback in this order: **validate ids → certainty → confidence floor on the first *surviving* candidate** (§2.4). Rewrite `result.Suggestions` only when a real cultivar is distinguishable; otherwise leave the species result unchanged.
- Be **best-effort**: any failure (vision error / timeout / invalid JSON / empty matches) falls back to the original species result. Rose rerank **never** makes `/v1/identify` fail.

### 1.2 What this package is NOT responsible for

- **Identification.** The species genus is already settled by the parent cascade (`proxy/handlers.go`). This package only refines a known-`Rosa` result; it never calls PlantNet / Plant.id / iNaturalist.
- **A client opt-in.** Rose rerank is **ON by default** for every Rosa identify; clients send nothing. The only gate is a **server kill-switch** `ROSE_RERANK_ENABLED` (secrets.env, defaults true) for cost/quality emergencies — no client release needed. (History: the V1 rollout was first header-gated `X-YM-Rose-Rerank` + an iOS DEBUG toggle for testing, then flipped to default-on after real-photo validation — §3 / §6.)
- **A new response field / "AI-guessed" marker.** V1 MVP fills the existing `suggestions[0..2]` contract unchanged; iOS needs **zero** display changes. The visible "possibly XX" cultivar marker is V1.1 (§6).
- **Other genera.** Hydrangea / Tulipa / Camellia / Hosta etc. are the same pattern but explicitly out of scope (§6). V1 implements `Rosa` only — no premature genus abstraction.
- **Image-based similarity.** No per-cultivar reference photos / embeddings. Candidates are described in **text** (colour + habit + description); only the *user's* photo is sent to the vision model (§5).
- **Mutating the catalog.** Candidates are read-only views over `fullPlantByID`.

### 1.3 Inputs

| Source | Input |
|---|---|
| In-process call from `handlers.go` | `image []byte`, `mime string` (the already-validated upload), the settled `*IdentifyResult`, and the `roseEnabled` kill-switch state. |
| Server config | `ROSE_RERANK_ENABLED` (secrets.env, default true) → `roseEnabled` bool. `false` disables rose rerank server-wide. |
| Startup | `ContentIndex.fullPlantByID` (already built in `content.go`) — source of the 110 `Rosa` candidates. |
| Server config | `OPENAI_API_KEY` from `secrets.Vault` (already loaded; same key as identify tier-3 + enrichment). |

Genus gate: the result's `suggestions[0].ScientificName` first token must equal **`Rosa` exactly** (case-insensitive) — an exact genus-token match, **not** a substring. Substring matching would wrongly fire on `Hibiscus rosa-sinensis`, `Anemone nemorosa`, `Pinus ponderosa` (real catalog false-positives — §7). Use `speciesBinomial(...)` (content.go:415) then split on first space.

### 1.4 Outputs

| Function | Output | Behaviour |
|---|---|---|
| `RerankRose(ctx, image, mime, candidates)` | `RoseRerankResult{ CultivarCertain bool, Matches []RoseMatch }` or typed error | `RoseMatch{ PlantID string, Confidence float64, Reason string }`, ≤3, descending confidence. |
| Handler-side apply | mutates `result.Suggestions` in place + sets `result.AIEnhancedAt` | only when the result is *applied* (not on fallback). |

When applied: `result.Suggestions` is rewritten to the matched cultivars (each `ScientificName` = candidate's `Rosa 'Cultivar'`, `Name` = common name, `Confidence` = model confidence). Downstream `resolvePlantID` + common-name override (handlers.go:549+) run **unchanged** — a `Rosa 'About Face'` scientific name resolves cleanly to its `AAA####` plantId and curated common name, so no special-casing downstream. `AIEnhancedAt` is set (AI influenced the result), matching the existing `RerankIdentify` convention.

### 1.5 External dependencies

- **`proxy.VisionClient`** — reuse the **18 s `identifyHTTP` client** (openai_vision.go:80, `visionIdentifyClientTimeout`), NOT the 8 s shared client: 110 text candidates + one image + strict `json_schema` output is closer to `IdentifyPlant`'s latency profile than `RerankIdentify`'s. The method is `VisionClient.RerankRose` in the **`proxy`** package (beside `RerankIdentify` in openai_vision.go), reusing the `post(...)` / `dataURL(...)` plumbing. **Do not reuse `RerankIdentify`** (different shape, wrong client, returns a single string — §7). `RerankRose` shares identify's request `ctx` (already partly consumed by the cascade) and derives `context.WithTimeout(ctx, roseRerankTimeout = 18 s)`, so its effective budget = `roseBudget = min(18 s, ctx remaining, WriteTimeout wall clock from reqStart)` — bounded by both identify's `ctx` and the 35 s WriteTimeout (§7 #6); the handler's budget gate (§2.1 #4) skips it outright when `< minRoseBudget`.
- **`proxy.ContentIndex`** — `fullPlantByID` (content.go:69) for the candidate index; `resolvePlantID` / `LookupCommonName` run later in the existing handler flow (not called from this package).
- **`gpt-4o-2024-08-06`** (`defaultVisionModel`) — vision-capable; cultivar disambiguation needs the image. (Enrichment uses `mini`; that is text-only and irrelevant here.)
- **Import boundary (no cycle).** `rosererank` holds only plain types + the pure `Decide` function and imports **nothing** from `proxy`. `proxy` imports `rosererank` one-way — `VisionClient.RerankRose` (vision IO), `buildRoseCandidates` (reads `proxy.PlantDetail`), and the handler rewrite all sit in `proxy`. This inverts the naive `rosererank → proxy.VisionClient` + `proxy.handlers → rosererank` cycle (Codex #44 P2).
- Standard library only otherwise (json / context / time / strings).

---

## 2. Trigger + flow (in-line in `/v1/identify`, not an endpoint)

### 2.1 Trigger conditions (ALL must hold)

1. `roseEnabled` — the server kill-switch is on (`ROSE_RERANK_ENABLED` defaults true; set false to disable server-wide), AND
2. identify cascade succeeded — `!unknownSentinel`, `vision != nil`, `len(result.Suggestions) > 0`, AND
3. `genusOf(result.Suggestions[0].ScientificName) == "Rosa"` (exact token match — §1.3), AND
4. **enough budget left** — `roseBudget(ctx, reqStart) ≥ minRoseBudget` (~6 s), where `roseBudget = min(18 s, ctx remaining, roseWallClockBudget − since(reqStart))`. Bounded by BOTH identify's `ctx` AND the WriteTimeout wall clock from request start: a slow cascade eats `ctx`; a slow upload + fast cascade leaves `ctx` but little wall clock — either way skip rather than risk a timeout or WriteTimeout overrun (Codex #44 P2 — budget).

Any false → skip; return the original result verbatim.

### 2.2 Candidate index (built once at startup)

```
roseCandidates = sort_by_plantId(
    [ RoseCandidate{plantId, scientificName, commonName, flowerColor, description}
      for entry in fullPlantByID if entry.genus == "Rosa" ] )   // ~110, deterministic order
```

`description` is truncated to ~30 words (the catalog `description` already encodes habit + colour + distinguishing trait, e.g. *"a bicolor grandiflora rose with reverse-colored petals, bright orange on the back, yellow face"*). Built once; **never** scanned per-request. **Sorted by plantId** for a deterministic prompt order — `fullPlantByID` is a Go map (random iteration), so an unsorted slice would shuffle the 110-row prompt across process starts, hurting reproducibility and OpenAI prompt-cache hit rate (Codex #44 P2).

### 2.3 Rerank call (prompt I/O)

- **User content**: the photo as a base64 data URL (`dataURL(mime, image)`) + the candidate list serialized compactly:
  `[{"id":"AAA1136","name":"About Face","colors":["orange","yellow"],"desc":"…"}, …]` (110 rows).
- **System instruction** (English-only output, per `app_language`):
  > You are a rose-cultivar expert. From the candidate list, pick the cultivars whose described flower colour / form (grandiflora, floribunda, climber, …) / petal shape / habit best match the photo. **First decide whether the photo even has enough distinguishing features** — flower colour combination, bloom form, petal count, plant habit. If many red double roses would look identical, or the photo is unclear, set `cultivar_certain:false` and return no matches. Only give high confidence when the visible traits genuinely single out a cultivar. Return at most 3, most-likely first. `reason` ≤ 15 words, English.
- **Output** (`response_format: {type:"json_schema", strict:true}`, `additionalProperties:false`):
  ```json
  { "cultivar_certain": true,
    "matches": [ {"plant_id":"AAA1136","confidence":0.78,"reason":"bicolor orange-back/yellow-face grandiflora"} ] }
  ```
  `matches` maxItems 3; strict mode keeps `required == properties`.

### 2.4 Decision: rewrite vs fall back

```
res, err := vision.RerankRose(...)
if err != nil           → fall back (log, keep species result)   // best-effort
if !res.CultivarCertain → fall back (model says indistinguishable)
// VALIDATE BEFORE THE FLOOR — a hallucinated high-confidence first id must not
// shield a low-confidence real candidate behind it (Codex #44 P2):
survivors := dedup([ m in res.Matches : m.PlantID is a known rose candidate id ])   // drop hallucinated + duplicate ids, keep first occurrence
if len(survivors) == 0            → fall back
if survivors[0].Confidence < 0.35 → fall back   // floor on the FIRST SURVIVING real candidate
rewrite result.Suggestions from survivors (≤3), set result.AIEnhancedAt
```

The **primary** gate is `cultivar_certain` (the model's own "can I distinguish?" judgement) plus catalog membership; the `0.35` floor is a defensive backstop for the self-contradiction case (certain=true yet trivially low confidence), **not** a tuned threshold — model self-reported confidence is uncalibrated, so we don't lean on a magic number (resolved discussion).

### 2.5 Where it hooks in `handlers.go`

After the existing optional `RerankIdentify` block (handlers.go:~529–545, the `aiEnhance` path) and **before** the plant_id resolution loop (handlers.go:~549). At that point `result.Suggestions[0]` is the final species; rose rerank may replace the slice; the downstream resolution + common-name override then run normally over whatever is in the slice. The two paths are independent: `aiEnhance` (multipart `ai_enhance`) refines *species*; `roseEnabled` (default-on, `ROSE_RERANK_ENABLED` kill-switch) controls the *cultivar* rerank.

---

## 3. Feature flag

- **Default ON.** Rose rerank runs for every identify whose genus is Rosa — no client opt-in, no request header. The identify body/header contract is unchanged.
- **Kill-switch**: `ROSE_RERANK_ENABLED` in secrets.env, read once at startup via `vault.GetBool("ROSE_RERANK_ENABLED", true)` and passed to `HandleIdentify` as `roseEnabled`. Missing/empty = ON. Set `=false` + restart to disable server-wide (cost/quality emergency) without any client release. It is **not** in deploy.sh's required-keys gate (optional key).
- **History (V1 rollout)**: first shipped header-gated (`X-YM-Rose-Rerank`, server) + a DEBUG toggle (iOS More tab, `@AppStorage`, no `EnvironmentObject` per the #325 crash) for safe real-photo testing. After validation, both were removed — the header read in `handlers.go` and the iOS toggle/header — and the default flipped to on (§6).

---

## 4. Error handling — best-effort, never blocks identify

Rose rerank is a pure enhancement wrapped so it can only **improve or no-op**, never degrade:

| Failure | Result |
|---|---|
| Vision 5xx / timeout (18 s) | log, fall back to species result, identify returns 200 |
| Invalid / non-strict JSON | log, fall back |
| `cultivar_certain:false` / empty / all ids unknown / below floor | fall back (the designed "can't tell" path, not an error) |
| Panic inside rerank | `recover()` → fall back |

Rose rerank does **not** add new error codes to parent SPEC §3 and never converts a successful identify into a 4xx/5xx. Upstream OpenAI error bodies are never surfaced (parent SPEC §5).

---

## 5. Resolved decisions (don't re-debate)

- **Default-ON with a server kill-switch (`ROSE_RERANK_ENABLED`).** Validated on real photos during a header-gated test phase, then flipped to default-on. The kill-switch is server-controlled (not a client flag) so the emergency off needs no client release.
- **`top-3`, not `top-1`.** Multiple candidates honestly convey uncertainty, fill the existing ≤3 `suggestions` contract, and need zero iOS display change.
- **Primary gate = model `cultivar_certain` bool, not a numeric threshold.** Self-reported vision confidence is uncalibrated/overconfident; we ask the model the binary "can the photo distinguish a cultivar?" and only keep a `0.35` floor as a defensive backstop.
- **Candidate set = all 110 `Rosa` (9 species + 101 cultivars).** Including the 9 species lets the model legitimately "stay at species" (pick `Rosa rugosa`) for a wild/uncertain photo instead of being forced onto a cultivar.
- **Text candidates + single user image — no per-cultivar reference photos.** The catalog has no clean canonical per-cultivar image, and 110-image comparison is slow + costly; `flower_color` + `description` carry enough discriminative signal.
- **`gpt-4o` (vision), 18 s `identifyHTTP` client.** Cultivar disambiguation needs the image; the candidate-heavy strict-JSON call matches `IdentifyPlant`'s latency, not `RerankIdentify`'s 8 s.
- **`temperature: 0` + fixed `seed` (best-effort stable, NOT fully deterministic).** Reduces the `cultivar_certain` flip-flop seen on a boundary `Rosa chinensis` photo during default-on smoke. OpenAI Chat Completions is **not** bit-identical even at temperature 0 — `seed` + a stable `system_fingerprint` is documented as best-effort, not a guarantee (Codex #47 P2) — so repeats are *substantially more consistent*, not identical. Implemented via new `*float64 Temperature` + `*int Seed` fields on `openAIChatRequest` (both omitempty → only `RerankRose` sets them; `RerankIdentify`/`IdentifyPlant` unchanged).
- **No new response field (MVP).** Reuse `suggestions`. The visible "AI-guessed cultivar" marker is V1.1.
- **Best-effort.** Rose rerank failure is invisible to the client — it degrades to the species result, never a 5xx.
- **Downstream reuse, no special-casing.** Rewritten `Rosa 'Cultivar'` scientific names flow through the existing `resolvePlantID` + common-name override unchanged.
- **Genus match is exact-token, never substring** — avoids the `Hibiscus rosa-sinensis` / `Anemone nemorosa` false-positives (§7).
- **All model text (`reason`) is English** per `app_language`.

---

## 6. Out-of-scope (V1.1+ candidates)

- ~~**Default-on.**~~ **DONE** — header gate + iOS toggle removed; rose rerank is on by default for all clients, gated only by the `ROSE_RERANK_ENABLED` server kill-switch.
- **Visible "possibly / best match" marker.** Add a response field (e.g. `match_kind: "cultivar_guess"`) + iOS "possibly XX" phrasing so the guess reads as a guess. Requires an iOS PR + contract bump.
- **Other cultivar-heavy genera** (Hydrangea, Tulipa, Camellia, Hosta, Iris …). Same machine; keep trigger-genus + candidate-source as the only genus-specific knobs so adding a genus = config + candidate data, not a rewrite. Still: V1 implements `Rosa` only.
- **Image-based similarity** (reference photos / embeddings per cultivar) if text descriptions prove too weak.
- **Confidence calibration / telemetry** on accept-vs-fallback rates to tune the floor and the prompt.

---

## 7. Pitfalls (don't re-rediscover)

1. **Genus gate must be exact-token, not substring.** `strings.Contains(sci, "rosa")` wrongly fires on `Hibiscus rosa-sinensis`, `Anemone nemorosa`, `Drosanthemum`, `Pinus ponderosa` — all real catalog entries. Match `genusOf(sci) == "Rosa"` (first token of the binomial, case-insensitive).
2. **Best-effort or bust.** Wrap the whole step so a vision failure/timeout/panic can only fall back. A rose rerank bug must never turn a working identify into a 5xx. Cover with a test that injects a failing vision stub and asserts the species result survives with 200.
3. **Do NOT reuse `RerankIdentify`.** It returns a single best-guess *species* string via the 8 s client and takes `[]Suggestion`. Rose rerank needs a list result, the 18 s client, and `[]RoseCandidate`. New method, parent SPEC §1.2 boundary (mirrors enrichment pitfall §9 #11).
4. **Validate `plant_id` against the candidate index before applying.** The model can hallucinate an id; drop unknown ids, and if none survive, fall back. Never emit a `plant_id` that isn't a real rose candidate.
5. **Candidate index is built once at startup, never per-request.** Iterating `fullPlantByID` (1522 entries) on every identify is needless work; build `roseCandidates` in the constructor.
6. **Budget-aware on TWO axes: identify's `ctx` AND the WriteTimeout wall clock.** `ctx` is created at handlers.go:265 (`WithTimeout(r.Context(), 30 s)`) AFTER the body is read. Two distinct failure modes: (i) a slow cascade eats most of `ctx`; (ii) a slow upload + *fast* cascade leaves `ctx` nearly full but little WriteTimeout wall clock — and rose rerank's extra ≤18 s IS extra wall clock here (the cascade did not use it), so it can push the response past the 35 s WriteTimeout (Codex #44 P2). The handler therefore computes `budget := roseBudget(ctx, reqStart) = min(18 s, ctx remaining, roseWallClockBudget(30 s) − since(reqStart))`, skips when `< minRoseBudget` (~6 s), else runs `RerankRose` under `WithTimeout(ctx, budget)`; a timeout falls back (§4). Gating on `ctx` ALONE is insufficient (ignores upload time); an independent `context.Background()` budget is wrong too (ignores `ctx`). `reqStart` is captured at handler entry ≈ the WriteTimeout start (Go resets WriteTimeout when the request header is read).
7. **`AIEnhancedAt` only on apply.** Set it when suggestions are actually rewritten; on fallback leave it as the cascade left it (parent contract: non-null iff AI influenced the result).
8. **`reason` / any model text is English-only** (`app_language`) — enforce in the system prompt + per-field schema description.
9. **strict `json_schema` rejects extra fields** — `additionalProperties:false`, `required == properties`, `matches.maxItems = 3`.
10. **No import cycle — depend one-way.** Naively `rosererank` would import `proxy` for `VisionClient` while `proxy/handlers.go` imports `rosererank` → Go import cycle (Codex #44 P2). Keep `rosererank` free of any `proxy` import (plain types + pure `Decide`); put `VisionClient.RerankRose`, `buildRoseCandidates` (it reads `proxy.PlantDetail`), and the suggestion rewrite in `proxy`. Direction: `proxy → rosererank` only.
11. **Validate ids BEFORE the confidence floor.** Applying `matches[0].confidence < floor` before dropping hallucinated ids lets a hallucinated high-confidence first id shield a low-confidence real candidate that then gets applied below the floor (Codex #44 P2). Drop unknown ids first, then floor `survivors[0]`. Covered by `decide_test.go`.

---

## 8. Implementation outline (not part of the contract)

```
proxy/rosererank/            package rosererank — imports NOTHING from proxy (dependency inversion)
├── SPEC.md                 (this file)
├── types.go                RoseCandidate / RoseRerankResult / RoseMatch (plain data)
├── decide.go               Decide(res, candidateIDs) ([]RoseMatch, bool) — pure: drop hallucinated ids → certainty → floor on first survivor
└── decide_test.go          hallucinated-first-id-shields-low-real (Codex #44 P2) · !certain · empty · all-unknown · below-floor

proxy/                       package proxy — imports rosererank one-way
├── rose_vision.go          VisionClient.RerankRose(...) -> rosererank.RoseRerankResult (gpt-4o · strict json_schema · 18 s identifyHTTP · dataURL) — beside RerankIdentify
├── rose_candidates.go      buildRoseCandidates (genus=="Rosa", sorted by plantId) + genusOf + roseBudget(ctx,reqStart) + rewriteSuggestionsFromRose + id-set/by-id helpers
└── rose_vision_test.go     mock-HTTP: schema · parse · timeout→error
```

Parent-package changes (in `proxy/`, not this package):

```go
// HandleIdentify factory body builds candidates ONCE (startup), captured by the closure:
//   roseCands := buildRoseCandidates(content); roseIDs := roseIDSet(roseCands); roseMap := roseByID(roseCands)
// Handler body captures reqStart := time.Now() (WriteTimeout wall-clock start).
// After the RerankIdentify block (~547), before plant_id resolution (~549):
if roseEnabled && !unknownSentinel && vision != nil &&
   len(result.Suggestions) > 0 && len(roseCands) > 0 &&
   genusOf(result.Suggestions[0].ScientificName) == "Rosa" {
    if budget := roseBudget(ctx, reqStart); budget >= minRoseBudget {  // min(18s, ctx, wall clock)
        rctx, cancel := context.WithTimeout(ctx, budget)
        res, err := vision.RerankRose(rctx, imgBytes, mime, roseCands)
        cancel()
        if err == nil {
            if matches, ok := rosererank.Decide(res, roseIDs); ok {
                rewriteSuggestionsFromRose(result, matches, roseMap)  // sets AIEnhancedAt
            }
        } // err / timeout / !ok → best-effort fall back; species result untouched
    }
}
// roseBudget(ctx, reqStart) = min(roseRerankTimeout 18 s, ctx remaining, roseWallClockBudget 30 s − since(reqStart))
```

- `roseCandidates` (`[]rosererank.RoseCandidate`) + derived `roseCandidateIDs` (set) + `roseByID` (map) built once at startup near `LoadContent()` (main.go), threaded into the handler closure alongside `content`/`vision`.
- `genusOf` is a tiny helper (first token of `speciesBinomial`), exported from `proxy` or duplicated trivially.
- No `secrets.Vault` additions (`OPENAI_API_KEY` already present). No new route, no rate-limit change (inherits identify's two-layer limit).

Estimated effort: ~1 day implementation + 0.5 day tests + 0.5 day deploy/smoke. iOS DEBUG toggle (separate `yardmate-swiftui` PR): ~0.5 day.
