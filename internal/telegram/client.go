package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nayakashwin/telegram-gateway/internal/metrics"
)

// Client talks to the Telegram Bot API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// Update is a single update from getUpdates.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is a Telegram message as delivered by getUpdates.
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      User   `json:"from"`
	Text      string `json:"text"`
}

// Chat identifies a Telegram conversation.
type Chat struct {
	ID int64 `json:"id"`
}

// User is the sender of a message.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// New creates a Client for the given bot token against the Telegram API.
// Use NewWithBaseURL to point at a different API base (e.g. in tests).
func New(token string, logger *slog.Logger) *Client {
	return NewWithBaseURL("https://api.telegram.org", token, logger)
}

// NewWithBaseURL creates a Client against a custom API base URL.
func NewWithBaseURL(baseURL, token string, logger *slog.Logger) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		logger:  logger,
	}
}

// SetMetrics attaches optional metrics collection. Passing nil is a no-op.
func (c *Client) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

// GetUpdates long-polls Telegram for new updates. timeout is the polling
// timeout in seconds. It returns only updates with a message.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int64) ([]Update, error) {
	body, err := json.Marshal(map[string]any{
		"offset":          offset,
		"limit":           100,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal getUpdates: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/getUpdates", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		OK          bool     `json:"ok"`
		Result      []Update `json:"result"`
		Description string   `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !payload.OK {
		return nil, fmt.Errorf("getUpdates error: %s", payload.Description)
	}

	updates := payload.Result[:0]
	for _, u := range payload.Result {
		if u.Message != nil {
			updates = append(updates, u)
		}
	}
	return updates, nil
}

// SendMessage sends a text message to the given chat and returns the message id.
// replyToMessageID, when > 0, makes the message a Telegram reply to that
// message id in the same chat.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyToMessageID int64) (int64, error) {
	req := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyToMessageID > 0 {
		req["reply_to_message_id"] = replyToMessageID
	}
	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal sendMessage: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/sendMessage", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var payload struct {
		OK          bool    `json:"ok"`
		Description string  `json:"description"`
		Result      Message `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode sendMessage: %w", err)
	}
	if !payload.OK {
		return 0, fmt.Errorf("sendMessage error: %s", payload.Description)
	}
	return payload.Result.MessageID, nil
}

// GetMe verifies the bot token and returns bot identity.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	resp, err := c.do(ctx, http.MethodPost, "/getMe", nil)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()

	var payload struct {
		OK          bool   `json:"ok"`
		Result      User   `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return User{}, fmt.Errorf("decode getMe: %w", err)
	}
	if !payload.OK {
		return User{}, fmt.Errorf("getMe error: %s", payload.Description)
	}
	return payload.Result, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	url := c.baseURL + "/bot" + c.token + path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.logger.Debug("telegram request", "method", method, "path", path, "url", redactToken(url))
	resp, err := c.http.Do(req)
	if c.metrics != nil {
		c.metrics.ObserveTelegram(path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("telegram request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if c.metrics != nil {
			c.metrics.ObserveTelegram(path, fmt.Errorf("http %d", resp.StatusCode))
		}
		return nil, fmt.Errorf("telegram http %d: %s", resp.StatusCode, string(raw))
	}
	return resp, nil
}

// redactToken masks the bot token in URLs before logging.
func redactToken(url string) string {
	return "***"
}
