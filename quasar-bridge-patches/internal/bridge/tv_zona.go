package bridge

var (
	zonaSearchMarkers = []string{
		"в зоне", "на зоне", "по зоне",
		"в zona", "на zona", "в зону",
	}
)

// parseZonaSearch extracts a movie or series search query targeted at Zona.
func parseZonaSearch(norm string) (string, bool) {
	return matchMarkerPattern(norm, zonaSearchMarkers, tvActionPrefixes)
}

