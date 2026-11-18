// Package notify delivers operator messages to Telegram.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	maxAttempts  = 4
	firstBackoff = time.Second
)

// Notifier sends a plain-text message to the operator.
type Notifier interface {
	Send(ctx context.Context, text string) error
}

// LogOnly is used when Telegram is not configured.
type LogOnly struct{ Log *slog.Logger }

// Send writes the message to the log.
func (l LogOnly) Send(_ context.Context, text string) error {
	l.Log.Info("notification", "text", text)
	return nil
}

// Telegram sends messages through the Bot API.
type Telegram struct {
	apiURL  string
	token   string
	chatID  string
	client  *http.Client
	backoff time.Duration
}

// NewTelegram creates a Telegram notifier.
func NewTelegram(apiURL, token, chatID string) *Telegram {
	return &Telegram{
		apiURL:  apiURL,
		token:   token,
		chatID:  chatID,
		client:  &http.Client{Timeout: 10 * time.Second},
		backoff: firstBackoff,
	}
}

// Send delivers a message, retrying rate limits and transient failures.
func (t *Telegram) Send(ctx context.Context, text string) error {
	payload, err := json.Marshal(map[string]string{"chat_id": t.chatID, "text": text})
	if err != nil {
		return err
	}

	var lastErr error
	wait := t.backoff
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retryAfter, err := t.post(ctx, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		var permanent *permanentError
		if errors.As(err, &permanent) || attempt == maxAttempts {
			break
		}
		if retryAfter > wait {
			wait = retryAfter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	return lastErr
}

type permanentError struct{ status int }

func (e *permanentError) Error() string {
	return fmt.Sprintf("telegram rejected the message: HTTP %d", e.status)
}

func (t *Telegram) post(ctx context.Context, payload []byte) (time.Duration, error) {
	endpoint := t.apiURL + "/bot" + t.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("telegram: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		// url.Error embeds the full URL, which contains the bot token.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return 0, fmt.Errorf("telegram request failed: %w", urlErr.Err)
		}
		return 0, errors.New("telegram request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		var parsed struct {
			Parameters struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		_ = json.Unmarshal(body, &parsed)
		return time.Duration(parsed.Parameters.RetryAfter) * time.Second, errors.New("telegram rate limit")
	case resp.StatusCode >= http.StatusInternalServerError:
		return 0, fmt.Errorf("telegram server error: HTTP %d", resp.StatusCode)
	default:
		return 0, &permanentError{status: resp.StatusCode}
	}
}
