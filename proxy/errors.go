package proxy

import "errors"

// Sentinel errors returned by Plant.id / OpenAI clients. HTTP handlers map
// these to client-facing error codes per SPEC §3.
var (
	// ErrPlantIDImageRejected is returned when Plant.id 400s the image
	// (unsupported format, not an image, etc.). Maps to client `bad_image`.
	ErrPlantIDImageRejected = errors.New("plant_id: image rejected")

	// ErrPlantIDUnauthorized is returned on Plant.id 401/403, meaning our
	// API key is invalid or revoked. This is a server config problem, not
	// a client problem. Maps to `plant_id_unauthorized` (502).
	ErrPlantIDUnauthorized = errors.New("plant_id: unauthorized")

	// ErrPlantIDRateLimit is returned when Plant.id 429s us (we hit their
	// limit). Maps to `plant_id_unavailable` (502 to client).
	ErrPlantIDRateLimit = errors.New("plant_id: rate limit")

	// ErrPlantIDUnavailable is returned for Plant.id 5xx, network errors,
	// or timeouts. Maps to `plant_id_unavailable` (502).
	ErrPlantIDUnavailable = errors.New("plant_id: unavailable")

	// ErrPlantIDBadResponse is returned when Plant.id returns 200 but the
	// JSON is malformed, or returns an unexpected status code. Maps to
	// `plant_id_unavailable` (502) — opaque to client.
	ErrPlantIDBadResponse = errors.New("plant_id: bad response")
)

// Sentinel errors returned by the Pl@ntNet client (primary identify engine,
// SPEC §1.4 / §7). The Unavailable/RateLimit/Unauthorized/BadResponse set
// triggers the Plant.id fallback in HandleIdentify; ImageRejected does not
// (Plant.id would reject the same bytes). A Pl@ntNet 404 "Species not found"
// is a VALID empty result, NOT one of these errors.
var (
	// ErrPlantNetImageRejected is returned when Pl@ntNet 400/413s the image
	// (bad request / payload too large). Maps to client `bad_image`. Does
	// NOT trigger the Plant.id fallback (same bytes would be rejected too).
	ErrPlantNetImageRejected = errors.New("plantnet: image rejected")

	// ErrPlantNetUnauthorized is returned on Pl@ntNet 401/403, meaning our
	// API key is invalid, revoked, or out of quota-tier. Server config
	// problem, not a client problem. Triggers the Plant.id fallback; if the
	// fallback also fails auth the handler maps to `plant_id_unauthorized`.
	ErrPlantNetUnauthorized = errors.New("plantnet: unauthorized")

	// ErrPlantNetRateLimit is returned when Pl@ntNet 429s us (free-tier
	// daily quota exhausted). Triggers the Plant.id fallback; maps to
	// `plant_id_unavailable` (502) if the fallback also fails.
	ErrPlantNetRateLimit = errors.New("plantnet: rate limit")

	// ErrPlantNetUnavailable is returned for Pl@ntNet 5xx, network errors,
	// or timeouts. Triggers the Plant.id fallback; maps to
	// `plant_id_unavailable` (502) if the fallback also fails.
	ErrPlantNetUnavailable = errors.New("plantnet: unavailable")

	// ErrPlantNetBadResponse is returned when Pl@ntNet returns 200 but the
	// JSON is malformed, or returns an unexpected status code. Triggers the
	// Plant.id fallback (opaque to client; maps to `plant_id_unavailable`).
	ErrPlantNetBadResponse = errors.New("plantnet: bad response")
)
