package main

// account_delete_supabase.go — Supabase admin operations for
// POST /v1/account/delete: Storage object cleanup (step 3, best-effort) and
// auth-user deletion (step 5, load-bearing).
//
// Both use the SAME service_role auth as a plain Bearer + apikey header — the
// documented, RLS-bypassing server-side credential. This deliberately does NOT
// use the S3 protocol (AWS SDK) for Storage: the Supabase S3 session-token mode
// requires the project's anon key as the SigV4 secret and is documented as
// user/RLS-scoped, so a service_role admin wipe over it is ambiguous and would
// fail SILENTLY (SignatureDoesNotMatch) — unacceptable for a best-effort step
// that must actually run. The Storage REST API with the service_role key is the
// unambiguous admin path and needs no extra secret beyond the one step 5 already
// requires. (See the PR description for the rationale + the alternative.)
//
// Object layout under bucket "diary-images" (set by the iOS app):
//   {userID}/{entryID}/{i}.jpg     diary photos (LiveDiaryService)
//   {userID}/covers/{uuid}.jpg     pinned cover copies (LiveGardenService)
// i.e. everything for a user is two levels under the prefix "{userID}/". The
// Storage list API is folder-scoped (a single "/"-delimited level), so we walk
// the known two-level shape: list "{userID}/" -> for each child folder, list its
// files -> batch-delete the collected object keys.
//
// SECURITY: the service_role key is read only from the vault and is sent in
// Authorization + apikey headers; it is NEVER logged. No object key, token, or
// response body is logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	// diaryImagesBucket is the Supabase Storage bucket the iOS app uploads diary
	// photos + pinned cover copies to (LiveDiaryService / LiveGardenService).
	diaryImagesBucket = "diary-images"

	// storageListPageSize bounds one list page; the bucket per user is small
	// (a handful of diary entries), but we paginate defensively.
	storageListPageSize = 100

	// supabaseHTTPTimeout caps each admin REST round-trip.
	supabaseHTTPTimeout = 10 * time.Second
)

// supabaseHTTPClient is shared across all admin REST calls (list / delete /
// auth-user delete). Reused package-wide rather than constructed per request so
// the underlying transport pools connections instead of leaking one per call.
var supabaseHTTPClient = &http.Client{Timeout: supabaseHTTPTimeout}

// supabaseListItem is one entry from POST /storage/v1/object/list. A "folder"
// (a synthetic prefix) has a null id; a real object has a non-null id.
type supabaseListItem struct {
	Name string  `json:"name"`
	ID   *string `json:"id"`
}

// deleteUserStorageObjects removes every Storage object under "{userID}/" in the
// diary-images bucket. BEST-EFFORT: returns an error that the caller logs +
// swallows. It walks the known two-level layout ({userID}/{folder}/{file}) and
// also deletes any stray files directly under "{userID}/".
//
// NOTE on the folder argument: Supabase's list `prefix` is a folder PATH with NO
// trailing slash (it mirrors the official storage-js `from(bucket).list(path)`,
// which posts `prefix: path` verbatim). The returned `name` is the LEAF only
// (e.g. "covers" or "<uuid>.jpg"), so we rebuild the full object key by joining
// the folder path + "/" + name ourselves.
func deleteUserStorageObjects(ctx context.Context, supabaseURL, serviceRoleKey, userID string) error {
	children, err := storageList(ctx, supabaseURL, serviceRoleKey, diaryImagesBucket, userID)
	if err != nil {
		return fmt.Errorf("list %q: %w", userID, err)
	}

	var keys []string
	for _, c := range children {
		if c.Name == "" {
			continue
		}
		if c.ID != nil {
			// A real object sitting directly under "{userID}/" (defensive — the
			// app nests everything one level deeper, but tolerate flat files).
			keys = append(keys, userID+"/"+c.Name)
			continue
		}
		// A folder ({entryID} or "covers"): list its files one level down.
		// folder = "{userID}/{name}" (slash-free at the leaf, per the list API);
		// keys are rebuilt as "{folder}/{file}".
		folder := userID + "/" + c.Name
		files, err := storageList(ctx, supabaseURL, serviceRoleKey, diaryImagesBucket, folder)
		if err != nil {
			// Don't abort the whole wipe on one folder; record + continue.
			// (Best-effort overall, but maximise what we do remove.) Log the
			// error text only — never the folder path / object keys (they embed
			// the userID), per this file's no-sensitive-logging contract.
			log.Printf("account/delete: storage sub-folder list failed (best-effort, continuing): %v", err)
			continue
		}
		for _, f := range files {
			if f.Name == "" || f.ID == nil {
				continue // skip empty names + any deeper folders (layout is 2-level)
			}
			keys = append(keys, folder+"/"+f.Name)
		}
	}

	if len(keys) == 0 {
		return nil // nothing to delete (already clean / never uploaded)
	}
	return storageDelete(ctx, supabaseURL, serviceRoleKey, diaryImagesBucket, keys)
}

// storageList pages through POST /storage/v1/object/list/{bucket} for a folder
// path (NO trailing slash), returning all items (files + synthetic folder
// prefixes) at that level. The body mirrors the official storage-js client's
// DEFAULT_SEARCH_OPTIONS (limit/offset/sortBy) so the server never rejects a
// missing field — `sortBy` is included for byte-for-byte parity.
func storageList(ctx context.Context, supabaseURL, serviceRoleKey, bucket, folder string) ([]supabaseListItem, error) {
	endpoint := fmt.Sprintf("%s/storage/v1/object/list/%s", supabaseURL, bucket)
	var all []supabaseListItem
	for offset := 0; ; offset += storageListPageSize {
		reqBody, err := json.Marshal(map[string]any{
			"prefix": folder,
			"limit":  storageListPageSize,
			"offset": offset,
			"sortBy": map[string]string{"column": "name", "order": "asc"},
		})
		if err != nil {
			return nil, err
		}
		body, status, err := supabaseDo(ctx, http.MethodPost, endpoint, serviceRoleKey, reqBody)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("list status %d", status)
		}
		var page []supabaseListItem
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode list page: %w", err)
		}
		all = append(all, page...)
		if len(page) < storageListPageSize {
			break // last page
		}
	}
	return all, nil
}

// storageDelete removes the given object keys via DELETE /storage/v1/object/{bucket}
// with a {"prefixes":[...]} body (exact keys, not glob prefixes — Supabase calls
// the field "prefixes" but treats them as object paths).
func storageDelete(ctx context.Context, supabaseURL, serviceRoleKey, bucket string, keys []string) error {
	endpoint := fmt.Sprintf("%s/storage/v1/object/%s", supabaseURL, bucket)
	reqBody, err := json.Marshal(map[string]any{"prefixes": keys})
	if err != nil {
		return err
	}
	_, status, err := supabaseDo(ctx, http.MethodDelete, endpoint, serviceRoleKey, reqBody)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("delete status %d", status)
	}
	return nil
}

// deleteAuthUser hard-deletes the Supabase auth user via the Admin API:
// DELETE {SUPABASE_URL}/auth/v1/admin/users/{userID} with the service_role key
// in both Authorization (Bearer) and apikey headers. LOAD-BEARING: the handler
// 500s on error.
//
// Status handling: 200/204 == deleted. A 404 is treated as success (idempotent
// retry — the user is already gone), so a client re-issuing the request after a
// partial earlier success still gets a clean 200.
func deleteAuthUser(ctx context.Context, supabaseURL, serviceRoleKey, userID string) error {
	endpoint := fmt.Sprintf("%s/auth/v1/admin/users/%s", supabaseURL, userID)
	_, status, err := supabaseDo(ctx, http.MethodDelete, endpoint, serviceRoleKey, nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusOK || status == http.StatusNoContent:
		return nil
	case status == http.StatusNotFound:
		// Already deleted — treat as success for idempotent retries.
		return nil
	default:
		return fmt.Errorf("auth admin delete status %d", status)
	}
}

// supabaseDo issues an authenticated admin request to a Supabase REST endpoint
// and returns (body, status). The service_role key authorises the request (it
// bypasses RLS) and is sent in BOTH the Authorization bearer header and the
// apikey header, as Supabase's API gateway (Kong/GoTrue/Storage) requires both.
// The key is never logged. body may be nil for verb-only requests (DELETE).
func supabaseDo(ctx context.Context, method, endpoint, serviceRoleKey string, jsonBody []byte) ([]byte, int, error) {
	var rdr io.Reader
	if jsonBody != nil {
		rdr = bytes.NewReader(jsonBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+serviceRoleKey)
	req.Header.Set("apikey", serviceRoleKey)
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := supabaseHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}
