package bridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/wire"
)

type mockStreamResolver struct {
	sentences []string
}

func (m mockStreamResolver) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: "llm", Rule: "ask_llm_slysh"}
}

func (m mockStreamResolver) Resolve(ctx context.Context, s string) (Resolution, error) {
	return Resolution{Route: "llm", Rule: "ask_llm_slysh"}, nil
}

func (m mockStreamResolver) ResolveDecision(ctx context.Context, s string, r RouteDecision) (Resolution, error) {
	return Resolution{Route: "llm", Rule: "ask_llm_slysh"}, nil
}

func (m mockStreamResolver) StreamFallback(ctx context.Context, text, systemPrompt, rule string) *StreamResult {
	ch := make(chan string, len(m.sentences))
	for _, s := range m.sentences {
		ch <- s
	}
	close(ch)
	var termErr error
	return &StreamResult{
		Sentences: ch,
		Err:       &termErr,
	}
}

func TestLLMStreamingEmulation(t *testing.T) {
	os.Setenv("QUASAR_LLM_STREAM", "1")
	defer os.Unsetenv("QUASAR_LLM_STREAM")

	down := make(chan wire.Frame, 100)
	up := make(chan wire.Frame, 100)
	resolver := mockStreamResolver{
		sentences: []string{
			"Первая фраза прожарки.",
			"Вторая фраза прожарки.",
		},
	}

	n := NewNative(t.Context(), func(ctx context.Context, k websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(ctx context.Context, k websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, resolver)
	defer n.Close()

	input := &client.TClientMessage{
		Message: &client.TClientMessage_Event{
			Event: &client.TClientMessage_TEvent{
				Header: &header.TEventHeader{MessageId: "stream-request"},
				Event: &client.TClientMessage_TEvent_TextInput{
					TextInput: &request.TAliceRequestProto{
						Header:  &request.TAliceRequestProto_THeader{RequestId: new("app-stream-req")},
						Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("слышь, ответь")}},
					},
				},
			},
		},
	}

	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}

	nextDown := func() wire.Frame {
		t.Helper()
		select {
		case f := <-down:
			return f
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for downstream frame")
			return wire.Frame{}
		}
	}

	nextUp := func() wire.Frame {
		t.Helper()
		select {
		case f := <-up:
			return f
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for upstream frame")
			return wire.Frame{}
		}
	}

	// Helper to find the next DeferredAliceResponse matching isLast
	nextDeferred := func(expectLast bool) wire.Frame {
		t.Helper()
		for {
			f := nextDown()
			if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil {
				if r.GetIsLast() == expectLast {
					return f
				}
			}
		}
	}

	// 1. Station receives InputStartAck
	ack := nextDown()
	if ack.Kind() != "InputStartAck" {
		t.Fatalf("expected InputStartAck, got %s", ack.Kind())
	}

	// 2. Station receives AliceResponse (beginDeferred)
	initial := nextDown()
	if initial.Kind() != "AliceResponse" || !initial.Server.GetEvent().GetAliceResponse().GetResponse().GetIsStreaming() {
		t.Fatalf("expected streaming AliceResponse, got %s", initial.Kind())
	}

	// 3. Station receives initial DeferredAliceResponse (empty placeholder)
	part0 := nextDeferred(false)
	if part0.Kind() != "DeferredAliceResponse" {
		t.Fatalf("expected initial DeferredAliceResponse, got %s", part0.Kind())
	}

	// 4. Upstream receives TtsGenerate for progress "а"
	ttsProgress := nextUp()
	if ttsProgress.Kind() != "TtsGenerate" || ttsProgress.Client.GetEvent().GetTtsGenerate().GetText() != "а" {
		t.Fatalf("expected TtsGenerate for progress 'а', got %s: %s", ttsProgress.Kind(), ttsProgress.Client.GetEvent().GetTtsGenerate().GetText())
	}
	progressTTSID := ttsProgress.Client.GetEvent().GetHeader().GetMessageId()
	if progressTTSID == "" {
		t.Fatal("progress ttsID must not be empty")
	}

	// Feed progress audio stream 100 from upstream
	feedSpeech(t, n, progressTTSID, 100, false)

	// Phrase 1 should be sent upstream
	ttsPhrase1 := nextUp()
	if ttsPhrase1.Kind() != "TtsGenerate" || ttsPhrase1.Client.GetEvent().GetTtsGenerate().GetText() != "Первая фраза прожарки." {
		t.Fatalf("expected TtsGenerate for phrase 1, got %s: %s", ttsPhrase1.Kind(), ttsPhrase1.Client.GetEvent().GetTtsGenerate().GetText())
	}
	phrase1TTSID := ttsPhrase1.Client.GetEvent().GetHeader().GetMessageId()

	// Station receives DeferredAliceResponse for phrase 1 text
	part1 := nextDeferred(false)
	if part1.Kind() != "DeferredAliceResponse" {
		t.Fatalf("expected non-last DeferredAliceResponse for phrase 1, got %s", part1.Kind())
	}

	// Feed phrase 1 audio stream 101 from upstream
	feedSpeech(t, n, phrase1TTSID, 101, false)

	// Phrase 2 was sent upstream
	ttsPhrase2 := nextUp()
	if ttsPhrase2.Kind() != "TtsGenerate" || ttsPhrase2.Client.GetEvent().GetTtsGenerate().GetText() != "Вторая фраза прожарки." {
		t.Fatalf("expected TtsGenerate for phrase 2, got %s: %s", ttsPhrase2.Kind(), ttsPhrase2.Client.GetEvent().GetTtsGenerate().GetText())
	}
	phrase2TTSID := ttsPhrase2.Client.GetEvent().GetHeader().GetMessageId()

	// Station receives DeferredAliceResponse for phrase 2 text
	part2 := nextDeferred(false)
	if part2.Kind() != "DeferredAliceResponse" {
		t.Fatalf("expected non-last DeferredAliceResponse for phrase 2, got %s", part2.Kind())
	}

	// Station receives final DeferredAliceResponse with is_last=true
	finalPart := nextDeferred(true)
	if finalPart.Kind() != "DeferredAliceResponse" {
		t.Fatalf("expected final DeferredAliceResponse with is_last=true, got %s", finalPart.Kind())
	}

	// Feed phrase 2 audio stream 102 from upstream
	feedSpeech(t, n, phrase2TTSID, 102, false)

	// Now station must receive the audio remuxed on stream 100, and EXACTLY ONE StreamControl.Close on stream 100!
	closed := false
	audioChunksCount := 0
	for {
		f := nextDown()
		if f.Kind() == "opaque" {
			streamID := binary.BigEndian.Uint32(f.Raw[:4])
			if streamID != 100 {
				t.Fatalf("audio sent on wrong stream: got %d, expected 100", streamID)
			}
			audioChunksCount++
		}
		if sc := f.Server.GetStreamControl(); sc != nil && sc.GetClose() != nil {
			if sc.GetStreamId() != 100 {
				t.Fatalf("close sent on wrong stream: got %d, expected 100", sc.GetStreamId())
			}
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("stream 100 was not closed after final phrase")
	}
	if audioChunksCount == 0 {
		t.Fatal("no audio chunks delivered to station")
	}
}
