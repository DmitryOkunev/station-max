package bridge

var (
	mediaSearchMarkers = []string{
		"в лейзи медиа", "на лейзи медиа", "в lazy media", "на lazy media",
		"в медиа", "на медиа", "по медиа",
		"в лейзи", "на лейзи", "в lazy", "на lazy",
		"в lazymedia", "на lazymedia",
	}
)

// parseLazyMediaSearch extracts a search query targeted at LazyMedia Deluxe.
func parseLazyMediaSearch(norm string) (string, bool) {
	return matchMarkerPattern(norm, mediaSearchMarkers, tvActionPrefixes)
}

