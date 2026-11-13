package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/coder/websocket"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/wire"
)

// streamlab.go is the offline streaming lab: it runs the same streaming
// pipeline the station uses (sentence streaming, TTS replacements, cloud TTS
// synthesis, Ogg remux) without a live station session, so the working route
// stays untouched while the streaming path is exercised end to end.

// NewOggMux starts an Ogg/Opus remuxer identical to the one joining streamed
// TTS segments for the station.
func NewOggMux() *OggMux { return &OggMux{} }

// OggMux collects independently synthesized Opus segments into one Ogg stream.
type OggMux struct {
	speech oggSpeech
	broken bool
}

// Add appends one complete Ogg/Opus segment (one TTS phrase).
func (m *OggMux) Add(segment []byte) error {
	if _, err := m.speech.feed(segment); err != nil {
		m.broken = true
		return err
	}
	if err := m.speech.endSource(); err != nil {
		m.broken = true
		return err
	}
	return nil
}

// AddRaw appends a segment without validating Opus packet durations. Cloud TTS
// marks every packet with the code-3 frame count, which the strict decoder
// rejects even though the audio plays; the raw path keeps the lab moving.
func (m *OggMux) AddRaw(segment []byte) error {
	if _, err := m.speech.feedRaw(segment); err != nil {
		m.broken = true
		return err
	}
	return nil
}

// feedRaw is oggSpeech.feed without Opus duration validation: audio packets are
// copied verbatim, so streams the strict decoder rejects still concatenate.
func (o *oggSpeech) feedRaw(b []byte) ([]byte, error) {
	if o.source == nil {
		o.next()
	}
	s := o.source
	s.buffer = append(s.buffer, b...)
	var out []byte
	for len(s.buffer) >= 27 {
		p := s.buffer
		if string(p[:4]) != "OggS" || p[4] != 0 {
			return nil, errors.New("invalid Ogg page")
		}
		count := int(p[26])
		if len(p) < 27+count {
			break
		}
		size := 27 + count
		for _, n := range p[27 : 27+count] {
			size += int(n)
		}
		if len(p) < size {
			break
		}
		page := p[:size]
		if len(o.head) == 0 {
			o.serial = binary.LittleEndian.Uint32(page[14:18])
		}
		pos := 27 + count
		for _, n := range page[27 : 27+count] {
			s.packet = append(s.packet, page[pos:pos+int(n)]...)
			pos += int(n)
			if n == 255 {
				continue
			}
			packet := s.packet
			s.packet = nil
			switch s.headers {
			case 0:
				if len(o.head) == 0 {
					o.head = bytes.Clone(packet)
					out = append(out, o.page(packet, 2, 0)...)
				}
			case 1:
				if o.sequence == 1 {
					out = append(out, o.page(packet, 0, 0)...)
				}
			default:
				out = append(out, o.flushPending(0, 0)...)
				o.pending = bytes.Clone(packet)
				o.pendingSamples = 960
			}
			s.headers++
		}
		s.buffer = s.buffer[size:]
	}
	return out, nil
}

// Finish finalises the stream with the Ogg EOS page and returns the whole file.
func (m *OggMux) Finish() []byte {
	if m.broken {
		return nil
	}
	return append([]byte(nil), m.speech.finish()...)
}

// Bytes returns the stream assembled so far, including a provisional EOS page,
// so a partial result can be listened to before the stream completes.
func (m *OggMux) Bytes() []byte {
	if m.broken {
		return nil
	}
	return append([]byte(nil), m.speech.finish()...)
}

// TTSSynth synthesizes speech through the cloud TTS the station uses, by
// opening its own upstream session. It never touches the station connection.
type TTSSynth struct {
	upstream string
}

// NewTTSSynth returns a synthesizer targeting the given UniProxy endpoint.
func NewTTSSynth(upstream string) *TTSSynth { return &TTSSynth{upstream: upstream} }

// Synthesize returns one phrase as a complete Ogg/Opus blob.
func (s *TTSSynth) Synthesize(ctx context.Context, text, lang, format, emotion string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, s.upstream, nil)
	if err != nil {
		return nil, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)

	id := newID()
	gen := &tts.TTtsGenerate{Text: new(text), Lang: new(lang), Format: new(format)}
	if emotion != "" {
		gen.Emotion = new(emotion)
	}
	msg := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id},
		Event:  &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: gen},
	}}}
	frame, err := (wire.Frame{Client: msg, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		return nil, err
	}

	var audio []byte
	deadline := time.Now().Add(40 * time.Second)
	for {
		readCtx, stop := context.WithDeadline(ctx, deadline)
		kind, data, err := conn.Read(readCtx)
		stop()
		if err != nil {
			if len(audio) > 0 {
				return audio, nil
			}
			return nil, err
		}
		if kind != websocket.MessageBinary {
			continue
		}
		f := wire.Decode(data, wire.ServerToClient)
		if f.Server == nil {
			if len(data) > 4 {
				audio = append(audio, data[4:]...)
			}
			continue
		}
		if sc := f.Server.GetStreamControl(); sc != nil && sc.GetClose() != nil {
			break
		}
		if e := f.Server.GetEvent(); e != nil && e.GetTtsSpeak() != nil && !strings.EqualFold(e.GetTtsSpeak().GetFormat(), format) {
			return nil, errors.New("tts format mismatch: " + e.GetTtsSpeak().GetFormat())
		}
	}
	if len(audio) == 0 {
		return nil, errors.New("tts returned no audio")
	}
	return audio, nil
}
