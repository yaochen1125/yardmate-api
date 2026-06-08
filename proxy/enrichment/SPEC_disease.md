# `proxy/enrichment` — disease enrichment (V1)

> Status: **draft — SPEC for review; implementation in follow-up PRs.**
> Companion: `proxy/enrichment/SPEC.md` (plant detail enrichment — same module, shared Supabase / cache / OpenAI plumbing) + parent `proxy/SPEC.md §2.2` (`/v1/diagnose`, whose contract this extends).
> Background: When `/v1/diagnose` produces a disease whose name maps to **no** entry of the 70-disease curated catalog (`proxy/data/diseases.json`), iOS today shows a slim, image-less detail (empty Treatment — see the "Drought Stress" report that prompted this). disease enrichment keeps the **real disease name**, generates catalog-quality structured content via an LLM that **references the curated `shared.steps` / `shared.remedies` pools — so the treatment steps + images are the SAME human-reviewed assets as in-catalog diseases**, stores it in a Supabase `diseases_pending` table keyed by normalized disease name, and reuses it for everyone who later hits the same disease. The hero image stays the user's own captured photo (client-side). The whole point of the table is to **never re-generate the same disease twice**.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md §6`)

### 1.1 What this is responsible for

- Inside `/v1/diagnose` (**NOT a new endpoint**), when `mapCatalogID` returns nil for an issue (exact-name + alias + LLM-disambiguation all miss / model answered NONE → genuinely out-of-catalog), **enrich that issue inline** before the 200:
  1. **Process LRU cache hit** (key = normalized disease name) → return.
  2. **Supabase `diseases_pending` row hit** (PK = normalized disease name, `status IN ('pending','approved')`) → return stored `data` JSONB.
  3. **Miss** → call OpenAI `gpt-4o-mini` (`response_format: json_schema, strict`), `INSERT INTO diseases_pending ... ON CONFLICT DO NOTHING`, return generated detail.
- The LLM outputs **only**: disease prose (`shortDescription`, `symptomAnalysis`, `cause`) + treatment / prevention **group skeletons that reference S-ids** + homeRemedy **K-id refs**. It writes **no** step bodies / titles / images.
- **Back-fill**: the server resolves each S-id → `{num, title, body, image}` from `shared.steps` and each K-id → `{title, recipe, usage, image}` from `shared.remedies`, inlining them (denormalized — byte-identical to how in-catalog diseases store steps).
- **Whitelist** AI-returned S-ids / K-ids against the curated pools; silently drop unknown ids (mirror `enrichment.filterCatalogDiseaseIDs`).
- **Parse `shared.steps` (S01–S44) + `shared.remedies` (K01–K15) into `ContentIndex`** with an id→detail lookup — these are currently NOT parsed by the backend (only 7 slim disease fields are; see `content.go:77-90`).
- Carry the enriched content back on the existing `HealthIssue` via a new optional structured field. **`catalogId` stays null** (honest out-of-catalog marker).

### 1.2 What this is NOT responsible for

- **Identification / the disease decision.** Plant.id / GPT-4o vision upstream decide *which* disease; enrichment only fills *detail* for an already-decided out-of-catalog name. No image upload of its own.
- **The hero image.** Always the user's captured photo, supplied client-side. Enrichment is text/refs in, structured-text/refs out — no R2 write, no image generation.
- **In-catalog diseases.** Those have a catalogId; iOS reads their steps from the CDN `diseases.json` (parent SPEC). Enrichment fires **only on `catalogId == nil`**.
- **Mapping unknown names INTO the catalog.** That is #50's job (alias table + LLM disambiguation). Enrichment is strictly the "truly not in catalog" tail and **does not set catalogId**.
- **Healthy verdicts / non-plant.** No out-of-catalog disease name exists → enrichment never fires. The "never healthy" force-pick (#49 `forceDiseaseOnHealthyVerdict`) stays an **in-catalog** path and is untouched.
- **Admin review UI.** V1 review = Yao editing rows in the Supabase Dashboard (same as plant enrichment).
- **Stampede coalescing.** V1 accepts two concurrent first-callers for the same disease each spend one LLM call; `ON CONFLICT DO NOTHING` keeps one row.

### 1.3 Inputs

| Source | Input |
|---|---|
| Within `HandleDiagnose` | the decided out-of-catalog `issue.Name` (free text) + `IdentifiedName` / `PlantID` (prompt context only) |
| Curated pools | `shared.steps` (S01–S44), `shared.remedies` (K01–K15) from `diseases.json` |
| Config | `OPENAI_API_KEY` + `SUPABASE_DB_URL` (same secrets as plant enrichment) |

### 1.4 Outputs

| Function | Output | Error cases |
|---|---|---|
| `DiseaseService.GetOrGenerate(ctx, diseaseName, plantCtx)` | `*StructuredDiseaseDetail` | `ErrDBUnavailable`, `ErrEnrichmentUnavailable`, budget-skip → caller degrades to slim |
| `/v1/diagnose` 200 | `DiagnoseResult` whose out-of-catalog issue now carries `structuredDetail` (catalogId still null) | never 502 from enrichment — degrades to slim issue |

Wire shape is **identical** whether the enriched detail came from cache / DB / LLM.

### 1.5 External dependencies

- **Supabase Postgres** — pgx pool, `SUPABASE_DB_URL` (shared with plant enrichment). **New table `diseases_pending`** (§5).
- **OpenAI `gpt-4o-mini`** — shared `proxy.VisionClient.post(...)` transport, distinct prompt path.

---

## 2. Trigger + cascade (where it sits)

Post-#50 `mapCatalogID` cascade (returns a catalog id or nil — **enrichment is NOT inside mapCatalogID**, it runs in `HandleDiagnose` after mapCatalogID returns nil):

1. exact catalog-name match → id
2. alias table (**narrowed to true synonyms / spelling variants** — e.g. `botrytis`/`gray mold`→L23, `sooty mold`→L21; the causal-name entries like `drought stress` are kept only if you want them mapped, see §8) → id
3. LLM disambiguation w/ descriptions (**may answer NONE**) → id
4. **nil (genuinely out-of-catalog)** → `HandleDiagnose` calls disease enrichment (this SPEC): preserve `Name`, fill `structuredDetail`, **catalogId stays null**
5. enrichment unavailable / errors / budget-skip → **slim issue** (legacy `Treatment` text), never blocks the 200

Healthy / non-plant never reach step 4 (no out-of-catalog name to enrich).

### Budget (critical — diagnose has a hard WriteTimeout)

`/v1/diagnose` runs under a 35s server WriteTimeout; Plant.id + the optional vision fallback already spend budget. Inline enrichment adds **one** `gpt-4o-mini` call. Rules:

- Budget the enrichment LLM on **wall-clock from `reqStart`** (mirror `diagnoseFallbackBudget`, parent SPEC §6 pitfall). Too little left → **skip generation → slim issue** (step 5), never 502.
- Cache / DB lookup is ~ms → always attempted (cheap even when budget is tight).
- **Only the FIRST out-of-catalog issue per diagnose is enriched** in V1. diagnose returns 1–3 issues; enriching all could blow the budget. Remaining out-of-catalog issues stay slim. (Enrich-all is §9.)

---

## 3. Generation contract (AI selects ids; backend back-fills text + image)

LLM (`gpt-4o-mini`, `json_schema` strict) returns **ids only**:

```jsonc
{
  "name": "Drought Stress",            // echo input name (preserve, NOT remapped)
  "shortDescription": "...",           // 15–40 words (slim, mirror plant enrichment v2)
  "symptomAnalysis": "...",
  "cause": "...",
  "treatment":  { "groups": [ { "label": "For mild cases", "stepRefs": ["S04","S12"] } ] },
  "homeRemedyRefs": ["K15"],
  "prevention": { "groups": [ { "label": null, "stepRefs": ["S20"] } ] }
}
```

- `stepRefs` / `homeRemedyRefs` MUST be ids drawn from the whitelisted pools (S01–S44 / K01–K15). **No bodies / titles / images from the AI.** The prompt lists the available S-ids + K-ids with their titles so the model can choose meaningfully (mirror how `common_diseases_list` lists catalog disease ids in plant enrichment `prompt.go:181`).
- Back-fill: each `stepRef` → `{num (group-local index), title, body, image}` from `shared.steps`; each `homeRemedyRef` → `{title, recipe, usage, image}` from `shared.remedies`. **Unknown ids dropped; a group emptied by dropping is dropped.**
- `max_tokens` small (~800) — slim output keeps it inside the diagnose budget.

---

## 4. Wire contract change (`HealthIssue`)

Additive, back-compat — legacy `Treatment` kept:

```go
type HealthIssue struct {
    // ...existing fields...
    Treatment        Treatment                 `json:"treatment"`        // legacy 3×[]string, unchanged
    StructuredDetail *StructuredDiseaseDetail   `json:"structuredDetail,omitempty"` // NEW — present ONLY for enriched out-of-catalog issues
}

type StructuredDiseaseDetail struct {
    ShortDescription string           `json:"shortDescription"`
    SymptomAnalysis  string           `json:"symptomAnalysis"`
    Treatment        TreatmentGroups  `json:"treatment"`
    HomeRemedies     []Remedy         `json:"homeRemedies"`
    Prevention       TreatmentGroups  `json:"prevention"`
}
// TreatmentGroups{ Groups []Group }; Group{ Label string; Steps []Step }
// Step{ Num int; Title, Body, Image, Ref string; SubSteps []Step }
// Remedy{ Ref, Title, Recipe, Usage, Image string }
```

- `catalogId` stays **null** (out-of-catalog marker). iOS: **if `structuredDetail` present → render it with the SAME view as in-catalog steps; else slim** (legacy Treatment text).
- `image` is a **filename** (e.g. `"uoIzi.png"`); iOS applies the **existing CDN-prefix rule** it already uses for in-catalog step images — same asset source.
- In-catalog issues unchanged: no `structuredDetail`, iOS still reads CDN `diseases.json` by `catalogId`.

---

## 5. Supabase schema (`diseases_pending`)

Mirror `plants_pending`. New table (migration in `proxy/enrichment/migrations/`):

```sql
CREATE TABLE diseases_pending (
  disease_name_normalized TEXT PRIMARY KEY,  -- normalizeDiseaseName(name); decision §8: name-keyed, plant-agnostic
  disease_name            TEXT NOT NULL,
  data                    JSONB NOT NULL,    -- StructuredDiseaseDetail, bodies/images ALREADY back-filled (frozen)
  status                  TEXT NOT NULL DEFAULT 'pending',  -- pending|approved|rejected
  source                  TEXT,              -- e.g. openai-gpt-4o-mini-2024-07-18
  source_version          TEXT,              -- PromptVersion
  generation_request_id   TEXT,
  created_at              TIMESTAMPTZ DEFAULT now()
);
-- lookup: WHERE disease_name_normalized=$1 AND status IN ('pending','approved')  (approved preferred)
-- write:  INSERT ... ON CONFLICT (disease_name_normalized) DO NOTHING; race winner re-queried
```

- `data` stores the **back-filled (denormalized)** detail → the row is self-contained, no re-back-fill on read. Trade-off: if `shared.steps` text/images later change, old rows keep old copies until regenerated — **acceptable**, mirrors `diseases.json`'s own denormalization.
- Reuses the same pgx pool / `SUPABASE_DB_URL` as plant enrichment.

---

## 6. Errors / timeout / fallback

- enrichment LLM error / timeout / budget-skip → **slim issue** (legacy `Treatment`), 200 (never 502).
- AI returns prose but all refs unknown/dropped → keep the prose (shortDescription / symptomAnalysis) with empty steps — **still better than today's empty Treatment**; do NOT fall all the way back to slim. *(confirm §8)*
- DB unavailable → still attempt LLM generation (works, just uncached) and return it; only the cache write is skipped. *(confirm §8)*

---

## 7. Pitfalls (don't re-rediscover)

- **DON'T enrich in-catalog issues** (`catalogId != nil`) — wasted LLM + would override curated content.
- **DON'T let enrichment block or 502 the diagnose** — it is a tail enhancement; always degrade to the slim issue.
- **Back-fill images via the SAME CDN rule** as in-catalog step images, or they 404.
- `normalizeDiseaseName` is the **single SOT** for the cache key + DB PK + #50's alias key.
- **Whitelist refs BEFORE persist** — the model will sometimes emit `S99` / `K99`.
- **Budget from `reqStart`, not `ctx`** (parent SPEC §6 fallback-budget pitfall).
- Store **back-filled** data, not raw refs, so a read needs no pool access and is robust to pool reshuffles.

---

## 8. Resolved decisions (with the open ones flagged for your review)

Locked (per your calls):
- **Synchronous, inline in `/v1/diagnose`** ("识别过程多等几秒"), not async/background.
- **Cache/PK = normalized disease name, plant-agnostic** (same disease → same content everywhere).
- **AI selects step ids; backend back-fills text + image from curated pools** (reuse human-reviewed assets; fast; cheap).
- **catalogId stays null for enriched issues** (honest out-of-catalog; Name preserved; no L08 mis-mapping — this replaces #50's dropped forced/L08 net).
- **pending 即上线 + Dashboard 审核** (mirror plant enrichment).
- **#50 keeps ①② (narrowed alias) + ③; drops ④forced + ⑤L08.**

⚠️ **Need your confirmation:**
1. **Only first out-of-catalog issue enriched per diagnose** (budget) — OK? (alt: enrich all 1–3, risk budget)
2. **All-refs-unknown fallback** (§6): keep prose with empty steps, vs full slim?
3. **DB-down behavior** (§6): generate-uncached vs skip-to-slim?
4. **`drought stress` itself**: under "尽量映射 + ② narrowed", does it map to L07 (shows "Underwatering yellowing") or do you want causal names like it to ALWAYS enrich+preserve-name? (decides whether `drought stress`→L07 stays in the alias table)

---

## 9. Out of scope (V1.1+)

- Enriching all 1–3 issues in one diagnose.
- Promoting approved disease rows back into curated `diseases.json`.
- Stampede coalescing.
- Per-plant disease variants (V1 is name-keyed only).
- Regenerating rows when `shared.steps` changes.

---

## 10. #50 relationship + phasing

- **#50 (`diagnose-catalog-fallback`)** narrows to "mapping-recall boost": keep ① descriptions in the disambiguation prompt + ② narrowed alias table; **DROP ③ forced pass + ④ L08 net**; step 4 returns `catalogId=null` (slim) until enrichment ships. Update `proxy/SPEC.md §2.2` to match (the P1 SPEC drift the #50 review flagged).
- **Phase 1 (backend, this SPEC):** parse `shared.steps`/`shared.remedies` → `ContentIndex`; `diseases_pending` table + DB layer; prompt + whitelist + back-fill; `HealthIssue.StructuredDetail`; wire into `HandleDiagnose` step 4.
- **Phase 2 (iOS, `yardmate-swiftui`):** render `structuredDetail` (reuse the in-catalog step view) for `catalogId==null` enriched issues; hero = captured photo. Doc/SPEC under `app/YardMate/YardMate/Diagnose/`.
