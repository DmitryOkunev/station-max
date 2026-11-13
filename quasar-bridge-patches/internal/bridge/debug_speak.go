package bridge

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

var (
	debugSpeakerMu sync.Mutex
	debugSpeaker   *Native
)

// SetDebugSpeaker registers the live native session backing the debug speak
// endpoint.
func SetDebugSpeaker(n *Native) {
	debugSpeakerMu.Lock()
	debugSpeaker = n
	debugSpeakerMu.Unlock()
}

// ClearDebugSpeaker unregisters the session once the station disconnects.
func ClearDebugSpeaker(n *Native) {
	debugSpeakerMu.Lock()
	if debugSpeaker == n {
		debugSpeaker = nil
	}
	debugSpeakerMu.Unlock()
}

// DebugSpeakHandler queues a local Ogg/Opus file for the next station request.
// The speaker only renders TTS that belongs to its own request, so the file is
// spoken as the answer to the next phrase (for example «Алиса, тест»).
func DebugSpeakHandler(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("QUASAR_DEBUG_SPEAK") == "" {
		http.Error(w, "debug speak disabled", http.StatusNotFound)
		return
	}
	file := r.URL.Query().Get("file")
	if file == "" {
		file = os.Getenv("QUASAR_BEEP")
	}
	if file == "" {
		http.Error(w, "no audio file", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	debugSpeakerMu.Lock()
	n := debugSpeaker
	debugSpeakerMu.Unlock()
	if n == nil {
		http.Error(w, "no speaker connected", http.StatusServiceUnavailable)
		return
	}
	n.QueueSpeak(file)
	slog.Info("debug_speak_queued", "file", file, "bytes", len(data))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "queued %d bytes from %s: say any phrase on the station to hear it\n", len(data), file)
}
