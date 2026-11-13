package bridge

import (
	"bytes"
	"log/slog"
	"math"
	"sync"

	"github.com/pion/opus"
)

// whisperDetector analyzes incoming microphone Opus audio packets in real-time
// to distinguish whispered speech from normal voiced speech using DSP acoustic features:
// 1. RMS Energy (silence / ambient floor rejection)
// 2. Normalized Autocorrelation Peak (pitch / vocal cord periodicity detection)
// 3. Zero-Crossing Rate (ZCR)
// 4. High-to-Low Spectral Energy Balance (spectral tilt)
type whisperDetector struct {
	mu            sync.Mutex
	decoder       opus.Decoder
	decoderInit   bool
	oggBuf        []byte
	oggPacket     []byte
	oggHeaders    int
	totalPackets  int
	totalDecoded  int
	decodeErrors  int
	lastErr       string
	silentFrames  int
	activeFrames  int
	whisperCount  int
	voicedCount   int
	maxRms        float64
	pcmBuf        [5760]float32
}

func newWhisperDetector() *whisperDetector {
	dec, err := opus.NewDecoderWithOutput(16000, 1)
	if err != nil {
		slog.Warn("whisper_detector_init_failed", "error", err)
	}
	return &whisperDetector{
		decoder:     dec,
		decoderInit: err == nil,
	}
}

// Feed consumes binary chunks of audio data from the client's audio stream.
// It supports both raw Opus packets and Ogg Opus page containers ("OggS").
func (d *whisperDetector) Feed(b []byte) {
	if len(b) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	d.totalPackets++

	// If already in an Ogg stream or packet starts with OggS magic marker
	if len(d.oggBuf) > 0 || (len(b) >= 4 && string(b[:4]) == "OggS") {
		d.feedOgg(b)
		return
	}

	// Otherwise, it is a raw Opus packet sent directly
	d.processPacket(b)
}

func (d *whisperDetector) feedOgg(b []byte) {
	if len(d.oggBuf)+len(b) > 2<<20 {
		d.oggBuf = nil
	}
	d.oggBuf = append(d.oggBuf, b...)

	for len(d.oggBuf) >= 27 {
		p := d.oggBuf
		if string(p[:4]) != "OggS" || p[4] != 0 {
			idx := bytes.Index(p[1:], []byte("OggS"))
			if idx < 0 {
				d.oggBuf = nil
				break
			}
			d.oggBuf = p[1+idx:]
			continue
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
		pos := 27 + count
		for _, n := range page[27 : 27+count] {
			d.oggPacket = append(d.oggPacket, page[pos:pos+int(n)]...)
			pos += int(n)
			if n == 255 {
				continue
			}
			packet := d.oggPacket
			d.oggPacket = nil
			if d.oggHeaders == 0 {
				// RFC 7845: OpusHead
				d.oggHeaders++
			} else if d.oggHeaders == 1 {
				// RFC 7845: OpusTags
				d.oggHeaders++
			} else {
				d.processPacket(packet)
			}
		}
		d.oggBuf = d.oggBuf[size:]
	}
}

func (d *whisperDetector) processPacket(packet []byte) {
	if len(packet) == 0 {
		return
	}
	if !d.decoderInit {
		return
	}

	n, err := d.decoder.DecodeToFloat32(packet, d.pcmBuf[:])
	if err != nil {
		d.decodeErrors++
		d.lastErr = err.Error()
		return
	}
	if n <= 0 {
		return
	}
	d.totalDecoded++
	d.analyzeSamples(d.pcmBuf[:n])
}

func (d *whisperDetector) analyzeSamples(samples []float32) {
	const frameSize = 320 // 20ms @ 16 kHz
	for len(samples) >= frameSize {
		d.analyzeFrame(samples[:frameSize])
		samples = samples[frameSize:]
	}
	if len(samples) >= 160 { // at least 10ms tail
		d.analyzeFrame(samples)
	}
}

func (d *whisperDetector) analyzeFrame(frame []float32) {
	n := len(frame)
	if n < 80 {
		return
	}

	// 1. RMS Energy (rejection of silence / noise floor; whisper typically 0.0004 - 0.003)
	var sumSq float64
	for _, s := range frame {
		v := float64(s)
		sumSq += v * v
	}
	rms := math.Sqrt(sumSq / float64(n))
	if rms > d.maxRms {
		d.maxRms = rms
	}
	if rms < 0.0003 {
		d.silentFrames++
		return
	}
	d.activeFrames++

	// 2. Zero-Crossing Rate (ZCR)
	var zcrCount int
	for i := 1; i < n; i++ {
		if (frame[i] >= 0 && frame[i-1] < 0) || (frame[i] < 0 && frame[i-1] >= 0) {
			zcrCount++
		}
	}
	zcr := float64(zcrCount) / float64(n-1)

	// 3. Normalized Autocorrelation Peak (Pitch Periodicity: 70 Hz to 350 Hz)
	const minLag = 45 // ~355 Hz @ 16 kHz
	maxLag := 228     // ~70 Hz @ 16 kHz
	if maxLag > n-20 {
		maxLag = n - 20
	}
	var maxR float64
	for lag := minLag; lag <= maxLag; lag++ {
		var num, den1, den2 float64
		for i := 0; i < n-lag; i++ {
			x0 := float64(frame[i])
			x1 := float64(frame[i+lag])
			num += x0 * x1
			den1 += x0 * x0
			den2 += x1 * x1
		}
		den := math.Sqrt(den1 * den2)
		if den > 1e-9 {
			r := num / den
			if r > maxR {
				maxR = r
			}
		}
	}

	// 4. High-to-Low Spectral Energy Balance (Boxcar moving-average lowpass at ~1000 Hz)
	var lowSumSq, highSumSq float64
	var ma float64
	for i := 0; i < n; i++ {
		ma += float64(frame[i])
		if i >= 8 {
			ma -= float64(frame[i-8])
			lowVal := ma / 8.0
			highVal := float64(frame[i]) - lowVal
			lowSumSq += lowVal * lowVal
			highSumSq += highVal * highVal
		}
	}
	specRatio := (highSumSq + 1e-6) / (lowSumSq + 1e-6)

	// Classification of the frame:
	// A voiced frame exhibits clear pitch periodicity from vocal cord vibration (maxR >= 0.38, zcr < 0.22, specRatio < 1.10).
	// An unvoiced / whispered frame lacks pitch periodicity (maxR < 0.38) or is dominated by turbulent high-frequency energy.
	if maxR >= 0.38 && zcr < 0.22 && specRatio < 1.10 {
		d.voicedCount++
	} else if maxR < 0.38 || (zcr >= 0.15 && specRatio >= 1.05) {
		d.whisperCount++
	}
}

// IsWhisper evaluates accumulated frame statistics and returns whether the speech was whispered.
func (d *whisperDetector) IsWhisper() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	whisperRatio := 0.0
	voicedRatio := 0.0
	if d.activeFrames > 0 {
		whisperRatio = float64(d.whisperCount) / float64(d.activeFrames)
		voicedRatio = float64(d.voicedCount) / float64(d.activeFrames)
	}

	// Speech is whisper if:
	// 1. At least 5 active speech frames (~100ms)
	// 2. Unvoiced / turbulent frames dominate over voiced frames (whisperCount > voicedCount)
	// 3. Voiced vocal cord vibration is minimal (voicedRatio <= 0.28 or voicedCount <= 4)
	// 4. Whisper ratio is at least 30% of active frames
	isWhisper := d.activeFrames >= 5 && (d.whisperCount > d.voicedCount) && (voicedRatio <= 0.28 || d.voicedCount <= 4) && whisperRatio >= 0.30

	slog.Info("whisper_detector_result",
		"is_whisper", isWhisper,
		"active_frames", d.activeFrames,
		"whisper_frames", d.whisperCount,
		"voiced_frames", d.voicedCount,
		"whisper_ratio", math.Round(whisperRatio*100)/100,
		"voiced_ratio", math.Round(voicedRatio*100)/100,
		"max_rms", math.Round(d.maxRms*10000)/10000,
		"silent_frames", d.silentFrames,
		"total_packets", d.totalPackets,
		"decoded_packets", d.totalDecoded,
		"decode_errors", d.decodeErrors,
		"last_err", d.lastErr,
		"ogg_headers", d.oggHeaders,
		"ogg_buf_len", len(d.oggBuf),
	)

	return isWhisper
}
