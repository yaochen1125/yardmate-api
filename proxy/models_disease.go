package proxy

import "context"

// StructuredDiseaseDetail is the generated, catalog-quality detail for an
// out-of-catalog disease (disease enrichment — proxy/enrichment/SPEC_disease.md).
//
// JSON tags are camelCase to match diseases.json + HealthIssue (NOT PlantDetail's
// snake_case). Treatment / Prevention steps and HomeRemedies are back-filled by
// the server from the shared step/remedy pools (S01–S44 / K01–K15), so the wire
// shape is identical to an in-catalog disease's CDN detail and iOS renders both
// with one view. The hero image is NOT here — it stays the user's captured photo
// (client-side).
type StructuredDiseaseDetail struct {
	ShortDescription string            `json:"shortDescription"`
	SymptomAnalysis  string            `json:"symptomAnalysis"`
	Cause            string            `json:"cause"`
	Treatment        DiseaseStepGroups `json:"treatment"`
	Prevention       DiseaseStepGroups `json:"prevention"`
	HomeRemedies     []DiseaseRemedy   `json:"homeRemedies"`
}

// DiseaseStepGroups mirrors diseases.json treatment/prevention: severity-labeled
// groups, each a list of steps.
type DiseaseStepGroups struct {
	Groups []DiseaseStepGroup `json:"groups"`
}

type DiseaseStepGroup struct {
	Label *string       `json:"label"` // e.g. "For mild cases"; null when ungrouped (matches diseases.json)
	Steps []DiseaseStep `json:"steps"`
}

// DiseaseStep is a back-filled treatment/prevention step. Ref is the S-id it was
// drawn from; Body/Image/Title are copied from that shared step (denormalized,
// same as in-catalog diseases).
type DiseaseStep struct {
	Num      int           `json:"num"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	Image    string        `json:"image"` // bare filename, e.g. "uoIzi.png" (iOS applies CDN prefix)
	Ref      string        `json:"ref"`   // source shared-step id, e.g. "S04"
	SubSteps []DiseaseStep `json:"subSteps"`
}

// DiseaseRemedy is a back-filled home remedy (drawn from the K-id pool).
type DiseaseRemedy struct {
	Ref    string `json:"ref"` // source shared-remedy id, e.g. "K15"
	Title  string `json:"title"`
	Recipe string `json:"recipe"`
	Usage  string `json:"usage"`
	Image  string `json:"image"`
}

// DiseaseEnricher generates (or fetches cached) structured detail for an
// out-of-catalog disease name, returning the detail + the minted/looked-up
// O-series catalog id (e.g. "O7").
//
// Implemented by proxy/enrichment.DiseaseService and injected into HandleDiagnose
// so the proxy package does NOT import enrichment (enrichment already imports
// proxy — injecting via this interface avoids an import cycle). A nil enricher
// (no OpenAI key / no DB configured) means diagnose simply leaves out-of-catalog
// issues slim; callers MUST treat any error as "keep the issue slim" and never
// surface it (disease enrichment never 502s the diagnose — SPEC_disease §6).
type DiseaseEnricher interface {
	GetOrGenerate(ctx context.Context, diseaseName, plantContext string) (*StructuredDiseaseDetail, string, error)
}
