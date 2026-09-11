package titles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Summarizer turns a slice of conversation into a title. An interface so the
// pass can be tested without a network, and so a different provider is a new
// file rather than a rewrite.
type Summarizer interface {
	Summarize(ctx context.Context, slice string) (string, error)

	// Name identifies the model, recorded in the index so it is possible to
	// tell later which titles came from what.
	Name() string
}

// DefaultModel is the smallest model that writes a decent title. Summarizing
// a couple of thousand tokens into eight words is not a job that rewards a
// larger one, and this pass runs over every session in the corpus.
const DefaultModel = "claude-haiku-4-5-20251001"

// MaxTitleRunes caps the stored title.
//
// The prompt asks for something short, but a prompt is a request, not a
// constraint: a model that decides to answer with a paragraph must not be able
// to put a paragraph in a list row.
const MaxTitleRunes = 80

// systemPrompt asks for a title and nothing else.
//
// The negative instructions are all failure modes worth naming: models
// narrate ("The user is asking about..."), they quote, and they write
// sentences. What the list needs is the kind of phrase a person would use to
// refer to the conversation later.
const systemPrompt = `You write short titles for transcripts of programming sessions between a developer and an AI assistant.

Given the opening of a session, reply with a title of at most eight words naming what the session is about. Prefer concrete specifics from the text -- the tool, file, error, or feature involved -- over general words like "debugging" or "discussion".

Reply with the title alone: no quotes, no trailing period, no preamble, no explanation.`

// Anthropic summarizes with the Anthropic messages API.
//
// Hand-rolled over net/http: the whole interaction is one JSON POST, and an
// SDK would be by far the largest dependency in the module for it.
type Anthropic struct {
	APIKey string
	Model  string

	// BaseURL defaults to the public API. Set by tests.
	BaseURL string

	// HTTP defaults to a client with a timeout. The zero http.Client has none,
	// which turns a hung connection into a pass that never finishes.
	HTTP *http.Client

	// sleep is overridable so retry tests do not wait out real backoff.
	sleep func(context.Context, time.Duration) error
}

// ErrNoAPIKey reports that summarizing is not configured. Returned rather than
// logged so the caller can say so once and skip the pass, instead of failing
// every session in turn.
var ErrNoAPIKey = errors.New("ANTHROPIC_API_KEY is not set")

// NewAnthropic builds a summarizer from the environment.
func NewAnthropic() (*Anthropic, error) {
	key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		return nil, ErrNoAPIKey
	}
	// ANTHROPIC_BASE_URL is the SDKs' own convention, and it is what makes a
	// gateway, a proxy, or a local fake usable without a build flag.
	return &Anthropic{
		APIKey:  key,
		Model:   strings.TrimSpace(os.Getenv("SPIREWEB_TITLE_MODEL")),
		BaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
	}, nil
}

func (a *Anthropic) Name() string {
	if a.Model != "" {
		return a.Model
	}
	return DefaultModel
}

// maxRetries is how many times a request is repeated before the session is
// left without a title. Rate limits are the expected reason to be here, and
// with eight workers against a shared account they are routine rather than
// exceptional.
const maxRetries = 4

// Summarize returns a title for slice.
func (a *Anthropic) Summarize(ctx context.Context, slice string) (string, error) {
	if strings.TrimSpace(slice) == "" {
		return "", fmt.Errorf("empty session")
	}

	body, err := json.Marshal(map[string]any{
		"model": a.Name(),
		// Eight words plus the odd model that ignores the instruction. Small
		// enough that a runaway response costs nothing.
		"max_tokens": 64,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": slice},
		},
	})
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			wait := backoff(attempt, lastErr)
			if err := a.nap(ctx, wait); err != nil {
				return "", err
			}
		}
		title, err := a.once(ctx, body)
		if err == nil {
			return title, nil
		}
		lastErr = err
		if !retryable(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("after %d attempts: %w", maxRetries, lastErr)
}

func (a *Anthropic) once(ctx context.Context, body []byte) (string, error) {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := a.HTTP
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// A transport error is a network blip most of the time, and the
		// request is idempotent, so it is worth another go.
		return "", &apiError{status: 0, msg: err.Error()}
	}
	defer resp.Body.Close()

	// Bounded: an error page from a proxy in the way is not something to read
	// into memory in full.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &apiError{status: resp.StatusCode, msg: err.Error()}
	}

	if resp.StatusCode != http.StatusOK {
		return "", &apiError{
			status:     resp.StatusCode,
			msg:        apiMessage(raw),
			retryAfter: retryAfter(resp.Header),
		}
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	for _, c := range out.Content {
		if c.Type == "text" {
			if t := Clean(c.Text); t != "" {
				return t, nil
			}
		}
	}
	return "", fmt.Errorf("no title in response")
}

var defaultClient = &http.Client{Timeout: 60 * time.Second}

// Clean turns a model's reply into something a list row can show: one line,
// no surrounding quotes, no trailing period, bounded length.
//
// Exported because it is the guarantee the rest of the system relies on -- a
// title is one short line -- and worth testing directly.
func Clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, ` "'“”‘’`)
	s = strings.TrimRight(s, ".")
	s = strings.TrimSpace(s)

	r := []rune(s)
	if len(r) > MaxTitleRunes {
		s = strings.TrimSpace(string(r[:MaxTitleRunes-1])) + "…"
	}
	return s
}

// apiError carries the status code, which is what decides whether to retry.
type apiError struct {
	status     int // 0 for a transport failure
	msg        string
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.status == 0 {
		return "anthropic: " + e.msg
	}
	return fmt.Sprintf("anthropic: %d %s", e.status, e.msg)
}

// retryable covers rate limits, server errors, and transport failures.
// Everything else -- a bad key, a model that does not exist, a request the API
// rejects -- will fail identically however many times it is sent.
func retryable(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.status == 0 || ae.status == http.StatusTooManyRequests ||
		ae.status == http.StatusRequestTimeout || ae.status >= 500
}

// backoff waits longer each attempt, but defers to the server when it says how
// long to wait.
func backoff(attempt int, err error) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) && ae.retryAfter > 0 {
		return ae.retryAfter
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func (a *Anthropic) nap(ctx context.Context, d time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("retry-after")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// apiMessage pulls the human-readable part out of an error body, falling back
// to the body itself when it is not the shape we expect.
func apiMessage(raw []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
