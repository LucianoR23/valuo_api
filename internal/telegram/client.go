package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the Bot API host. Tests point the client at a fake.
const DefaultBaseURL = "https://api.telegram.org"

// Client calls the few Bot API methods the bot needs (Bot API 10.4). A small
// stdlib client instead of a library, as with the AI providers (plan 8.1).
type Client struct {
	token string
	base  string
	http  *http.Client
}

func NewClient(token, baseURL string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{token: token, base: strings.TrimRight(baseURL, "/"), http: hc}
}

// APIError is an answer with ok=false.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// notModified is what editMessageText answers when nothing changed.
func notModified(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.Contains(ae.Description, "message is not modified")
}

// call posts params as JSON and decodes result into out (if not nil). The
// token is in the URL, so errors never include the URL.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	// The host is DefaultBaseURL (or a test server from config) and method a
	// constant from this file.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body)) //nolint:gosec // G704: fixed host, see above
	if err != nil {
		return fmt.Errorf("telegram %s: build request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req) //nolint:gosec // G704: fixed host, see above
	if err != nil {
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer func() { _ = res.Body.Close() }()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: status %d, unreadable answer", method, res.StatusCode)
	}
	if !env.OK {
		return &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// InlineButton is a callback button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Keyboard is rows of buttons; an empty one removes the buttons.
type Keyboard [][]InlineButton

func (k Keyboard) markup() map[string]any {
	rows := k
	if rows == nil {
		rows = Keyboard{}
	}
	return map[string]any{"inline_keyboard": rows}
}

// SendMessage sends plain text (no parse mode, so nothing needs escaping)
// and returns the new message ID.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, kb Keyboard) (int64, error) {
	p := map[string]any{
		"chat_id": chatID, "text": text,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if kb != nil {
		p["reply_markup"] = kb.markup()
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, "sendMessage", p, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// EditMessage replaces a message's text and buttons (nil kb removes them).
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, text string, kb Keyboard) error {
	err := c.call(ctx, "editMessageText", map[string]any{
		"chat_id": chatID, "message_id": messageID, "text": text,
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         kb.markup(),
	}, nil)
	if notModified(err) {
		return nil
	}
	return err
}

// AnswerCallback stops the button's spinner; text shows as a short toast.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
	}
	return c.call(ctx, "answerCallbackQuery", p, nil)
}

// Command is one entry of the bot's command menu.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// Setup registers the webhook (only messages and button taps, signed with
// secret) and the command menu. Idempotent: it runs on every start.
func (c *Client) Setup(ctx context.Context, url, secret string, cmds []Command) error {
	if err := c.call(ctx, "setWebhook", map[string]any{
		"url": url, "secret_token": secret,
		"allowed_updates": []string{"message", "callback_query"},
		"max_connections": 10,
	}, nil); err != nil {
		return err
	}
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// GetFile returns the download path of a file the person sent.
func (c *Client) GetFile(ctx context.Context, fileID string) (string, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &f); err != nil {
		return "", err
	}
	if f.FilePath == "" {
		return "", errors.New("telegram getFile: no path")
	}
	return f.FilePath, nil
}

// Download reads a file from GetFile, up to max bytes.
func (c *Client) Download(ctx context.Context, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+path, nil)
	if err != nil {
		return nil, errors.New("telegram download: build request")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("telegram download: request failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: status %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, errors.New("telegram download: read failed")
	}
	if int64(len(data)) > max {
		return nil, errors.New("telegram download: file too big")
	}
	return data, nil
}
