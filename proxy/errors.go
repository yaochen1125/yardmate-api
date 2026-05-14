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

	// ErrOpenAIUnauthorized is returned on OpenAI 401/403 — server config
	// problem. Maps to `openai_unauthorized` (502).
	ErrOpenAIUnauthorized = errors.New("openai: unauthorized")

	// ErrOpenAIRateLimit is returned on OpenAI 429 (our org limit hit).
	// Maps to `openai_unavailable` (502).
	ErrOpenAIRateLimit = errors.New("openai: rate limit")

	// ErrOpenAIUnavailable is returned for OpenAI 5xx / network errors /
	// timeouts. Maps to `openai_unavailable` (502).
	ErrOpenAIUnavailable = errors.New("openai: unavailable")

	// ErrOpenAIBadResponse is returned when OpenAI returns 200 with
	// malformed / empty content. Maps to `openai_unavailable` (502).
	ErrOpenAIBadResponse = errors.New("openai: bad response")
)
