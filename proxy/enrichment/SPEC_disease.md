# `proxy/enrichment` — disease enrichment (V1)

> Status: **draft v2 — SPEC for review; implementation in follow-up PRs.**
> Companion: `proxy/enrichment/SPEC.md` (plant detail enrichment — same module, shared Supabase / cache / OpenAI plumbing) + parent `proxy/SPEC.md §2.2` (`/v1/diagnose`, whose contract this extends).
> Background: When `/v1/diagnose` decides a disease whose name has **no equivalent** in the curated catalog (`diseases.json`: 70 entries, prefixes L/R/ST/FL/FR/P), iOS today shows a slim, image-less detail (empty Treatment — the "Drought Stress" report that prompted this). disease enrichment instead:
> 1. tries hard to **map** the name to an existing catalog entry — an equivalent like "overwatering" → L08 must NOT spawn a duplicate;
> 2. only when there is **genuinely no equivalent**, generates catalog-quality structured content via an LLM that **references the curated `shared.steps` / `shared.remedies` pools — the SAME human-reviewed steps + images as in-catalog diseases**;
> 3. assigns the generated disease a new **`O` (Other) catalog id**, stores it in Supabase `diseases_pending` keyed by normalized name, and reuses it for everyone who later hits the same disease.
>
> Hero image stays the user's own captured photo (client-side). The point of the table: **never re-generate the same disease twice**, and let `O` rows graduate into the curated catalog later.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md §6`)

### 1.1 What this is responsible for

- Inside `/v1/diagnose` (**NOT a new endpoint**), when `mapCatalogID` finds **no existing catalog equivalent** for an issue (exact + alias + LLM-disambiguation all miss / model answered NONE), enrich it inline before the 200:
  1. **Process LRU cache** (key = normalized disease name) → return.
  2. **Supabase `diseases_pending` hit** (PK = normalized name, `status IN ('pending','approved')`) → return stored detail + its `O` id.
  3. **Miss** → `gpt-4o-mini` (json_schema strict) generate → **assign next `O` id** → `INSERT ... ON CONFLICT DO NOTHING` → return.
- LLM outputs **ids only**: prose (`shortDescription` / `symptomAnalysis` / `cause`) + treatment / prevention group skeletons whose `stepRefs` are **enum-constrained to S01–S44** + `homeRemedyRefs` **enum-constrained to K01–K15**. No bodies / titles / images from the LLM.
- **Back-fill** each S-id/K-id → `{num,title,body,image}` / `{title,recipe,usage,image}` from the curated pools (denormalized — identical to how in-catalog diseases store steps).
- **Parse `shared.steps` (S01–S44) + `shared.remedies` (K01–K15) into `ContentIndex`** (currently unparsed; only 7 slim disease fields are) with an id→detail lookup.
- Assign the generated disease an **`O` (Other) catalog id** and return it as `catalogId` (**NOT null**) so client / history / cache / Dashboard can all reference it by id.

### 1.2 What this is NOT responsible for

- **Identification / the disease decision** (upstream Plant.id / GPT-4o vision). No image upload of its own.
- **The hero image** — always the user's captured photo, supplied client-side. No R2 write, no image generation.
- **In-catalog diseases** (L/R/ST/FL/FR/P) — render from CDN `diseases.json` by catalogId (parent SPEC), untouched.
- **Mapping equivalents** — that's #50 (alias + LLM disambiguation). If a name has an existing equivalent (overwatering → L08), it maps there; enrichment does **not** fire and does **not** mint an O id. Enrichment is strictly the "no equivalent exists" tail.
- **Healthy / non-plant** — no out-of-catalog disease name exists → enrichment never fires (#49 "never healthy" force-pick stays an in-catalog path, untouched).
- **Admin review UI** — V1 review = Dashboard editing (same as plant enrichment).
- **Stampede coalescing** — concurrent first-callers each spend one LLM call; `ON CONFLICT DO NOTHING` keeps one row + one O id (the loser's minted O number is discarded — gaps are fine).

### 1.3 Inputs

| Source | Input |
|---|---|
| Within `HandleDiagnose` | out-of-catalog `issue.Name` + `IdentifiedName` / `PlantID` (prompt context only); optional `lang` display-language hint (from the `/v1/diagnose` multipart `lang` form field) |
| Curated pools | `shared.steps` (S01–S44), `shared.remedies` (K01–K15) |
| Config | `OPENAI_API_KEY` + `SUPABASE_DB_URL` (same secrets as plant enrichment) |

- `lang` — optional. Normalized (`NormalizeLang`, **shared with plant enrichment**) to the nearest of the 11 iOS `Localizable.xcstrings` codes: `en`, `de`, `es`, `fr`, `it`, `ja`, `ko`, `pt`, `vi`, `zh-Hans`, `zh-Hant`. Region subtags drop for single-variant languages (`pt-BR`→`pt`); **Chinese script is preserved** (`zh-CN`→`zh-Hans`; `zh-TW`/`zh-HK`→`zh-Hant`; bare `zh`→`zh-Hans`). Absent / empty / unsupported → `en`. Second component of the composite key `(disease_name_normalized, lang)` for both the in-process cache and Supabase (§6 multi-language).

### 1.4 Outputs

| Case | Output |
|---|---|
| Enriched | `HealthIssue` with **`catalogId` = O id** (e.g. `"O1"`), `structuredDetail` filled, `isFallback=true` |
| Generation unavailable (error / timeout / budget / DB-down-and-uncacheable) | degrade to **slim issue: `catalogId = null` + legacy `Treatment` text**. Never 502. |

Wire shape is identical regardless of cache / DB / LLM / language path. Prose is returned in the requested `lang` (best-available, English-fallback per §6); non-prose (ids / enums) stays canonical (§3).

### 1.5 External dependencies

- **Supabase Postgres** — pgx pool (shared `SUPABASE_DB_URL`). New table `diseases_pending` + an O-id sequence (§5).
- **OpenAI `gpt-4o-mini`** — shared `proxy.VisionClient.post(...)`.

---

## 2. Trigger + cascade

`mapCatalogID` (post-#50) returns an **existing** catalog id or nil; **enrichment runs in `HandleDiagnose` after nil**, and on success the issue's catalogId becomes an `O` id:

1. exact catalog-name match → existing id
2. alias table (**true equivalents only** — overwatering→L08, botrytis→L23, sooty mold→L21) → existing id
3. LLM disambiguation w/ descriptions (**may answer NONE**) → existing id
4. **nil (no equivalent)** → disease enrichment: generate → **assign O id** → `catalogId = O id`, `structuredDetail` filled
5. generation unavailable → **slim issue** (`catalogId = null`, legacy `Treatment`)

Equivalents map at step 2/3; only genuinely-new diseases reach step 4 and mint an O id. **drought stress**: leaning step 4 → O (symptoms broader than L07 "Underwatering yellowing", and the name is preserved); tunable via alias/LLM + Dashboard. Healthy / non-plant never reach step 4.

### Budget (critical — diagnose has a hard WriteTimeout)

`/v1/diagnose` runs under a 35s WriteTimeout; Plant.id + optional vision fallback already spend budget. Inline enrichment adds **one** `gpt-4o-mini` call.

- Budget the LLM on **wall-clock from `reqStart`** (mirror `diagnoseFallbackBudget`). Too little left → skip → slim issue.
- cache / DB lookup is ~ms → always attempted.
- **Only the FIRST out-of-catalog issue per diagnose is enriched** (decision). Remaining out-of-catalog issues stay slim.

---

## 3. Generation contract (AI picks ids; backend back-fills)

`gpt-4o-mini`, `json_schema` strict, with **ref values enum-constrained** so the model structurally cannot emit an invalid id:

```jsonc
{
  "name": "Drought Stress",          // echo input name (preserve, NOT remapped)
  "shortDescription": "...",         // 15–40 words (slim, mirror plant enrichment v2)
  "symptomAnalysis": "...", "cause": "...",
  "treatment":  { "groups": [ { "label": "For mild cases", "stepRefs": ["S04","S12"] } ] },
  "homeRemedyRefs": ["K15"],
  "prevention": { "groups": [ { "label": null, "stepRefs": ["S20"] } ] }
}
```

- `stepRefs` items: JSON-schema `enum: [S01..S44]`; `homeRemedyRefs`: `enum: [K01..K15]`. The prompt lists each id with its title so the model chooses meaningfully (mirror `common_diseases_list` in plant enrichment).
- **enum makes an "all-invalid refs" outcome structurally impossible**; the only residual is the model returning an **empty** array (it judged no step fits) → keep prose with empty steps.
- Back-fill ids → `{num (group-local), title, body, image}` / `{title, recipe, usage, image}`. **Defense-in-depth: still whitelist server-side** in case enum is relaxed; drop unknown → drop emptied group.
- `max_tokens` ~800 (slim output keeps it inside the diagnose budget).
- **Lang-aware (`buildDiseaseSchema(…, lang)`, `DiseasePromptVersion` v2).** For non-English `lang`, the strict `json_schema` `description` of each **prose** field (`shortDescription`, `symptomAnalysis`, `cause`, each group `label`) is suffixed `"Write this field in <Language>…"`, and `diseaseSystemPrompt(lang)` carries the same rule. The schema description is what actually drives localization under strict mode (the `#59` lesson from plant enrichment — a one-line system prompt alone is not enough). Refs / enums / `name` (echo) stay canonical English.

---

## 4. Wire contract (`HealthIssue`)

Additive, back-compat — legacy `Treatment` kept:

```go
type HealthIssue struct {
    // ...existing fields...
    CatalogID        *string                  `json:"catalogId"`        // existing id (L/R/..) OR new "O…" OR null (slim fallback)
    Treatment        Treatment                `json:"treatment"`        // legacy 3×[]string, unchanged
    StructuredDetail *StructuredDiseaseDetail `json:"structuredDetail,omitempty"` // present for O-series enriched issues
}
// StructuredDiseaseDetail{ ShortDescription, SymptomAnalysis, Cause string; Treatment, Prevention DiseaseStepGroups; HomeRemedies []DiseaseRemedy }
// DiseaseStepGroups{ Groups []DiseaseStepGroup }; DiseaseStepGroup{ Label *string (null=ungrouped, matches diseases.json); Steps []DiseaseStep }
// DiseaseStep{ Num int; Title, Body, Image, Ref string; SubSteps []DiseaseStep }; DiseaseRemedy{ Ref, Title, Recipe, Usage, Image string }
```

**iOS dispatches by `catalogId`:**
- prefix `O` → render `structuredDetail` (backend-supplied steps + images)
- `L / R / ST / FL / FR / P` → read CDN `diseases.json` by id (**existing logic, unchanged**)
- `null` → if `structuredDetail` is present (DB-down generated, no O id minted), render it; else slim (legacy `Treatment` text)

`image` is a **filename** (e.g. `"uoIzi.png"`); iOS applies the **existing CDN-prefix rule** it already uses for in-catalog step images — same asset source.

---

## 5. Supabase schema (`diseases_pending`)

```sql
CREATE SEQUENCE diseases_other_seq;             -- mints O numbers
CREATE TABLE diseases_pending (
  disease_name_normalized TEXT NOT NULL,        -- normalizeDiseaseName(name); name-keyed, plant-agnostic
  lang                    TEXT NOT NULL DEFAULT 'en', -- supported code (en/de/.../zh-Hans/zh-Hant); added in 004
  catalog_id              TEXT NOT NULL,         -- 'O' || nextval; SHARED across a disease's language rows (004 dropped UNIQUE)
  disease_name            TEXT NOT NULL,
  data                    JSONB NOT NULL,        -- StructuredDiseaseDetail, bodies/images ALREADY back-filled (frozen)
  status                  TEXT NOT NULL DEFAULT 'pending',  -- pending|approved|rejected
  source                  TEXT, source_version TEXT, generation_request_id TEXT,
  created_at              TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (disease_name_normalized, lang)    -- composite (004); one row per (disease, language)
);
-- lookup (exact):   WHERE disease_name_normalized=$1 AND lang=$2 AND status IN ('pending','approved')  (approved preferred)
-- lookupAny (race): WHERE disease_name_normalized=$1 AND status IN ('pending','approved')
--                   ORDER BY (status='approved') DESC, (lang='en') DESC  -- any-language master, prefers approved then en
-- write:  catalog_id := COALESCE(passed-through master O id, 'O' || nextval('diseases_other_seq'));
--         INSERT ... (lang, catalog_id, ...) ON CONFLICT (disease_name_normalized, lang) DO NOTHING; race winner re-queried
--         (master mints the O id; translated rows REUSE it; loser's minted number discarded — gaps OK)
```

- `data` stores **back-filled (denormalized)** detail → self-contained row, no re-back-fill on read. Trade-off: if `shared.steps` later changes, old rows keep old copies until regenerated — acceptable (mirrors `diseases.json` denormalization).
- O id is **stable per disease and SHARED across that disease's language rows** — the master row mints it, translated rows reuse it (iOS keys its detail page on `catalog_id`, so it must be byte-identical across languages). 004 dropped the global `UNIQUE` on `catalog_id` to allow this; it stays unique *per disease* (one O id per normalized name). Graduating an approved row into curated `diseases.json` later may keep or remap the id (V1.1, §9).
- Reuses the same pgx pool / `SUPABASE_DB_URL` as plant enrichment.

---

## 6. Errors / timeout / fallback

- LLM error / timeout / budget-skip → **slim issue** (`catalogId=null`, legacy `Treatment`), 200 (never 502).
- **DB unavailable → still generate & return (experience-first), but DON'T persist.** Your question answered: when DB recovers, the next caller generates + persists the durable row. **No data conflict** — `ON CONFLICT DO NOTHING` means the first post-recovery writer wins one row + one id; later INSERTs are ignored; nothing is overwritten. The only costs are (a) repeat LLM spend during the outage, (b) the during-outage user got an unsaved version that may differ slightly (LLM nondeterminism) from the eventually-persisted one. (Open: what `catalogId` the transient response carries — see §8.)
- All refs empty (enum makes "invalid" impossible; only "model chose none") → keep prose, empty steps.

### Multi-language fallback chain (mirrors plant `SPEC.md §7`)

English-pivoted, one-master-per-disease. `GetOrGenerate(ctx, name, plantContext, lang)`:

0. `lang := NormalizeLang(lang)` (unsupported / empty → `en`).
1. in-process cache `(normalized|lang)` or Supabase exact `(normalized, lang)` hit → return it.
2. exact `lang` missing and `lang != en` → try the English row → return it (**English fallback**; not re-cached under the `lang` key).
3. **one-master invariant** — a master exists in ANOTHER language but neither exact `lang` nor English yet (racing the backfill): `LookupDiseaseAny(normalized)` (prefers approved, then `en`) → `DiseaseTranslate` it into `lang` + INSERT (**reuse the master's O id**, `…-translated` source) rather than minting a 2nd independent master that could carry a different O id.
4. nothing yet → generate the master **in `lang`** → INSERT (mints the O id) → `enqueue` async backfill (**English FIRST**, then the rest; translate-only-prose; `ON CONFLICT DO NOTHING`).
- **Only prose is localized**: `shortDescription` / `symptomAnalysis` / `cause` + each treatment/prevention group `label`. `name` (echo), step/remedy ref ids, and the offline `shared.steps` / `shared.remedies` step+remedy text stay canonical (iOS localizes catalog text via its own offline pipeline). `DiseaseTranslate` copies every non-prose field byte-for-byte.
- DB-down (§6 bullet 2) still applies per-language: generate + return in `lang`, don't persist.

---

## 7. Pitfalls (don't re-rediscover)

- **DON'T enrich when an equivalent exists** — it must map at step 2/3; minting an O for "overwatering" duplicates L08.
- **DON'T mint an O id for in-catalog issues** (catalogId already set).
- **DON'T let enrichment block or 502 diagnose** — tail enhancement; always degrade to slim.
- **Back-fill images via the SAME CDN rule** as in-catalog step images, or they 404.
- `normalizeDiseaseName` is the **single SOT** for cache key + DB PK + #50 alias key.
- **enum-constrain refs AND whitelist server-side** (defense in depth).
- **Budget from `reqStart`, not `ctx`** (parent SPEC fallback-budget pitfall).
- Store **back-filled** data (not raw refs) → reads need no pool access.

---

## 8. Resolved decisions

**Locked (per your calls):**
- Synchronous, inline in `/v1/diagnose` ("识别过程多等几秒").
- Cache / PK = normalized disease name, plant-agnostic.
- AI picks step ids (**enum-constrained** S01–S44 / K01–K15); backend back-fills text + image from curated pools.
- **Only the first out-of-catalog issue enriched per diagnose** (budget).
- **Out-of-catalog generated diseases get an `O` (Other) catalog id (not null); equivalents map to existing L/R/.. and do NOT mint O** (overwatering → L08).
- **iOS dispatches by catalogId prefix** (O → structuredDetail; L/R/.. → CDN; null → slim).
- **DB-down → generate-uncached (experience-first); no data conflict on recovery.**
- pending 即上线 + Dashboard 审核; approved O rows can graduate to the curated catalog later.
- **#50** keeps ① descriptions + ② alias (**equivalents only**) + ③ LLM (NONE allowed); drops ④ forced + ⑤ L08.

⚠️ **Minor opens (non-blocking, can settle at implementation):**
1. During a DB outage, does the transient response carry a placeholder id (e.g. `O0`) or `null`? (Leaning: a temp marker since it isn't persisted; iOS renders `structuredDetail` regardless of the id.)
2. All-refs-empty handling: keep prose + empty steps — confirmed.

---

## 9. Out of scope (V1.1+)

- Enriching all 1–3 issues in one diagnose.
- **Graduating approved `O` rows into curated `diseases.json`** (+ optional id remap).
- Stampede coalescing.
- Per-plant disease variants (V1 is name-keyed only).
- Regenerating rows when `shared.steps` changes.

---

## 10. #50 relationship + phasing

- **#50 (`diagnose-catalog-fallback`)** narrows to "mapping-recall boost": keep ① descriptions in the disambiguation prompt + ② alias table (**equivalents only** — overwatering→L08, spelling variants; NOT causal-but-distinct names like drought stress); ③ LLM (NONE allowed). **DROP ④ forced + ⑤ L08.** Step 4 returns nil → slim until enrichment ships. Update `proxy/SPEC.md §2.2` to match (the P1 SPEC drift the #50 review flagged).
- **Phase 1 (backend, this SPEC):** parse `shared.steps`/`shared.remedies` → `ContentIndex`; `diseases_pending` table + O-id sequence + DB layer; prompt (enum) + back-fill; `HealthIssue.StructuredDetail` + `catalogId=O`; wire into `HandleDiagnose` step 4.
- **Phase 2 (iOS, `yardmate-swiftui`):** dispatch by catalogId prefix; render `structuredDetail` (reuse the in-catalog step view) for O-series; hero = captured photo. Doc/SPEC under `app/YardMate/YardMate/Diagnose/`.
