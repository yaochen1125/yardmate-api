# `training` package — opt-in user photo flywheel intake (P2a)

Server-side intake for the **user photo flywheel** (identify-accuracy L1/L2 data
layer). Receives opt-in user identification photos + labels, strips metadata,
stores them on the local training corpus, and records one metadata row per photo
so the L1 re-embed job (P2b) and the 7788 dashboard (P2d) can consume them.

This package is intake + storage + deletion ONLY. Embedding into the hnswlib
index and index growth live in `vision/` (P2b); the dashboard lives in the 7788
admin tool (P2d). Both read the same SQLite file this package writes.

## 1. Five questions

### 1.1 What this package is responsible for
- `POST /v1/training/photo` — accept one opt-in photo (multipart) + its identify
  label metadata; strip JPEG metadata (EXIF/GPS/XMP); store the image on the
  local corpus; write one `photos` row.
- `POST /v1/training/delete` — delete every photo uploaded by a device
  (`X-Device-Install-Id`): remove the files + tombstone the rows. This backs the
  iOS "delete my uploaded photos" action and the opt-out flow.
- `Store.DeleteByUserID(ctx, userID)` — package API called from
  `account_delete.go` so account deletion cascades to training photos.
- `Store.Stats(ctx)` — counts (total / embedded / pending) for `/healthz`-style
  introspection and the 7788 dashboard.

### 1.2 What this package is NOT responsible for
- Embedding photos into the vision index / growing `max_elements` — that is
  `vision/` P2b. This package only sets `embedded=0`; the embed job flips it.
- Deciding which labels are trustworthy enough to enter the DISCRIMINATIVE index
  — the quality gate (accept/correct only) is applied by the embed job (P2b),
  not here. This package stores everything opted-in, labeled or not (unlabeled
  photos still feed L2 self-supervised pretraining).
- Enforcing the client opt-in — the toggle is client-side (default OFF). The
  server records `consent_version` per photo as proof-of-consent metadata but
  does not itself gate on consent state (it cannot observe it).
- Any App Attest signature verification — the repo has no assertion-verifying
  middleware (identify only logs the headers, iOS-26 issue). Abuse defense is
  per-IP + per-device rate limit + inflight bound, same posture as identify.
- Exporting the corpus / disk accounting UI — that is 7788 P2d.

### 1.3 Inputs
`POST /v1/training/photo` (multipart/form-data), per-device rate-limit group:
- headers: `X-Device-Install-Id` (UUID, required), `X-App-Version` (required).
- `image` — JPEG bytes (required; byte-sniffed, `image/jpeg` only; ≤ `trainingMaxBody`).
- `label_kind` — `accept` | `correct` | `none` (default `none`).
- `plant_id` — catalog id (e.g. `AAA0701`), only meaningful when labeled.
- `scientific_name`, `source_engine`, `top_confidence` — optional identify context.
- `consent_version` — required; the privacy-policy/consent version the user agreed to.
- `user_id` — optional UUID soft-attribution (see §5 threat model).

`POST /v1/training/delete`: header `X-Device-Install-Id` (UUID, required). No body.

### 1.4 Outputs
- upload → `200 {"id": "<uuid>", "stored": true}`.
- delete → `200 {"deleted": <n>}`.
- errors → `4xx/5xx {"error": "<code>"}` (see §3), same envelope as identify.

### 1.5 External dependencies
- Local disk under `trainingDir` (default `/var/lib/yardmate-api/training`,
  override `YARDMATE_API_TRAINING_DIR`). This is inside the systemd
  `ReadWritePaths` — no unit change needed. Layout:
  - `training_meta.db` — SQLite (WAL) metadata, `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`).
  - `photos/<YYYY>/<MM>/<uuid>.jpg` — stripped images.
- No Supabase / no network. Deliberately server-local (the corpus is server
  infra state, like the vision index — not user product state), so the embed job
  and dashboard (both on the same host) read the SQLite file directly.

## 2. Contract

### 2.1 kill-switch + registration
`TRAINING_UPLOAD_ENABLED` (`vault.GetBool`, default **false**, same posture as
`VISION_KNN_ENABLED`). `main.go` opens the `Store` only when true; a nil store
makes `server.go` register neither route. Rollback = flip the flag + restart.

### 2.2 rate limit / concurrency
Registered inside the per-device group AND the inflight sub-group (it buffers a
full image in memory like identify/diagnose), so a burst sheds 503 instead of
OOM. Inherits per-IP + per-device (300/h) limits. It does NOT consume the global
paid-spend gate (no paid upstream call).

### 2.3 metadata strip
Every uploaded JPEG passes `stripJPEGMetadata` before storage: drop APP1..APP15
(Exif/GPS/XMP/thumbnails) + COM segments, keep APP0 (JFIF) + the entropy-coded
image. No re-encode → no quality loss for training. iOS already strips by
re-encoding from a bare bitmap; this is defense-in-depth. A JPEG that fails to
parse is rejected `400 bad_image` (doubles as validation).

### 2.4 storage + row
`id` is generated server-side (crypto/rand UUID v4) — the client-supplied
device id is never the photo id. File written atomically (tmp + rename). Row
inserted after the file lands; if the insert fails the file is removed (no
orphans). `embedded=0`, `exported=0`, `deleted=0` at insert.

## 3. Error / outcome matrix
| code | status | when |
|---|---|---|
| `missing_device_id` | 400 | header absent / not a UUID |
| `missing_app_version` | 400 | header absent |
| `bad_multipart` | 400 | not multipart / reader error |
| `missing_image` | 400 | no `image` part |
| `image_too_large` | 413 | body over `trainingMaxBody` |
| `bad_image` | 400 | MIME sniff ≠ image/jpeg, or JPEG parse fails in strip |
| `missing_consent_version` | 400 | `consent_version` absent (proof-of-consent required) |
| `server_error` | 500 | disk / DB write failure |
| `rate_limit_ip` / `rate_limit_device` | 429 | limiter (middleware) |
| `server_busy` | 503 | inflight bound (middleware) |

## 4. Rate-limit / caps
- `trainingMaxBody = 9 << 20` (same as identify: 8 MB image + overhead).
- Field readers are `io.LimitReader`-bounded (label 16 / plant_id 32 /
  sci_name 128 / engine 32 / confidence 16 / consent 32 / user_id 64).

## 5. Security / privacy model
- **No plaintext PII in URLs/logs.** No coordinates are accepted here (unlike
  identify). userID/installID never logged.
- **`user_id` is a spoofable soft-attribution field**, not Bearer-verified (kept
  cheap; verifying a Supabase JWT on every upload is not worth it for a soft
  cascade hint). Threat model: the ONLY thing user_id drives is the
  account-deletion cascade (`DeleteByUserID`). Worst case, a client stamps
  someone else's userID on ITS OWN photo → that photo (which it owns) gets
  deleted when the other account is deleted. No cross-user disclosure, no ability
  to delete another user's photos (delete-by-device uses the keychain install id;
  delete-by-user runs only inside Bearer-verified `/v1/account/delete`). Photos
  with no user_id are still covered by device-level delete.
- **Deletion = file removed immediately + row tombstoned** (`deleted=1`,
  `deleted_at`, PII columns nulled). The tombstone retains only `id` +
  `embedded` so the P2b index-cleanup can drop the vector; a full rebuild
  excludes `deleted=1` naturally.
- **iOS integration requirement (P2a-5 / task 2):** account deletion must call
  BOTH `/v1/account/delete` (server cascades `DeleteByUserID`) AND
  `/v1/training/delete` (install id) so device photos are removed regardless of
  whether `user_id` was recorded.

## 6. Schema (`photos`)
```sql
CREATE TABLE IF NOT EXISTS photos (
  id              TEXT PRIMARY KEY,
  rel_path        TEXT NOT NULL,          -- photos/YYYY/MM/<id>.jpg ('' after delete)
  install_id      TEXT,                   -- device key (nulled on delete)
  user_id         TEXT,                   -- soft attribution (nulled on delete)
  label_kind      TEXT NOT NULL,          -- accept | correct | none
  plant_id        TEXT,                   -- catalog AAA id when labeled
  scientific_name TEXT,
  source_engine   TEXT,
  top_confidence  REAL,
  consent_version TEXT,
  app_version     TEXT,
  bytes           INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,       -- unix seconds
  embedded        INTEGER NOT NULL DEFAULT 0,
  embedded_at     INTEGER,
  exported        INTEGER NOT NULL DEFAULT 0,
  exported_at     INTEGER,
  deleted         INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER
);
```
WAL + `busy_timeout=5000` so the Python embed job (separate process) can read
concurrently with serving writes.

## 7. Resolved decisions (don't re-debate)
- Metadata store = server-local SQLite, NOT Supabase (corpus is server infra
  state; three readers — Go writer, Python embed job, Python dashboard — share
  one file; avoids Supabase egress/row limits).
- Originals live on server disk (R2 is only 10 GB, shared with plant images);
  cold export to local + delete-already-trained is P2d.
- Store everything opted-in (labeled or not); the accept/correct quality gate is
  applied at index time (P2b), not intake.
- Photo id generated server-side; client device id is not the photo id.
- Delete-by-device needs no Bearer; delete-by-user runs only Bearer-verified.

## 8. Out-of-scope (later phases)
- P2b: re-embed job, hnswlib `resize_index`/`add_items`, weekly full rebuild,
  quality gate (accept/correct only), tombstone index cleanup.
- P2c: explicit correct-this-ID UI signal (today only accept + implicit garden).
- P2d: 7788 dashboard (total/embedded/pending/disk/coverage) + one-click export.

## 9. Pitfalls (don't re-rediscover)
- `CGO_ENABLED=0` (deploy scripts) → SQLite MUST be a pure-Go driver
  (`modernc.org/sqlite`), never `mattn/go-sqlite3`.
- Set `db.SetMaxOpenConns(1)` for the writer + WAL; modernc can return "database
  is locked" under concurrent writers otherwise. Cross-process reads are handled
  by WAL + busy_timeout, not by Go's pool.
- Strip metadata by DROPPING APPn segments, not by re-encoding — re-encoding a
  training photo adds JPEG artifacts that hurt L2 downstream.
- Remove the file if the DB insert fails; insert the row only after the file is
  durably renamed into place (no orphans, no dangling rows).

## 10. Implementation outline
- `store.go` — `OpenStore(dir)`, `Insert`, `DeleteByInstallID`,
  `DeleteByUserID`, `Stats`, `Close`. SQLite schema + atomic file write.
- `jpeg.go` — `stripJPEGMetadata([]byte) ([]byte, error)`.
- `handlers.go` — `HandleUpload(store)`, `HandleDelete(store)`.
- wiring — `main.go buildTrainingStore(vault)`; `server.go` per-device+inflight
  registration; `account_delete.go` cascade.
