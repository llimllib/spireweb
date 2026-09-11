package titles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// reply writes a successful messages-API response.
func reply(w http.ResponseWriter, text string) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

// testClient returns a client pointed at srv with backoff removed, so a retry
// test does not wait out real seconds.
func testClient(srv *httptest.Server) *Anthropic {
	return &Anthropic{
		APIKey:  "sk-test",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		sleep:   func(context.Context, time.Duration) error { return nil },
	}
}

func TestSummarizeSendsPromptAndReturnsTitle(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header missing; the API requires it")
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		reply(w, "Centering a div with flexbox")
	}))
	defer srv.Close()

	got, err := testClient(srv).Summarize(context.Background(), "user: how do I center a div")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Centering a div with flexbox" {
		t.Errorf("Summarize() = %q", got)
	}
	if body["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", body["model"], DefaultModel)
	}
	if s, _ := body["system"].(string); !strings.Contains(s, "title") {
		t.Errorf("system prompt = %q", s)
	}
}

func TestSummarizeRetriesRateLimits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("retry-after", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		reply(w, "Fixing the WAL checkpoint stall")
	}))
	defer srv.Close()

	got, err := testClient(srv).Summarize(context.Background(), "user: hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fixing the WAL checkpoint stall" {
		t.Errorf("Summarize() = %q", got)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("calls = %d, want 3 (two rate limits then success)", n)
	}
}

// A bad key fails the same way however many times it is sent. Retrying it
// wastes the pass's time and makes the eventual error message worse.
func TestSummarizeDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	_, err := testClient(srv).Summarize(context.Background(), "user: hello")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("error = %v, want the API's message preserved", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestSummarizeGivesUpAfterRepeatedFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(srv).Summarize(context.Background(), "user: hello"); err == nil {
		t.Fatal("want an error")
	}
	if n := calls.Load(); n != maxRetries {
		t.Errorf("calls = %d, want %d", n, maxRetries)
	}
}

func TestSummarizeHonoursContextCancellation(t *testing.T) {
	// Released by the test rather than by the request's own context: an
	// HTTP/1.1 server does not necessarily observe a client disconnect until
	// it next reads, and Close would then wait for the handler forever.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	c := &Anthropic{APIKey: "sk-test", BaseURL: srv.URL, HTTP: srv.Client(),
		sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	if _, err := c.Summarize(ctx, "user: hello"); err == nil {
		t.Fatal("want an error once the context is cancelled")
	}
}

// The prompt asks for a short bare phrase; this is what happens when the model
// does not listen.
func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"Fixing the indexer"`, "Fixing the indexer"},
		{"Fixing the indexer.", "Fixing the indexer"},
		{"  Fixing   the\n indexer  ", "Fixing the indexer"},
		{"“Fixing the indexer”", "Fixing the indexer"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	long := Clean(strings.Repeat("words ", 100))
	if n := len([]rune(long)); n > MaxTitleRunes {
		t.Errorf("Clean() returned %d runes, want <= %d: a list row is one line",
			n, MaxTitleRunes)
	}
}
