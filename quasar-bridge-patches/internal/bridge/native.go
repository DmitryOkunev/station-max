package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asrcommon "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	continuation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/continue_request"
	ack "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/input_start_ack"
	stream "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	ttsspeak "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/reply"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

type Resolution struct {
	Route  string
	Answer string
	Rule   string
}
type Resolver interface {
	Resolve(context.Context, string) (Resolution, error)
}
type RoutedResolver interface {
	Decide(context.Context, string) RouteDecision
	ResolveDecision(context.Context, string, RouteDecision) (Resolution, error)
}
type FallbackResolver interface {
	Fallback(context.Context, string) (Resolution, error)
}

type nativeRequest struct {
	streaming                       bool
	listenOnStreamStart             bool
	partial                         uint32
	responseID                      string
	final                           bool
	header                          *header.TEventHeader
	isText                          bool
	ttsStarted                      bool
	gated                           bool
	input                           []byte
	id                              string
	request                         *request.TAliceRequestProto
	text                            string
	route                           string
	ctx                             context.Context
	cancel                          context.CancelFunc
	started, canceled, local, cloud bool
	continuation                    []byte
	ttsID                           string
	progressTTS                     string
	audio                           oggSpeech
	audioIsProgress                 bool
	audioStarted                    bool
	audioStream                     uint32
	audioWaiting                    bool
	progressAudioDone               chan struct{}
	soundRequest                    *sound.Request
	soundDirective                  *soundDirective
	soundSealed                     bool
	expires                         *time.Timer
	rule                            string
	detector                        *whisperDetector
	isWhisper                       bool
}
type Native struct {
	connectionScope   string
	ctx               context.Context
	cancel            context.CancelFunc
	up, down          Sender
	resolver          Resolver
	mu                sync.Mutex
	requests          map[string]*nativeRequest
	tts               map[string]*nativeRequest
	streams           map[uint32]*nativeRequest
	blocked           map[uint32]*nativeRequest
	micStreams        map[uint32]*nativeRequest
	currentMicRequest *nativeRequest
	pendingSpeak      string
	diagFeedSeq       int
	diagPassSeq       int
	wg                sync.WaitGroup
	OnRoute           func(id, route string)
	OnCloudResponse   func(ctx context.Context, isSmartHome bool)
	GateText          bool
	ShouldListen      bool
	DecisionDelay     time.Duration
	ProgressAfter     time.Duration
	ProgressInterval  time.Duration
	TTSReplacer       *TTSReplacer
}

func NewNative(ctx context.Context, up, down Sender, resolver Resolver) *Native {
	ctx, cancel := context.WithCancel(ctx)
	return &Native{connectionScope: newID(), ctx: ctx, cancel: cancel, up: up, down: down, resolver: resolver, ShouldListen: true, requests: map[string]*nativeRequest{}, tts: map[string]*nativeRequest{}, streams: map[uint32]*nativeRequest{}, blocked: map[uint32]*nativeRequest{}, micStreams: map[uint32]*nativeRequest{}}
}
func (n *Native) Close() {
	n.cancel()
	n.mu.Lock()
	for _, p := range n.requests {
		p.canceled = true
		p.cancel()
		p.expires.Stop()
	}
	n.mu.Unlock()
	n.wg.Wait()
}
func (n *Native) Client(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ClientToServer)
	if f.Client != nil {
		if sc := f.Client.GetStreamControl(); sc != nil {
			slog.Info("native_stream_control", "origin", "station", "stream", sc.GetStreamId(), "msg", requestTag(sc.GetMessageId()), "action", streamControlAction(sc))
		}
	}
	if f.Client == nil || f.Client.GetEvent() == nil {
		payload := data
		streamID := uint32(0)
		if len(data) >= 4 {
			idCandidate := binary.BigEndian.Uint32(data[:4])
			if idCandidate < 65536 {
				streamID = idCandidate
				payload = data[4:]
			}
		}
		n.mu.Lock()
		p := n.micStreams[streamID]
		if p == nil && n.currentMicRequest != nil && !n.currentMicRequest.canceled {
			p = n.currentMicRequest
		}
		n.mu.Unlock()
		if p != nil && p.detector != nil && len(payload) > 0 {
			p.detector.Feed(payload)
		}
		return n.up(ctx, k, data)
	}
	e := f.Client.GetEvent()
	if stat := e.GetRequestStat(); stat != nil {
		kind := "none"
		raw := strings.ToLower(stat.GetError())
		if raw != "" {
			kind = "other"
			if strings.Contains(raw, "timeout") || strings.Contains(raw, "timed out") || strings.Contains(raw, "deadline") {
				kind = "timeout"
			} else if strings.Contains(raw, "cancel") {
				kind = "canceled"
			}
		}
		timestamps := stat.GetTimestamps()
		reported := func(value string) bool { return value != "" && value != "0" }
		slog.Info("native_playback_status", "request", requestTag(stat.GetRequestMessageId()), "error_class", kind, "playback_reported", reported(timestamps.GetOnSoundPlayerBeginTime()) || reported(timestamps.GetOnPlayerBegin()), "finished_reported", reported(timestamps.GetOnSoundPlayerEndTime()) || reported(timestamps.GetOnPlayerEnd()))
	}
	var req *request.TAliceRequestProto
	switch f.Kind() {
	case "TextInput":
		req = e.GetTextInput()
	case "VoiceInput":
		req = e.GetVoiceInput()
	case "SpeechInput":
		req = e.GetSpeechInput()
	}
	if req != nil && f.Kind() == "TextInput" && strings.TrimSpace(req.GetRequest().GetEvent().GetText()) == "" {
		return n.up(ctx, k, data)
	}
	if req != nil {
		id := e.GetHeader().GetMessageId()
		if id == "" {
			return n.up(ctx, k, data)
		}
		scope := "connection:" + n.connectionScope
		if id := req.GetApplication().GetDeviceId(); id != "" {
			scope = "device:" + id
		} else if id := req.GetApplication().GetUuid(); id != "" {
			scope = "uuid:" + id
		}
		pctx, cancel := context.WithTimeout(dialogue.WithSpeaker(n.ctx, scope), 90*time.Second)
		pctx = reply.WithPolicy(pctx)
		playerState := nativePlaybackState(req)
		pctx = sound.WithPlaybackState(pctx, playerState)
		slog.Info("native_player_state", "request", requestTag(id), "present", playerState.Present, "player_state", playerState.PlayerState, "offset_known", playerState.OffsetKnown, "offset_ms", playerState.OffsetMS, "has_stream", playerState.StreamID != "", "stream_tag", requestTag(playerState.StreamID))
		p := &nativeRequest{id: id, request: proto.Clone(req).(*request.TAliceRequestProto), text: req.GetRequest().GetEvent().GetText(), ctx: pctx, cancel: cancel, detector: newWhisperDetector()}
		p.ctx = sound.WithPlayer(p.ctx, func(ctx context.Context, request sound.Request) error {
			return n.queueSound(ctx, p, request)
		})
		p.header = proto.Clone(e.GetHeader()).(*header.TEventHeader)
		p.isText = f.Kind() == "TextInput"
		p.input = append([]byte{}, data...)
		p.gated = !p.isText || n.GateText

		slog.Info("native_request_shape", "request", requestTag(id), "text_bytes", len(p.text), "event_type", req.GetRequest().GetEvent().GetType().String(), "stream", e.GetHeader().GetStreamId(), "unknown_request_bytes", len(req.ProtoReflect().GetUnknown()), "payload_bytes", len(req.GetRequest().GetEvent().GetPayloadRaw()))
		n.mu.Lock()
		if n.requests[id] != nil {
			n.mu.Unlock()
			cancel()
			return nil
		}
		if len(n.requests) >= 128 {
			n.mu.Unlock()
			cancel()
			return errors.New("too many pending native requests")
		}
		if preparer, ok := n.resolver.(interface {
			Prepare(context.Context) context.Context
		}); ok {
			p.ctx = preparer.Prepare(p.ctx)
		}
		p.expires = time.AfterFunc(90*time.Second, func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			p.canceled = true
			p.cancel()
			delete(n.requests, p.id)
			delete(n.tts, p.ttsID)
			delete(n.tts, p.progressTTS)
			for stream, q := range n.blocked {
				if q == p {
					delete(n.blocked, stream)
				}
			}
			for stream, q := range n.streams {
				if q == p {
					delete(n.streams, stream)
				}
			}
			for stream, q := range n.micStreams {
				if q == p {
					delete(n.micStreams, stream)
				}
			}
			if n.currentMicRequest == p {
				n.currentMicRequest = nil
			}
		})
		n.requests[id] = p
		n.currentMicRequest = p
		if streamID := uint32(e.GetHeader().GetStreamId()); streamID != 0 {
			n.micStreams[streamID] = p
		}
		n.mu.Unlock()
		if p.gated {
			gated, err := wire.EnableApproval(f)
			if err != nil {
				return err
			}
			if err := n.up(ctx, k, gated); err != nil {
				return err
			}
		}
		// The original input stays local until routing has selected exactly one executor.
		n.maybeResolve(p)
		return nil
	}
	if c := e.GetContinueRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		if p != nil && (p.canceled || p.local) {
			n.mu.Unlock()
			return nil
		}
		if p != nil && !p.cloud {
			if p.continuation != nil {
				n.mu.Unlock()
				return nil
			}
			p.continuation = append([]byte{}, data...)
			n.mu.Unlock()
			n.maybeResolve(p)
			return nil
		}
		n.mu.Unlock()
	}
	if c := e.GetCancelRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		ttsID := ""
		if p != nil {
			p.canceled = true
			p.cancel()
			ttsID = p.ttsID
			if n.currentMicRequest == p {
				n.currentMicRequest = nil
			}
		}
		n.mu.Unlock()
		if ttsID != "" {
			_ = n.sendCancel(ctx, ttsID)
		}
	}
	return n.up(ctx, k, data)
}
func (n *Native) findLocked(h *header.TEventHeader) (*nativeRequest, bool) {
	if h == nil {
		return nil, false
	}
	for _, id := range []string{h.GetRefMessageId(), h.GetSubRequestRef().GetMessageId(), h.GetSubRequestRef().GetRequestId()} {
		if p := n.tts[id]; p != nil {
			return p, true
		}
		if p := n.requests[id]; p != nil {
			return p, false
		}
		if id != "" {
			for _, p := range n.requests {
				if p.request.GetHeader().GetRequestId() == id {
					return p, false
				}
			}
		}
	}
	return nil, false
}
func (n *Native) Server(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ServerToClient)
	if f.Server == nil {
		if len(data) >= 4 {
			streamID := binary.BigEndian.Uint32(data)
			n.mu.Lock()
			p := n.streams[streamID]
			drop := n.blocked[streamID] != nil || (p != nil && p.canceled)
			if p != nil && p.streaming && !drop {
				n.diagFeedSeq++
				logFeed := n.diagFeedSeq <= 8 || n.diagFeedSeq%50 == 0
				out, err := p.audio.feed(data[4:])
				if err != nil {
					slog.Warn("native_audio_feed_error", "request", requestTag(p.id), "stream", streamID, "bytes", len(data), "err", err)
				}
				if err == nil && len(out) > 0 {
					err = n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), out...))
				}
				if err != nil {
					p.cancel()
				}
				if logFeed {
					slog.Info("native_audio_feed", "request", requestTag(p.id), "stream", streamID, "down_stream", p.audioStream, "bytes", len(data), "out", len(out))
				}
				n.mu.Unlock()
				return err
			}
			n.mu.Unlock()
			if drop {
				return nil
			}
			n.mu.Lock()
			n.diagPassSeq++
			seq := n.diagPassSeq
			n.mu.Unlock()
			if seq <= 24 || seq%100 == 0 {
				slog.Info("native_audio_passthrough", "stream", streamID, "bytes", len(data), "seq", seq)
			}
		}
		return n.down(ctx, k, data)
	}
	if sc := f.Server.GetStreamControl(); sc != nil {
		slog.Info("native_stream_control", "origin", "cloud", "stream", sc.GetStreamId(), "msg", requestTag(sc.GetMessageId()), "action", streamControlAction(sc))
		n.mu.Lock()
		if n.blocked[uint32(sc.GetStreamId())] != nil {
			delete(n.blocked, uint32(sc.GetStreamId()))
			n.mu.Unlock()
			return nil
		}
		p := n.tts[sc.GetMessageId()]
		if p == nil {
			p = n.streams[uint32(sc.GetStreamId())]
		}
		if sc.GetClose() != nil {
			delete(n.streams, uint32(sc.GetStreamId()))
		}
		original := n.requests[sc.GetMessageId()]
		if p != nil {
			if p.canceled {
				n.mu.Unlock()
				return nil
			}
			if p.streaming && sc.GetClose() != nil {
				if err := p.audio.endSource(); err != nil {
					p.cancel()
					n.mu.Unlock()
					return err
				}
				if p.audioIsProgress && sc.GetClose().GetReason() == 0 {
					if tail := p.audio.flushPending(0, 0); len(tail) > 0 {
						if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), tail...)); err != nil {
							n.mu.Unlock()
							return err
						}
					}
					if !p.audioWaiting {
						p.audioWaiting = true
						n.waitAudio(p)
						close(p.progressAudioDone)
					}
					n.mu.Unlock()
					return nil
				}
				end := p.audio.finish()
				if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), end...)); err != nil {
					n.mu.Unlock()
					return err
				}
				sc.StreamId = int64(p.audioStream)
			}
			sc.MessageId = p.id
			finalSpeech := sc.GetClose() != nil && (!p.streaming || !p.audioIsProgress)
			n.mu.Unlock()
			out, err := f.Marshal()
			if err != nil {
				return err
			}
			err = n.down(ctx, k, out)
			if err == nil && finalSpeech {
				reply.Delivered(p.ctx)
			}
			return err
		}
		drop := original != nil && original.local && (original.header.StreamId == nil || sc.GetStreamId() != original.header.GetStreamId())
		n.mu.Unlock()
		if drop {
			return nil
		}
		return n.down(ctx, k, data)
	}
	e := f.Server.GetEvent()
	if e == nil {
		return n.down(ctx, k, data)
	}

	n.mu.Lock()
	if f.Kind() == "TtsSpeak" {
		delete(n.streams, uint32(e.GetHeader().GetStreamId()))
		delete(n.blocked, uint32(e.GetHeader().GetStreamId()))
	}
	p, isTTS := n.findLocked(e.GetHeader())
	if r := e.GetAliceResponse(); r != nil {
		var ds []string
		for _, d := range r.GetResponse().GetDirectives() {
			ds = append(ds, d.GetType()+":"+d.GetName())
		}

		slog.Info("native_cloud_shape", "matched", p != nil, "directives", ds, "cards", len(r.GetResponse().GetCards()), "voice_text_bytes", len(r.GetVoiceResponse().GetOutputSpeech().GetText()))
	}
	if p == nil {
		n.mu.Unlock()
		return n.down(ctx, k, data)
	}
	if isTTS {
		if p.canceled {
			n.mu.Unlock()
			return nil
		}
		e.Header.RefMessageId = p.id
		if ref := e.Header.SubRequestRef; ref != nil {
			ref.MessageId = p.id
			ref.RequestId = p.request.GetHeader().GetRequestId()
		}
		if e.GetTtsSpeak() != nil {
			n.streams[uint32(e.Header.GetStreamId())] = p
			if p.streaming {
				if e.GetTtsSpeak().GetFormat() != "audio/opus" {
					p.cancel()
					n.mu.Unlock()
					return errors.New("streaming progress requires Opus")
				}
				p.audioIsProgress = p.ttsID == p.progressTTS
				p.audio.next()
				p.audio.source.discardAudio = p.audioIsProgress
				if p.audioStarted {
					p.audioWaiting = false
					n.mu.Unlock()
					slog.Info("native_tts_swallowed", "request", requestTag(p.id), "stream", e.Header.GetStreamId())
					return nil
				}
				p.audioStream = uint32(e.Header.GetStreamId())
				p.audioStarted = true
			}
			slog.Info("native_tts_ready", "request", requestTag(p.id), "format", e.GetTtsSpeak().GetFormat(), "stream", e.Header.GetStreamId())
		}
		n.mu.Unlock()
		out, err := f.Marshal()
		if err != nil {
			return err
		}
		return n.down(ctx, k, out)
	}

	if f.Kind() == "TtsSpeak" {
		p.ttsStarted = true
		if p.local || p.canceled {
			n.blocked[uint32(e.GetHeader().GetStreamId())] = p
		}
	}
	if p.local || p.canceled {
		n.mu.Unlock()
		return nil
	}
	if r := e.GetAliceResponse(); r != nil && p.cloud && !p.ttsStarted && canReplaceResponse(r) {
		text := ""
		for _, card := range r.GetResponse().GetCards() {
			text += card.GetText() + " "
		}
		if strings.TrimSpace(text) == "" {
			text = r.GetVoiceResponse().GetOutputSpeech().GetText()
		}

		if resolver, ok := n.resolver.(FallbackResolver); ok && n.isRefusal(text) {
			p.local = true
			if p.detector != nil {
				p.isWhisper = p.detector.IsWhisper()
			}
			if n.OnRoute != nil {
				n.OnRoute(p.id, "llm")
			}
			p.ttsID = newID()
			n.tts[p.ttsID] = p
			query := p.text
			n.mu.Unlock()
			n.wg.Go(func() {
				result, err := resolver.Fallback(p.ctx, query)
				if p.ctx.Err() != nil {
					return
				}
				if errors.Is(err, context.Canceled) {
					n.discardStale(p)
					return
				}
				if err != nil {
					n.discardSound(p)
					result.Answer = "Сейчас не получилось получить ответ локальной модели."
				}
				_ = n.sendCancel(p.ctx, p.id)
				if err := n.reply(p, result.Answer); err != nil {
					slog.Warn("native_reply_failed", "request", requestTag(p.id))
				}
				slog.Info("native_fallback", "request", requestTag(p.id), "route", "llm")
			})
			return nil
		}
	}
	emptyFinal := false
	switch f.Kind() {
	case "AliceResponse":
		if !p.cloud {
			p.canceled = true
			p.cancel()
			slog.Warn("native_approval_not_honored", "request", requestTag(p.id))
		}
		if r := e.GetAliceResponse(); r != nil && p.cloud {
			isSmartHome := isSmartHomeResponse(r)
			if isSmartHome {
				// Suppress TV cards/toolbar so com.yandex.tv.alice does not draw a persistent dialog
				if r.GetResponse() != nil {
					r.GetResponse().Cards = nil
				}
				// Strip verbal text (e.g. "С удовольствием", "Включаю"), keep sound chime only
				if vr := r.GetVoiceResponse(); vr != nil {
					vr.ShouldListen = false
					if os := vr.GetOutputSpeech(); os != nil {
						speechText := os.GetText()
						chimes := chimeRegex.FindAllString(speechText, -1)
						if len(chimes) > 0 {
							cleanChime := strings.Join(chimes, " ")
							os.Text = &cleanChime
						}
					}
				}
				// Re-encode modified protobuf frame to send down to station
				if modifiedData, err := f.Marshal(); err == nil {
					data = modifiedData
				}
				slog.Info("native_smarthome_suppressed", "request", requestTag(p.id), "chime_only", true)
			}
			if n.OnCloudResponse != nil {
				go n.OnCloudResponse(p.ctx, isSmartHome)
			}
		}
	case "ServerRequestReady":
		slog.Info("native_server_ready", "request", requestTag(p.id))
	case "AsrResult":
		asr := e.GetAsrResult()
		if asr.GetEndOfUtt() {
			p.final = true
			slog.Info("native_asr_final", "request", requestTag(p.id), "hypotheses", len(asr.GetRecognition()), "code", asr.GetResponseCode().String())
		}
		if asr.GetEndOfUtt() && len(asr.GetRecognition()) > 0 {
			r := asr.GetRecognition()[0]
			p.text = r.GetNormalized()
			if p.text == "" {
				var words []string
				for _, w := range r.GetWords() {
					words = append(words, w.GetValue())
				}
				p.text = strings.Join(words, " ")
			}
		}
		emptyFinal = !p.started && asr.GetEndOfUtt() && asr.GetResponseCode() == asrcommon.EAsrResponseCode_OK && strings.TrimSpace(p.text) == ""

	}
	n.mu.Unlock()
	if emptyFinal {
		if listener, ok := n.resolver.(interface{ EmptyInput(context.Context) }); ok {
			listener.EmptyInput(p.ctx)
		}
	}
	n.maybeResolve(p)
	if err := n.down(ctx, k, data); err != nil {
		return err
	}
	n.captureCloud(p, e)
	return nil
}
func (n *Native) maybeResolve(p *nativeRequest) {
	n.mu.Lock()
	if p.started || p.canceled || (strings.TrimSpace(p.text) == "" && !p.final) {
		n.mu.Unlock()
		return
	}
	p.started = true
	text := p.text
	n.mu.Unlock()
	n.wg.Go(func() {
		if n.DecisionDelay > 0 {
			select {
			case <-time.After(n.DecisionDelay):
			case <-p.ctx.Done():
				return
			}
		}
		if file := n.takePendingSpeak(); file != "" {
			slog.Info("native_local_speak_pending", "request", requestTag(p.id), "file", file)
			if err := n.speakLocal(p, file); err != nil {
				slog.Warn("native_local_speak_failed", "request", requestTag(p.id), "err", err)
			}
			return
		}
		result := Resolution{Route: "yandex"}
		var err error
		var llmDecision RouteDecision
		if n.resolver != nil && strings.TrimSpace(text) != "" {
			if routed, ok := n.resolver.(RoutedResolver); ok {
				decision := routed.Decide(p.ctx, text)
				llmDecision = decision
				n.mu.Lock()
				p.listenOnStreamStart = decision.Route == "llm" || decision.Source == "dialogue_retry"
				p.rule = decision.Rule
				if p.detector != nil && p.detector.IsWhisper() {
					p.isWhisper = true
				}
				n.mu.Unlock()
				slog.Info("native_route_decision", "request", requestTag(p.id), "route", decision.Route, "rule", decision.Rule, "source", decision.Source, "text", text, "is_whisper", p.isWhisper)
				stop := func() {}
				if decision.Route != "yandex" && decision.Source != "smarttube_media" && decision.Source != "smarttube_launch" && decision.Source != "airplay_media" && decision.Route != "playback" && n.ProgressAfter > 0 && (decision.Route != "llm" || llmStreamEnabled()) {
					stop = n.progress(p)
				}
				if beep := beepProbePath(text); beep != "" {
					stop()
					if err := n.speakLocal(p, beep); err != nil {
						slog.Warn("native_local_speak_failed", "request", requestTag(p.id), "err", err)
					}
					return
				}
				result, err = routed.ResolveDecision(p.ctx, text, decision)
				stop()
			} else {
				result, err = n.resolver.Resolve(p.ctx, text)
			}
		}
		if p.ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			n.discardStale(p)
			return
		}
		if err != nil {
			n.discardSound(p)
			if result.Route == "home" {
				result.Answer = "Не удалось подтвердить выполнение команды в умном доме."
			} else if result.Route == "airplay" {
				result.Answer = "Не удалось изменить состояние AirPlay. Проверьте журнал Quasar Bridge."
			} else if result.Route == "smarttube" {
				result.Answer = "Не удалось выполнить команду для YouTube."
			} else if result.Route == "music" {
				result.Answer = "Не удалось включить музыку с этой страницы."
			} else {
				result.Route = "llm"
				result.Answer = "Сейчас не получилось получить ответ локальной модели."
			}
		}
		n.mu.Lock()
		if p.canceled {
			n.mu.Unlock()
			return
		}
		if n.OnRoute != nil {
			n.OnRoute(p.id, result.Route)
		}
		p.route = result.Route
		if result.Rule != "" {
			p.rule = result.Rule
		}
		if p.detector != nil && p.detector.IsWhisper() {
			p.isWhisper = true
		}
		if result.Route == "yandex" {
			p.cloud = true
			var err error
			if p.gated {
				control := p.continuation
				if control == nil {
					control, err = n.makeContinue(p)
				}
				if err == nil {
					err = n.up(p.ctx, websocket.MessageBinary, control)
				}
			} else {
				err = n.up(p.ctx, websocket.MessageBinary, p.input)
				if err == nil && p.continuation != nil {
					err = n.up(p.ctx, websocket.MessageBinary, p.continuation)
				}
			}
			n.mu.Unlock()
			slog.Info("native_route", "request", requestTag(p.id), "route", "yandex", "sent", err == nil, "is_whisper", p.isWhisper)
			return
		}
		// Check for LLM streaming path before standard local-route initialisation.
		isLLMStream := llmStreamEnabled() && result.Route == "llm" && (result.Rule == "ask_llm_slysh" || result.Rule == "ask_llm_podskazhi")
		if isLLMStream {
			if _, ok := n.resolver.(interface {
				StreamFallback(ctx context.Context, text, systemPrompt, rule string) *StreamResult
			}); !ok {
				isLLMStream = false
			}
		}
		p.local = true
		if !p.streaming && !isLLMStream {
			p.ttsID = newID()
			n.tts[p.ttsID] = p
		}
		n.mu.Unlock()
		slog.Info("native_route", "request", requestTag(p.id), "route", result.Route, "is_whisper", p.isWhisper)
		if p.gated && !p.streaming && !isLLMStream {
			if err := n.sendCancel(p.ctx, p.id); err != nil {
				return
			}
		}
		// Use streaming for ask_llm_slysh when the resolver supports StreamFallback.
		if isLLMStream {
			streamer := n.resolver.(interface {
				StreamFallback(ctx context.Context, text, systemPrompt, rule string) *StreamResult
			})
			sRes := streamer.StreamFallback(p.ctx, text, llmDecision.SystemPrompt, result.Rule)
			if err := streamReply(p.ctx, n, p, sRes); err != nil {
				slog.Warn("native_stream_failed", "request", requestTag(p.id), "err", err)
			}
			return
		}
		if err := n.reply(p, result.Answer); err != nil {
			slog.Warn("native_reply_failed", "request", requestTag(p.id))
		}
	})
}

// replyStreamSentence sends one TTS segment during a streaming reply.
// It allocates a fresh ttsID, registers the request in n.tts, sends
// a deferredPart proto, and fires the TTS synthesis request upstream.
func (n *Native) replyStreamSentence(p *nativeRequest, sentence string, last bool) error {
	if !last {
		n.mu.Lock()
		p.ttsID = newID()
		n.tts[p.ttsID] = p
		n.mu.Unlock()
	}
	if err := n.deferredPart(p, sentence, last); err != nil {
		return err
	}
	if sentence == "" {
		return nil
	}
	return n.synthesize(p, sentence)
}

func (n *Native) sendCancel(ctx context.Context, id string) error {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: id, Reason: &cancellation.TCancelRequest_LocalExecution{LocalExecution: &cancellation.TCancelRequest_TReasonLocalExecution{}}}}}}}
	b, err := (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.up(ctx, websocket.MessageBinary, b)
}
func (n *Native) reply(p *nativeRequest, text string) error {
	if p.streaming {
		return n.finishDeferred(p, text)
	}
	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	speechText := text
	if (p.isWhisper || (p.detector != nil && p.detector.IsWhisper())) && text != "" {
		p.isWhisper = true
		if !strings.HasPrefix(speechText, "<speaker is_whisper=") {
			speechText = "<speaker is_whisper=\"true\"> " + speechText
		}
	}
	answer := &response.TAliceResponseProto{Header: &response.TAliceResponseProto_THeader{RequestId: new(requestID), ResponseId: new(newID()), DialogId: new(p.request.GetHeader().GetDialogId()), SequenceNumber: new(p.request.GetHeader().GetSequenceNumber())}, VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(speechText)}, HasVoiceResponse: true}, Response: &response.TAliceResponseProto_TResponse{Cards: []*response.TAliceResponseProto_TResponse_TCard{{Type: "simple_text", Text: new(text), CardId: new(newID())}}}}
	silentPlayback := n.hasSilentPlayback(p) || text == ""
	shouldListen := n.ShouldListen && !silentPlayback && reply.ListeningAllowed(p.ctx)
	if p.route == "smarttube" || p.route == "playback" || p.route == "home" {
		shouldListen = false
	}
	answer.VoiceResponse.ShouldListen = shouldListen
	if silentPlayback {
		answer.VoiceResponse.HasVoiceResponse = false
		answer.VoiceResponse.OutputSpeech.Text = new("")
	}
	if text == "" || p.route == "smarttube" {
		answer.Response.Cards = nil
	}
	answer.Response.Directives = n.takeSound(p)
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if !p.gated && !p.cloud {
		if err := n.ack(p); err != nil {
			return err
		}
	}
	if err := n.down(p.ctx, websocket.MessageBinary, b); err != nil {
		return err
	}
	n.captureDelivered(p, text, true, silentPlayback || len(answer.Response.Directives) > 0, true)
	if silentPlayback {
		streamID := int64(0)
		if p.header != nil && p.header.StreamId != nil {
			streamID = *p.header.StreamId
		}
		mClose := &server.TServerMessage{
			Message: &server.TServerMessage_StreamControl{
				StreamControl: &stream.TStreamControl{
					MessageId: p.id,
					StreamId:  streamID,
					Action: &stream.TStreamControl_Close{
						Close: &stream.TStreamControl_TActionClose{},
					},
				},
			},
		}
		bClose, errClose := (wire.Frame{Server: mClose, Prefix: []byte("AAPI")}).Marshal()
		if errClose == nil {
			_ = n.down(p.ctx, websocket.MessageBinary, bClose)
		}
		return nil
	}
	return n.synthesize(p, text)
}
func (n *Native) prepareTTSText(text string) string {
	if n != nil && n.TTSReplacer != nil {
		return n.TTSReplacer.Replace(text)
	}
	return defaultTTSReplacer.Replace(text)
}
func (n *Native) synthesize(p *nativeRequest, text string) error {
	text = n.prepareTTSText(text)
	ttsText := text
	if p.isWhisper || (p.detector != nil && p.detector.IsWhisper()) {
		p.isWhisper = true
		if !strings.HasPrefix(ttsText, "<speaker is_whisper=") {
			ttsText = "<speaker is_whisper=\"true\"> " + ttsText
		}
		slog.Info("native_tts_whisper_activated", "request", requestTag(p.id))
	}
	ttsGen := &tts.TTtsGenerate{
		Text:   new(ttsText),
		Lang:   new("ru-RU"),
		Format: new("audio/opus"),
	}
	if p.rule == "ask_llm_slysh" {
		evil := "evil"
		ttsGen.Emotion = &evil
		slog.Info("native_tts_emotion_evil", "request", requestTag(p.id))
	}
	t := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: p.ttsID}, Event: &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: ttsGen}}}}
	b, err := (wire.Frame{Client: t, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.up(p.ctx, websocket.MessageBinary, b)
}

// llmStreamEnabled reports whether LLM sentence streaming is on. Streaming is
// disabled by default: the deferred re-mux path delivers no audible audio on
// screenless speakers, so the classic reply path stays the working default.
func llmStreamEnabled() bool {
	switch strings.ToLower(os.Getenv("QUASAR_LLM_STREAM")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// beepProbePath returns the diagnostic audio file when the query asks for the
// direct-to-speaker probe and QUASAR_BEEP is configured.
func beepProbePath(text string) string {
	path := os.Getenv("QUASAR_BEEP")
	if path == "" {
		return ""
	}
	low := strings.ToLower(text)
	if strings.Contains(low, "тест") && strings.Contains(low, "звук") {
		return path
	}
	return ""
}

func streamControlAction(sc *stream.TStreamControl) string {
	switch {
	case sc.GetClose() != nil:
		return "close"
	case sc.GetSpotterEnd() != nil:
		return "spotter_end"
	case sc.GetLazyTTS() != nil:
		return "lazy_tts"
	default:
		return "other"
	}
}

// takePendingSpeak returns and clears a diagnostic voice file queued for the
// next station request.
func (n *Native) takePendingSpeak() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	file := n.pendingSpeak
	n.pendingSpeak = ""
	return file
}

// QueueSpeak schedules a local voice file for the next station request. The
// station only renders TTS that belongs to its own active request, so the file
// cannot be pushed while the speaker is idle.
func (n *Native) QueueSpeak(path string) {
	n.mu.Lock()
	n.pendingSpeak = path
	n.mu.Unlock()
}

// speakLocal plays an Ogg/Opus file as the answer to a live station request.
// It reproduces the cloud sequence (AliceResponse, TtsSpeak, Opus chunks,
// StreamControl close) with the module refs bound to the request id.
func (n *Native) speakLocal(p *nativeRequest, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n.mu.Lock()
	p.local = true
	p.streaming = false
	if p.ttsID == "" {
		p.ttsID = newID()
	}
	ttsID := p.ttsID
	n.tts[ttsID] = p
	n.mu.Unlock()
	if p.gated {
		if err := n.sendCancel(p.ctx, p.id); err != nil {
			return err
		}
	} else if err := n.ack(p); err != nil {
		return err
	}
	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	answer := &response.TAliceResponseProto{
		Header: &response.TAliceResponseProto_THeader{
			RequestId:      new(requestID),
			ResponseId:     new(newID()),
			DialogId:       new(p.request.GetHeader().GetDialogId()),
			SequenceNumber: new(p.request.GetHeader().GetSequenceNumber()),
		},
		VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{
			OutputSpeech:     &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Тест звука")},
			HasVoiceResponse: true,
			ShouldListen:     false,
		},
	}
	msg := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id},
		Event:  &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer},
	}}}
	frame, err := (wire.Frame{Server: msg, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.down(p.ctx, websocket.MessageBinary, frame); err != nil {
		return err
	}
	streamID := int64(20000 + time.Now().UnixNano()%40000)
	speak := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{MessageId: ttsID, RefMessageId: p.id, StreamId: new(streamID)},
		Event:  &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &ttsspeak.TTtsSpeak{Format: "audio/opus"}},
	}}}
	frame, err = (wire.Frame{Server: speak, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.down(p.ctx, websocket.MessageBinary, frame); err != nil {
		return err
	}
	for off := 0; off < len(data); off += 8000 {
		end := min(off+8000, len(data))
		chunk := append(binary.BigEndian.AppendUint32(nil, uint32(streamID)), data[off:end]...)
		if err := n.down(p.ctx, websocket.MessageBinary, chunk); err != nil {
			return err
		}
	}
	closeMsg := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{
		MessageId: ttsID,
		StreamId:  streamID,
		Action:    &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}},
	}}}
	frame, err = (wire.Frame{Server: closeMsg, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.down(p.ctx, websocket.MessageBinary, frame); err != nil {
		return err
	}
	slog.Info("native_local_speak", "request", requestTag(p.id), "file", path, "stream", streamID, "bytes", len(data))
	return nil
}

func requestTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:4])
}

// Quasar uses Yandex TGUID spelling (8-8-8-8), not RFC UUID spelling.
func newID() string {
	return sound.NewStreamID()
}

func (n *Native) makeContinue(p *nativeRequest) ([]byte, error) {
	h := proto.Clone(p.header).(*header.TEventHeader)
	h.MessageId = newID()
	h.RefMessageId = ""
	h.StreamId = nil
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: h, Event: &client.TClientMessage_TEvent_ContinueRequest{ContinueRequest: &continuation.TContinueRequest{RequestMessageId: p.id}}}}}
	return (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
}

func (n *Native) ack(p *nativeRequest) error {
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_InputStartAck{InputStartAck: &ack.TInputStartAck{RequestStartTime: uint64(time.Now().UnixMilli())}}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.down(p.ctx, websocket.MessageBinary, b)
}

func canReplaceResponse(r *response.TAliceResponseProto) bool {
	for _, d := range r.GetResponse().GetDirectives() {
		if d.GetType() != "client_action" || d.GetName() != "tts_play_placeholder" {
			return false
		}
	}
	return true
}

func (n *Native) isRefusal(text string) bool {
	if detector, ok := n.resolver.(interface{ IsRefusal(string) bool }); ok {
		return detector.IsRefusal(text)
	}
	return IsRefusal(text)
}

func (n *Native) discardStale(p *nativeRequest) {
	n.mu.Lock()
	p.canceled = true
	p.cancel()
	n.mu.Unlock()
	_ = n.sendCancel(n.ctx, p.id)
}

var chimeRegex = regexp.MustCompile(`<speaker audio="[^"]+">`)

func isSmartHomeResponse(r *response.TAliceResponseProto) bool {
	if r == nil {
		return false
	}
	resp := r.GetResponse()
	if resp != nil {
		for _, d := range resp.GetDirectives() {
			name := strings.ToLower(d.GetName())
			dtype := strings.ToLower(d.GetType())
			if strings.Contains(name, "on_off") ||
				strings.Contains(name, "iot") ||
				strings.Contains(name, "brightness") ||
				strings.Contains(name, "color") ||
				strings.Contains(name, "temperature") ||
				strings.Contains(name, "thermostat") ||
				strings.Contains(name, "toggle") ||
				strings.Contains(name, "mode_directive") ||
				strings.Contains(name, "range_directive") ||
				strings.Contains(name, "start_bluetooth") ||
				strings.Contains(name, "stop_bluetooth") {
				return true
			}
			if strings.Contains(dtype, "iot") {
				return true
			}
		}
	}
	speech := r.GetVoiceResponse().GetOutputSpeech().GetText()
	if strings.Contains(speech, "alice-iot-sounds-") || strings.Contains(speech, "dialog-sounds-") {
		return true
	}
	return false
}
