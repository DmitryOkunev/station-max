package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

type TGUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type TGChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type TGMessage struct {
	MessageID int64  `json:"message_id"`
	From      TGUser `json:"from"`
	Chat      TGChat `json:"chat"`
	Text      string `json:"text"`
}

type TGCallbackQuery struct {
	ID      string     `json:"id"`
	From    TGUser     `json:"from"`
	Message *TGMessage `json:"message"`
	Data    string     `json:"data"`
}

type TGUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *TGMessage       `json:"message"`
	CallbackQuery *TGCallbackQuery `json:"callback_query"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type TGBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type TGClient struct {
	token   string
	baseURL string
	httpCli *http.Client
}

func NewTGClient(token string) *TGClient {
	return &TGClient{
		token:   token,
		baseURL: "https://api.telegram.org",
		httpCli: &http.Client{Timeout: 45 * time.Second},
	}
}

func Esc(s string) string {
	return html.EscapeString(s)
}

func (c *TGClient) apiCall(ctx context.Context, method string, payload any, out any) error {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("tg %s: build request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		// Sanitize token out of any URL error string
		msg := strings.ReplaceAll(err.Error(), c.token, "<REDACTED>")
		return fmt.Errorf("tg %s: %s", method, msg)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return fmt.Errorf("tg %s read: %w", method, err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("tg %s json (%d): %w", method, resp.StatusCode, err)
	}
	if !envelope.OK {
		return &tgAPIError{Method: method, Code: envelope.ErrorCode, Description: envelope.Description}
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("tg %s decode result: %w", method, err)
		}
	}
	return nil
}

type tgAPIError struct {
	Method      string
	Code        int
	Description string
}

func (e *tgAPIError) Error() string {
	return fmt.Sprintf("tg %s error %d: %s", e.Method, e.Code, e.Description)
}

func (c *TGClient) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]TGUpdate, error) {
	req := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "callback_query"},
	}
	var updates []TGUpdate
	if err := c.apiCall(ctx, "getUpdates", req, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func clampTGText(s string) string {
	runes := []rune(s)
	if len(runes) <= 3950 {
		return s
	}
	return string(runes[:3900]) + "\n…(обрезано)"
}

func (c *TGClient) SendMessage(ctx context.Context, chatID int64, htmlText string, markup *InlineKeyboardMarkup) (*TGMessage, error) {
	req := map[string]any{
		"chat_id":    chatID,
		"text":       clampTGText(htmlText),
		"parse_mode": "HTML",
	}
	if markup != nil {
		req["reply_markup"] = markup
	}
	var msg TGMessage
	if err := c.apiCall(ctx, "sendMessage", req, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (c *TGClient) EditMessageText(ctx context.Context, chatID, messageID int64, htmlText string, markup *InlineKeyboardMarkup) error {
	req := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       clampTGText(htmlText),
		"parse_mode": "HTML",
	}
	if markup != nil {
		req["reply_markup"] = markup
	}
	err := c.apiCall(ctx, "editMessageText", req, nil)
	if err != nil {
		if ae, ok := err.(*tgAPIError); ok && strings.Contains(strings.ToLower(ae.Description), "message is not modified") {
			return nil
		}
		return err
	}
	return nil
}

func (c *TGClient) AnswerCallbackQuery(ctx context.Context, callbackID, text string, showAlert bool) error {
	if len([]rune(text)) > 180 {
		text = string([]rune(text)[:177]) + "..."
	}
	req := map[string]any{
		"callback_query_id": callbackID,
	}
	if text != "" {
		req["text"] = text
	}
	if showAlert {
		req["show_alert"] = true
	}
	return c.apiCall(ctx, "answerCallbackQuery", req, nil)
}

func (c *TGClient) SetMyCommands(ctx context.Context, cmds []TGBotCommand) error {
	return c.apiCall(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

