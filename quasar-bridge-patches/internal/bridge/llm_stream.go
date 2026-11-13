package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"quasarbridge/internal/llm"
)

// StreamCompleter is an optional extension of Completer for LLM clients that
// support server-sent-events streaming. Engine detects it at runtime via
// interface assertion; non-streaming clients fall back to Complete().
type StreamCompleter interface {
	Completer
	// StreamComplete sends the SSE request and writes delta.content tokens to
	// out as they arrive. It closes out when done and returns any terminal error.
	// reasoning_content and other non-content fields are silently discarded.
	StreamComplete(ctx context.Context, messages []llm.Message, out chan<- string) error
}

// llmStreamClient wraps *llm.Client and implements StreamCompleter for the
// chat/completions SSE format used by OpenAI-compatible providers.
type llmStreamClient struct {
	*llm.Client
}

// NewStreamClient returns a StreamCompleter that wraps c. All non-streaming
// calls (classifier, dialogue, tool calls) pass through to c unchanged.
func NewStreamClient(c *llm.Client) StreamCompleter { return &llmStreamClient{c} }

func (c *llmStreamClient) StreamComplete(ctx context.Context, messages []llm.Message, out chan<- string) error {
	request := map[string]any{
		"model": c.Model, "messages": messages, "stream": true,
		"temperature": 0,
	}
	if c.MaxOutputTokens > 0 {
		request["max_tokens"] = c.MaxOutputTokens
	}
	if c.ReasoningEffort != "" {
		request["reasoning_effort"] = c.ReasoningEffort
	}
	for key, value := range c.ChatParameters {
		if value == nil {
			delete(request, key)
		} else {
			request[key] = value
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return errors.New("cannot encode LLM stream request")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid LLM streaming endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("LLM stream transport: %w", ctx.Err())
		}
		return errors.New("LLM stream transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LLM stream returned HTTP %d", resp.StatusCode)
	}
	return sseReadStream(ctx, resp.Body, out)
}

func sseReadStream(ctx context.Context, body io.Reader, out chan<- string) error {
	const maxStreamBytes = 2 << 20
	limited := &io.LimitedReader{R: body, N: maxStreamBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 16*1024), maxStreamBytes+1)
	var dataBuilder strings.Builder
	flush := func() (string, error) {
		if dataBuilder.Len() == 0 {
			return "", nil
		}
		payload := dataBuilder.String()
		dataBuilder.Reset()
		if payload == "[DONE]" {
			return "", nil
		}
		return sseExtractContent(payload)
	}
	for scanner.Scan() {
		if limited.N <= 0 {
			return errors.New("LLM stream exceeded size limit")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if line == "" {
			tok, err := flush()
			if err != nil {
				return err
			}
			if tok != "" {
				select {
				case out <- tok:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok || field != "data" {
			continue
		}
		if dataBuilder.Len() > 0 {
			dataBuilder.WriteByte('\n')
		}
		dataBuilder.WriteString(strings.TrimPrefix(value, " "))
	}
	if limited.N <= 0 {
		return errors.New("LLM stream exceeded size limit")
	}
	if err := scanner.Err(); err != nil {
		return errors.New("cannot read LLM stream")
	}
	// Handle any remaining data without trailing blank line.
	if tok, err := flush(); err != nil {
		return err
	} else if tok != "" {
		select {
		case out <- tok:
		case <-ctx.Done():
		}
	}
	return nil
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func sseExtractContent(payload string) (string, error) {
	var chunk sseChunk
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return "", nil // tolerate unknown event types
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}
	c := chunk.Choices[0]
	if c.FinishReason != nil && *c.FinishReason == "length" {
		return "", errors.New("LLM answer exceeded token budget")
	}
	// reasoning_content is internal chain-of-thought — never forward to TTS
	return c.Delta.Content, nil
}

// ── Sentence chunker ──────────────────────────────────────────────────────────

// sentenceChunker buffers incoming tokens and emits complete sentences.
// Boundaries: '.', '!', '?', '\n', '…' followed by space or end-of-string.
// Short fragments are merged to avoid creating very short TTS synthesis calls.
type sentenceChunker struct {
	buf      strings.Builder
	minRunes int
}

func newSentenceChunker() *sentenceChunker { return &sentenceChunker{minRunes: 25} }

// Feed appends tok and returns any complete sentences.
func (sc *sentenceChunker) Feed(tok string) []string {
	sc.buf.WriteString(tok)
	return sc.flush(false)
}

// Flush returns whatever remains in the buffer as a final sentence.
func (sc *sentenceChunker) Flush() []string {
	return sc.flush(true)
}

func (sc *sentenceChunker) flush(final bool) []string {
	text := sc.buf.String()
	var out []string
	for {
		idx := sseSentenceBoundary(text)
		if idx < 0 {
			break
		}
		sentence := strings.TrimSpace(text[:idx+1])
		text = text[idx+1:]
		if sseRuneLen(sentence) < sc.minRunes && !final && strings.TrimSpace(text) != "" {
			// fragment too short – accumulate more
			text = sentence + " " + text
			break
		}
		if sentence != "" {
			out = append(out, sentence)
		}
	}
	sc.buf.Reset()
	if final {
		if remaining := strings.TrimSpace(text); remaining != "" {
			out = append(out, remaining)
		}
	} else {
		sc.buf.WriteString(text)
	}
	return out
}

func sseSentenceBoundary(text string) int {
	runes := []rune(text)
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' && r != '\n' && r != '…' {
			continue
		}
		after := i + 1
		if after >= len(runes) || runes[after] == ' ' || runes[after] == '\n' {
			return sseRuneIndexToByte(text, i)
		}
	}
	return -1
}

func sseRuneIndexToByte(s string, runeIdx int) int {
	byteIdx := 0
	for i := 0; i < runeIdx; i++ {
		_, size := utf8.DecodeRuneInString(s[byteIdx:])
		byteIdx += size
	}
	_, size := utf8.DecodeRuneInString(s[byteIdx:])
	return byteIdx + size - 1
}

func sseRuneLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// ── StreamFallback ────────────────────────────────────────────────────────────

// StreamResult carries the sentence channel produced by StreamFallback.
type StreamResult struct {
	// Sentences emits TTS-ready sentences as they arrive from the LLM.
	// The channel is closed when the stream is complete.
	Sentences <-chan string
	// Err holds the terminal error of the stream after Sentences is closed.
	// Read it only after the channel has been drained.
	Err *error
}

// StreamFallback starts an LLM stream for rule ask_llm_slysh and returns a
// StreamResult whose Sentences channel emits complete sentences suitable for
// TTS synthesis. If the LLM client does not implement StreamCompleter it falls
// back to Complete() and emits the full answer as one sentence.
func (e *Engine) StreamFallback(ctx context.Context, text, systemPrompt, rule string) *StreamResult {
	sentences := make(chan string, 8)
	var termErr error
	res := &StreamResult{Sentences: sentences, Err: &termErr}

	answerer := e.LLM
	if rule == "ask_llm_podskazhi" && e.Assistant != nil {
		answerer = e.Assistant
	} else if answerer == nil {
		answerer = e.Assistant
	}
	prompt := defaultSystemPrompt
	if systemPrompt != "" {
		prompt = systemPrompt
	}
	ctx2, snapshot, ticket := e.dialogue().Start(ctx)
	if !e.dialogue().Current(ctx2, ticket) {
		close(sentences)
		termErr = context.Canceled
		return res
	}
	messages := []llm.Message{{Role: "system", Content: prompt}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	private := snapshot.PrivateHistory

	go func() {
		defer close(sentences)

		sc, canStream := answerer.(StreamCompleter)
		if !canStream {
			answer, err := answerer.Complete(ctx2, messages, false)
			if err != nil {
				termErr = err
				return
			}
			e.dialogue().Commit(ctx2, ticket, text, answer, private)
			e.setDialogueState(ctx2, ticket, systemPrompt, rule)
			select {
			case sentences <- answer:
			case <-ctx2.Done():
			}
			return
		}

		tokenCh := make(chan string, 64)
		chunker := newSentenceChunker()
		var fullAnswer strings.Builder

		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			for tok := range tokenCh {
				fullAnswer.WriteString(tok)
				for _, s := range chunker.Feed(tok) {
					select {
					case sentences <- s:
					case <-ctx2.Done():
						return
					}
				}
			}
			for _, s := range chunker.Flush() {
				select {
				case sentences <- s:
				case <-ctx2.Done():
				}
			}
		}()

		streamErr := sc.StreamComplete(ctx2, messages, tokenCh)
		close(tokenCh)
		<-consumerDone

		if streamErr != nil && ctx2.Err() == nil {
			termErr = streamErr
			return
		}
		answer := strings.TrimSpace(fullAnswer.String())
		if answer != "" {
			e.dialogue().Commit(ctx2, ticket, text, answer, private)
			e.setDialogueState(ctx2, ticket, systemPrompt, rule)
		}
	}()

	return res
}

// streamReply drains res.Sentences and synthesizes each sentence to TTS in
// order using the deferred/streaming protocol.  It is called from
// Native.maybeResolve when rule == ask_llm_slysh and the LLM supports
// streaming.  The function blocks until all sentences have been delivered or
// the request context is cancelled.
//
// By the time streamReply is called, maybeResolve has already set p.local=true
// and registered the initial p.ttsID in n.tts. streamReply completes the
// streaming initialisation (p.streaming, responseID, progressTTS, etc.) under
// the native mutex, then opens the deferred session before the first sentence.
func streamReply(ctx context.Context, n *Native, p *nativeRequest, res *StreamResult) error {
	first := true
	for sentence := range res.Sentences {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if first {
			first = false
			// Complete streaming initialisation under mutex (n.tts already has
			// p.ttsID from maybeResolve; just fill the remaining fields).
			n.mu.Lock()
			p.streaming = true
			p.responseID = newID()
			p.progressTTS = p.ttsID
			p.progressAudioDone = make(chan struct{})
			n.mu.Unlock()
			// Send cancel or ack to station before opening the stream.
			if p.gated {
				if err := n.sendCancel(ctx, p.id); err != nil {
					return err
				}
			} else {
				if err := n.ack(p); err != nil {
					return err
				}
			}
			// Open the deferred streaming session on the station.
			if err := n.beginDeferred(p); err != nil {
				return err
			}
			slog.Info("native_llm_stream_started", "request", requestTag(p.id))
		}
		if err := n.replyStreamSentence(p, sentence, false); err != nil {
			return err
		}
	}
	if err := *res.Err; err != nil {
		return err
	}
	if first {
		// No sentences at all – treat as empty answer.
		return n.reply(p, "")
	}
	// Close the deferred session with the last (empty) part.
	return n.replyStreamSentence(p, "", true)
}
