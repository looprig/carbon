package model

// Origin is a model descriptor's provenance. The zero value is OriginCustom (fail-safe): a raw
// Model{} literal is treated as user-asserted, so gating stays conservative until proven curated.
type Origin uint8

const (
	OriginCustom  Origin = iota // user-supplied; capabilities are asserted, not verified
	OriginCatalog               // curated by the consumer or integration layer (not necessarily this SDK); capabilities are trusted
)

func (o Origin) String() string {
	if o == OriginCatalog {
		return "catalog"
	}
	return "custom"
}
