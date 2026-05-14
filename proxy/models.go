package proxy

// IdentifyResult is the client-facing JSON shape for POST /v1/identify (SPEC §2.1).
// Sanitized from Plant.id's raw response — only fields V1 client needs.
type IdentifyResult struct {
	IsPlant           bool         `json:"is_plant"`
	IsPlantConfidence float64      `json:"is_plant_confidence"`
	Suggestions       []Suggestion `json:"suggestions"`
}

// Suggestion is one plant-identification suggestion. Top-3 only per SPEC §2.1.
type Suggestion struct {
	Name           string   `json:"name"`
	ScientificName string   `json:"scientific_name"`
	CommonNames    []string `json:"common_names"`
	Confidence     float64  `json:"confidence"`
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
// (empty slice instead of null on the wire).
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
