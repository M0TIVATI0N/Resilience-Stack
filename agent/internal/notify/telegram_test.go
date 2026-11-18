package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastTelegram(url string) *Telegram {
	tg := NewTelegram(url, "SECRET-TOKEN", "42")
	tg.backoff = time.Millisecond
	return tg
}

func TestSendPostsMessage(t *testing.T) {
	var body map[string]string
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer server.Close()

	if err := fastTelegram(server.URL).Send(context.Background(), "привет"); err != nil {
		t.Fatal(err)
	}
	if path != "/botSECRET-TOKEN/sendMessage" || body["chat_id"] != "42" || body["text"] != "привет" {
		t.Fatalf("path=%q body=%v", path, body)
	}
}

func TestSendRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer server.Close()

	if err := fastTelegram(server.URL).Send(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSendDoesNotRetryRejectedMessages(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	if err := fastTelegram(server.URL).Send(context.Background(), "x"); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestErrorsNeverExposeBotToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // Connection refused from now on.

	tg := fastTelegram(url)
	err := tg.Send(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("token leaked: %v", err)
	}
}
