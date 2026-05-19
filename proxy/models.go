package proxy

// IdentifyResult is the client-facing JSON shape for POST /v1/identify (SPEC §2.1).
// Sanitized from Plant.id's raw response — only fields V1 client needs.
//
// AIEnhancedAt is non-null iff the optional ai_enhance multipart flag was
// true AND the LLM rerank call succeeded (the timestamp marks when the
// post-processing finished, UTC RFC3339). It stays null when ai_enhance was
// false or when the LLM failed / timed out — i.e. AIEnhancedAt being null
// does NOT mean Plant.id failed, only that no AI rerank happened.
type IdentifyResult struct {
	IsPlant           bool         `json:"is_plant"`
	IsPlantConfidence float64      `json:"is_plant_confidence"`
	Suggestions       []Suggestion `json:"suggestions"`
	AIEnhancedAt      *string      `json:"ai_enhanced_at"`
}

// Suggestion is one plant-identification suggestion. Top-3 only per SPEC §2.1.
//
// PlantID is the YardMate catalog plantId resolved from ScientificName by the
// HTTP handler (NOT PlantIDClient) via ContentIndex.LookupPlantID — the same
// resolver /v1/diagnose uses. Per-suggestion (not top-level) because the three
// candidates are distinct plants and the client navigates by the selected one
// (SPEC §2.1 "plant_id mapping" + §7). nil → JSON null on a catalog miss; the
// iOS client must tolerate null and must not render an empty detail page.
type Suggestion struct {
	Name           string   `json:"name"`
	ScientificName string   `json:"scientific_name"`
	CommonNames    []string `json:"common_names"`
	Confidence     float64  `json:"confidence"`
	PlantID        *string  `json:"plant_id"`
	// ImageURL is a per-suggestion species reference image URL supplied by
	// Pl@ntNet (the primary engine, via include-related-images=true). It is
	// null for the Plant.id fallback path and whenever Pl@ntNet returns no
	// related image. iOS uses it for the detail hero / gallery of
	// out-of-catalog plants (PlantID == null), which otherwise have no image.
	ImageURL *string `json:"image_url"`
}

// --- diagnose (POST /v1/diagnose, SPEC §2.2) ---

// DiagnoseResult is the client-facing JSON shape for POST /v1/diagnose.
//
// Healthy path (IsHealthy=true): Issues is an empty array. The iOS client
// is expected to route into the plant-detail page and surface a toast
// confirming the plant is healthy (no disease card needed).
//
// Unhealthy path: Issues is guaranteed non-empty (top-3 max). The server
// constructs a fallback issue with IsFallback=true if Plant.id reports
// unhealthy but returns zero disease suggestions.
type DiagnoseResult struct {
	IdentifiedName    string           `json:"identifiedName"`
	PlantID           *string          `json:"plantId"`
	IsHealthy         bool             `json:"isHealthy"`
	HealthProbability float64          `json:"healthProbability"`
	Top               *PlantSuggestion `json:"top"`
	Issues            []HealthIssue    `json:"issues"`
}

// PlantSuggestion is the top-1 Plant.id classification suggestion attached
// to a diagnose result (so iOS can render the identified plant without
// reissuing a /v1/identify call).
type PlantSuggestion struct {
	Name           string   `json:"name"`
	ScientificName string   `json:"scientific_name"`
	CommonNames    []string `json:"common_names"`
	Confidence     float64  `json:"confidence"`
}

// HealthIssue is one disease suggestion attached to a diagnose result.
// CatalogID is null when no YardMate catalog id could be mapped (neither
// the name-match nor the LLM fallback found a candidate).
type HealthIssue struct {
	Name        string    `json:"name"`
	CatalogID   *string   `json:"catalogId"`
	Probability float64   `json:"probability"`
	Description string    `json:"description"`
	Cause       string    `json:"cause"`
	IsFallback  bool      `json:"isFallback"`
	Treatment   Treatment `json:"treatment"`
}

// Treatment groups the Plant.id-provided remediation lists. All three slices
// are non-nil on the wire (empty array, not null) to keep the JSON shape
// stable for the iOS decoder.
type Treatment struct {
	Biological []string `json:"biological"`
	Chemical   []string `json:"chemical"`
	Prevention []string `json:"prevention"`
}

// plantIDDiagnoseResponse mirrors the Plant.id v3 response when called with
// health=all. Adds disease.suggestions and is_healthy on top of the shape
// plantIDAPIResponse already covers. description is `any` because Plant.id
// occasionally returns it as an object (e.g. {value, citations}) rather
// than a plain string; diagnoseDescriptionString flattens it.
type plantIDDiagnoseResponse struct {
	Result struct {
		IsPlant struct {
			Probability float64 `json:"probability"`
			Binary      bool    `json:"binary"`
		} `json:"is_plant"`
		IsHealthy struct {
			Probability float64 `json:"probability"`
			Binary      bool    `json:"binary"`
		} `json:"is_healthy"`
		Classification struct {
			Suggestions []struct {
				Name        string  `json:"name"`
				Probability float64 `json:"probability"`
				Details     struct {
					CommonNames    []string `json:"common_names"`
					ScientificName string   `json:"scientific_name"`
				} `json:"details"`
			} `json:"suggestions"`
		} `json:"classification"`
		Disease struct {
			Suggestions []struct {
				Name        string  `json:"name"`
				Probability float64 `json:"probability"`
				Details     struct {
					LocalName   string `json:"local_name"`
					Description any    `json:"description"`
					Cause       string `json:"cause"`
					Treatment   struct {
						Biological []string `json:"biological"`
						Chemical   []string `json:"chemical"`
						Prevention []string `json:"prevention"`
					} `json:"treatment"`
				} `json:"details"`
			} `json:"suggestions"`
		} `json:"disease"`
	} `json:"result"`
}

// plantIDAPIResponse is the subset of Plant.id v3 /identification response
// that we consume. Extra fields (access_token, model_version, similar_images,
// entity_id, etc.) are ignored.
type plantIDAPIResponse struct {
	Result struct {
		IsPlant struct {
			Probability float64 `json:"probability"`
			Binary      bool    `json:"binary"`
		} `json:"is_plant"`
		Classification struct {
			Suggestions []struct {
				Name        string  `json:"name"`
				Probability float64 `json:"probability"`
				Details     struct {
					CommonNames    []string `json:"common_names"`
					ScientificName string   `json:"scientific_name"`
				} `json:"details"`
			} `json:"suggestions"`
		} `json:"classification"`
	} `json:"result"`
}

// toIdentifyResult sanitizes Plant.id's raw response into the V1 client shape.
// Top-3 suggestions max (SPEC §2.1). common_names is normalized to non-nil
// (empty slice instead of null on the wire). AIEnhancedAt and per-suggestion
// PlantID are left nil here; the handler fills them (PlantID via ContentIndex
// after the optional rerank, AIEnhancedAt after the rerank itself).
func (r *plantIDAPIResponse) toIdentifyResult() *IdentifyResult {
	const maxSuggestions = 3

	out := &IdentifyResult{
		IsPlant:           r.Result.IsPlant.Binary,
		IsPlantConfidence: r.Result.IsPlant.Probability,
		Suggestions:       []Suggestion{},
	}

	n := len(r.Result.Classification.Suggestions)
	if n > maxSuggestions {
		n = maxSuggestions
	}
	for i := 0; i < n; i++ {
		s := r.Result.Classification.Suggestions[i]
		common := s.Details.CommonNames
		if common == nil {
			common = []string{}
		}
		out.Suggestions = append(out.Suggestions, Suggestion{
			Name:           s.Name,
			ScientificName: s.Details.ScientificName,
			CommonNames:    common,
			Confidence:     s.Probability,
		})
	}
	return out
}

// diagnoseDescriptionString coerces Plant.id's description field (sometimes a
// string, sometimes an object with a "value" subfield) into a plain string.
// Returns "" when neither form is present.
func diagnoseDescriptionString(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["value"].(string); ok {
			return s
		}
	}
	return ""
}
