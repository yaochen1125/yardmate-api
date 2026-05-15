package proxy

// PlantDetail is the full per-plant detail entry. Mirrors one row of
// yardmate-content/plants_detail.json (the curated 1522 catalog) and the
// LLM-generated rows stored in Supabase plants_pending.data.
//
// All string-valued fields are English-only per the app_language memory.
// Catalog and LLM responses share this same shape; only ID + WateringNote +
// FertilizeFormula are typically null for LLM rows (see enrichment/SPEC §7).
//
// Pointer types mark fields that can legitimately be null on the wire
// (per the json shape of the source data); plain types are guaranteed
// present.
type PlantDetail struct {
	ID                 *string         `json:"id"`
	ScientificName     string          `json:"scientific_name"`
	CommonName         string          `json:"common_name"`
	CommonNameSource   string          `json:"common_name_source"`
	FlowerColor        []string        `json:"flower_color"`
	FlowerColorPrimary *string         `json:"flower_color_primary"`
	FoliageColor       []string        `json:"foliage_color"`
	Fragrance          Fragrance       `json:"fragrance"`
	FruitColor         []string        `json:"fruit_color"`
	FruitColorPrimary  *string         `json:"fruit_color_primary"`
	BloomTip           string          `json:"bloom_tip"`
	BloomMonthsNorth   []int           `json:"bloom_months_north"`
	BloomPeriodShort   string          `json:"bloom_period_short"`
	FruitTip           string          `json:"fruit_tip"`
	FruitMonthsNorth   []int           `json:"fruit_months_north"`
	FruitPeriodShort   *string         `json:"fruit_period_short"`
	Difficulty         int             `json:"difficulty"`
	Sunlight           int             `json:"sunlight"`
	HardinessZones     *HardinessZones `json:"hardiness_zones"`
	IndoorTempF        *IndoorTempRange `json:"indoor_temp_f"`
	WateringDays       SeasonDays      `json:"watering_days"`
	WateringNote       *int            `json:"watering_note"`
	FertilizingDays    SeasonDays      `json:"fertilizing_days"`
	FertilizeFormula   *int            `json:"fertilize_formula"`
	NativeRegion       []string        `json:"native_region"`
	Locations          []string        `json:"locations"`
	WeedLevel          int             `json:"weed_level"`
	Toxicity           Toxicity        `json:"toxicity"`
	Description        string          `json:"description"`
	HistoryTextShort   string          `json:"history_text_short"`
	HistoryTextLong    string          `json:"history_text_long"`
	NameOrigin         string          `json:"name_origin"`
	Attributes         []string        `json:"attributes"`
	Height             Dimension       `json:"height"`
	Spread             Dimension       `json:"spread"`
	Soil               []string        `json:"soil"`
	UsesList           []UseItem       `json:"uses_list"`
	SymbolismList      []SymbolismItem `json:"symbolism_list"`
	SymbolismStory     string          `json:"symbolism_story"`
	FlowerMeaning      string          `json:"flower_meaning"`
	CommonDiseasesList []string        `json:"common_diseases_list"`
	Genus              string          `json:"genus"`
}

// Fragrance describes scent characteristics (level + plant parts emitting it).
type Fragrance struct {
	Level string   `json:"level"`
	Parts []string `json:"parts"`
	Notes string   `json:"notes"`
}

// HardinessZones is the USDA hardiness zone range (1..13). Pointer in the
// parent struct because catalog houseplants set this to null (no outdoor zones).
type HardinessZones struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// IndoorTempRange is the recommended indoor temperature range in °F for
// houseplants. Pointer in the parent struct because outdoor-only plants set
// this to null.
type IndoorTempRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// SeasonDays is the per-season cadence (in days) for watering or fertilizing.
type SeasonDays struct {
	Spring int `json:"spring"`
	Summer int `json:"summer"`
	Fall   int `json:"fall"`
	Winter int `json:"winter"`
}

// Toxicity captures human/pet hazard data plus active compounds and a free-text note.
type Toxicity struct {
	Human           ToxicityRoute `json:"human"`
	Dog             ToxicityRoute `json:"dog"`
	Cat             ToxicityRoute `json:"cat"`
	ActiveCompounds []string      `json:"active_compounds"`
	NotesEn         string        `json:"notes_en"`
}

// ToxicityRoute is the per-species toxicity profile (level 0–4 + routes/parts/symptoms).
type ToxicityRoute struct {
	Level    int      `json:"level"`
	Routes   []string `json:"routes"`
	Parts    []string `json:"parts"`
	Symptoms []string `json:"symptoms"`
}

// Dimension is a min/max numeric range with a unit string (e.g. "ft", "m", "cm").
type Dimension struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Unit string  `json:"unit"`
}

// UseItem is one entry in the uses_list (an icon key + short user-facing text).
type UseItem struct {
	Icon string `json:"icon"`
	Text string `json:"text"`
}

// SymbolismItem is one keyword + description pair in symbolism_list.
type SymbolismItem struct {
	Keyword     string `json:"keyword"`
	Description string `json:"description"`
}

// PlantDetailID returns the plant's catalog id, or "" if absent. Convenience
// helper for callers that prefer empty-string-is-missing semantics.
func (p *PlantDetail) PlantDetailID() string {
	if p == nil || p.ID == nil {
		return ""
	}
	return *p.ID
}
