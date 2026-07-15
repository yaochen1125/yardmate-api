# `vision` — L1 catalog-native vision kNN microservice (V1.x)

The "L1 参考图向量库" from the identify-accuracy overhaul: a catalog-native visual
nearest-neighbour signal that runs alongside PlantNet / Plant.id / GPT. Python
microservice (BioCLIP-2 + hnswlib), same-host, called by the Go cascade over
localhost. This SPEC is the contract; `README.md` is the operator runbook.

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for
- Embedding a user photo with **BioCLIP-2** and returning the nearest catalog
  species by visual cosine similarity over a **real-photo reference index**.
- A readable **in-catalog / out-of-catalog** verdict from nearest-neighbour distance.
- Building + serving that index (`pull_reference.py` → `build_index.py` → `app.py`).

### 1.2 What this package is NOT responsible for
- NOT the identify decision. It is ONE fusion signal; the Go cascade owns ranking.
- NOT cultivar identification from AI-generated art (see invariant #1 — impossible).
- NOT a public endpoint. Bound to `127.0.0.1`; only the Go server (same host) calls it.
- NOT stateful per user. The flywheel (user accept/correct → index append) is P2, elsewhere.

### 1.3 Inputs
- Build: current authoritative `plants_index.json` (id + scientific_name) + iNaturalist.
- Serve: `POST /v1/vision/identify` multipart `image` (the same bytes the cascade buffers).

### 1.4 Outputs
- JSON: `candidates[{catalog_id, vision_sim}]`, `nn_sim`, `in_catalog`,
  `in_catalog_confidence`, `model`. See §2.1.

### 1.5 External dependencies
- iNaturalist API (build-time only, real reference photos; only vectors are kept).
- BioCLIP-2 weights (HuggingFace, one-time download). No per-request third party, no keys.

## 2. Contract

### 2.1 HTTP — `POST /v1/vision/identify` (multipart `image`, optional `k`,`top`)
```json
{ "candidates": [{"catalog_id": "AAA0262", "vision_sim": 0.85}],
  "nn_sim": 0.85, "in_catalog": true, "in_catalog_confidence": 0.61,
  "model": "hf-hub:imageomics/bioclip-2" }
```
`GET /health` → `{ok, loaded, count}`. Errors (index unloaded / bad image) → non-2xx.

### 2.2 Go integration — `handlers.go` 7a-4 (kill-switch `VISION_KNN_ENABLED`, default OFF)
- Fired in **parallel** with the GPT arbiter (goroutine + buffered chan) so CPU latency
  hides behind the engine cascade. Client `proxy/vision_knn.go` (`VisionKNNClient`).
- **v1 conservative fusion**: ACTS only to RAISE confidence when the kNN top candidate
  == the chosen in-catalog `plant_id` AND `in_catalog==true` (`visionKNNAgreesWithDecision`).
  Reuses `boostedConfidence` (raise-only). NEVER changes which plant is returned.
- Every other outcome (in-catalog disagreement / out-of-catalog verdict vs in-catalog
  decision) is **logged only** — staging data before we let it override (P1 follow-up).

## 3. Core invariants (P0 铁律 — violating breaks it; see `P0_CONCLUSION.md`)
1. **Discrimination index holds REAL photos only.** AI-generated catalog art vs real
   photos differ by cosine ~0.18; near-species/cultivar morphology differs by ~0.04.
   Mixing lets nearest-neighbour decide by *art style*, not botany (flagship 0/20).
2. **Never mix real + generated references in one candidate set.**
3. **Cultivars (Spiralis, named roses) have no iNat taxon** → no real reference. L1 does
   NOT auto-solve cultivars; the flywheel (P2) supplies real cultivar photos over time.

## 4. Fail-open + kill-switch
- `VISION_KNN_ENABLED=false` (default) → nil client → 7a-4 skipped entirely.
- Enabled but service unreachable / slow (6 s cap) / non-2xx / decode error → logged,
  main cascade unchanged. The signal can never worsen a result while it only boosts.

## 5. Why BioCLIP-2 (P0 evidence)
Near-species discrimination (real photos, chance 33%): generic CLIP 66% / **BioCLIP-2 89%** /
DINOv2 84%. In/out AUC 0.995. Bio-specialized + retains a text tower (L2 fine-tune path).

## 6. Roadmap
- P1b: build full-catalog real-photo index (server; `pull_reference.py` + `build_index.py`).
- P1d: staging calibration of the in/out threshold (0.80 prior) + boost strength on real traffic.
- P1-follow: enable override for in-group disambiguation once staging data supports it.
- P2b (SHIPPED 2026-07-14, `fold_external.py` = build step 3): fold catalog real **external gallery**
  photos (iNat/Wikimedia CC0/BY/SA, R2 `{id}/external/`) into the index for under-covered plants
  (iNat ≤ 20) + fix cultivar-on-species mislabels. Relative-NN filter (abs threshold fails — same-plant
  vs diff-plant sim overlap heavily: same p5≈0.80 vs diff p95≈0.81) + current-catalog gate (drops
  stale removed ids). NOT user photos — no correction signal (accept ≠ correct) would poison the
  index; dropped. NOT AI-generated main images (P0 铁律).
- P2b **Phase B** (`pull_supplemental.py` = build step 3.5, default-OFF `VISION_SUPP_ENABLED`): go BEYOND
  the ~4 curated external images and **actively deep-mine** more real photos for still-thin plants —
  Wikimedia Commons deep (past imageingest's top-4) + GBIF occurrence media + PlantNet related-images.
  Only CC0/CC-BY/CC-BY-SA (`supp_sources.classify_license`; NC/ND never — commercial app). **Correctness
  gate is the crux** (search results carry noise: maps / diagrams / herbarium sheets / wrong species /
  non-plants): iNat-anchored thin *species* → relative-NN (free); **anchor-free** plants (0-iNat + ALL
  cultivars — the parent-species anchor is unreliable, would mis-reject true cultivar photos) →
  **embedding-cluster consensus + GPT-4o (`detail:low`) borderline verifier** (`supp_verify`), hard-capped
  by `VISION_SUPP_MAX_VLM`. **Index-only**: verified images cached on the build box (+ license/attribution
  manifest), only their vectors folded (`fold_external --supp`) — user galleries + the public bucket
  untouched. Staging-validate the lift with `eval_holdout.py` (LOO top-1, `mark_deleted`) before prod.
- L2 (future): fine-tune BioCLIP on own catalog real photos → domain-robust "own plant model".

## 7. Pitfalls (don't re-rediscover)
- Generated art in the index looks like it works offline but collapses real queries by style.
- BioCLIP-2 is ViT-L: ~2 GB resident + ~1-2 s/img CPU on the 4-core box. Runs concurrent with
  engines so latency hides, but measure on the server (§README perf) — consider ONNX/INT8.
- iNat has no cultivar taxa; `pull_reference.py` queries species-level (strips the cultivar) →
  a VISUALLY-DISTINCTIVE cultivar (Juncus effusus 'Spiralis' corkscrew, Tulipa 'Queen of Night'
  black tulip → iNat even matched *Liriodendron tulipifera* the tulip TREE) gets its parent-SPECIES
  photos MISLABELED as the cultivar in the index → active mis-ID (this was the founding corkscrew
  bug's root cause). `fold_external.py` (build step 3) auto-detects it — the cultivar's external
  gallery photo is dissimilar to its iNat centroid — and REPLACES the mislabeled species vectors
  with the correct external photos. Subtle cultivars (look like their species) keep both. Don't
  "fix" taxonomic synonyms (Aloe→Aristaloe, Anemone→Anemonoides): same plant, correct photos.
- **GBIF media license ≠ occurrence license (Phase B landmine).** The `occurrence/search?license=` param
  filters the *record's* data license, NOT the *photo's* license. Measured: `license=CC_BY_4_0` returned
  records whose `media[].license` was `cc-by-nc` (iNat photos are overwhelmingly NC on GBIF). So the
  server-side filter is both wrong-field AND rejects repeated `license` params with HTTP 400. `supp_sources`
  filters ONLY on per-`media[].license` (`classify_license`). Net GBIF yield of usable CC0/BY/SA photos is
  therefore modest (most iNat-sourced media correctly rejected) — that's the license rule working, not a bug.
- **Cultivars are anchor-free even WITH iNat parent vectors (Phase B).** `pull_supplemental` routes ALL
  cultivars to the cluster+GPT gate, never relative-NN: the parent-species anchor may be a visually-distinct
  mislabel (corkscrew rush vs straight rush), so NN-against-parent would reject the TRUE cultivar photos and
  keep the wrong ones — the exact bug `fold_external` exists to fix. Search cultivars by their FULL name
  (Commons/GBIF do carry some cultivar photos, unlike iNat's species-only taxa); GPT confirms cultivar identity.
