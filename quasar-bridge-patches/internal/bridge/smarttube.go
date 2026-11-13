package bridge

import (
	"strings"
)

var (
	ytActionPrefixes = []string{
		"включи", "поставь", "найди", "открой", "запусти",
		"покажи", "проиграй", "воспроизведи", "сыграй", "вруби",
	}

	ytMarkers = []string{
		"на ютубе", "в ютубе", "на ютуб", "в ютуб", "по ютубу",
		"на youtube", "в youtube", "по youtube",
		"на смарттьюбе", "в смарттьюбе", "смарттьюб", "smarttube",
		"ютубе", "youtube", "ютуб",
	}
)

// SmartTubePlayCommand extracts the search/play query if the text is a direct YouTube request.
func SmartTubePlayCommand(text string) (string, bool) {
	norm := cleanText(text)
	if norm == "" {
		return "", false
	}

	// Just opening the YouTube app without specific query
	for _, appName := range []string{"ютуб", "youtube", "смарттьюб", "smarttube"} {
		for _, prefix := range ytActionPrefixes {
			if norm == prefix+" "+appName {
				return "", true
			}
		}
		if norm == appName {
			return "", true
		}
	}

	// Check for "action + marker + query" or "action + query + marker"
	for _, marker := range ytMarkers {
		for _, prefix := range ytActionPrefixes {
			// e.g. "включи на ютубе линкин парк"
			lead := prefix + " " + marker + " "
			if strings.HasPrefix(norm, lead) {
				q := strings.TrimSpace(strings.TrimPrefix(norm, lead))
				if q != "" {
					return q, true
				}
			}

			// e.g. "включи линкин парк на ютубе"
			prefixLead := prefix + " "
			markerTail := " " + marker
			if strings.HasPrefix(norm, prefixLead) && strings.HasSuffix(norm, markerTail) {
				middle := norm[len(prefixLead) : len(norm)-len(markerTail)]
				q := strings.TrimSpace(middle)
				if q != "" {
					return q, true
				}
			}
		}

		// Direct query with marker: "ютуб линкин парк" or "линкин парк на ютубе"
		markerLead := marker + " "
		if strings.HasPrefix(norm, markerLead) {
			q := strings.TrimSpace(strings.TrimPrefix(norm, markerLead))
			if q != "" {
				return q, true
			}
		}

		markerTail := " " + marker
		if strings.HasSuffix(norm, markerTail) {
			q := strings.TrimSpace(strings.TrimSuffix(norm, markerTail))
			if q != "" && !strings.HasPrefix(q, "пауза") && !strings.HasPrefix(q, "стоп") {
				return q, true
			}
		}
	}

	return "", false
}
