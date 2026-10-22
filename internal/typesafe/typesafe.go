// Package typesafe asks TypeSafe's System One endpoint choice questions,
// within a daily token budget.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultURL is TypeSafe's evaluation endpoint.
const DefaultURL = "https://api.typesafe.ai/v1/systemone"

// ErrBudget is returned, without a request, once the day's tokens are spent.
var ErrBudget = errors.New("typesafe: daily token budget spent")

// Question is a choice question; Options maps each option to its
// description.
type Question struct {
	Instructions string
	Options      map[string]string
}

// Answer is the chosen option of a question.
type Answer struct {
	Choice     string
	Confidence float64
}

// APIError is a non-200 answer from the endpoint.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("typesafe: %d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

// Client calls the endpoint.
type Client struct {
	Key   string
	URL   string
	Model string
	HTTP  *http.Client
	// Retries is how many times a 429, 529 or other 5xx answer is retried.
	Retries int
	// Backoff is the wait before the first retry; it doubles each time.
	Backoff time.Duration
	Budget  *Budget
}

// New returns a Client for the API key with the production endpoint, the
// jev-latest model and the budget.
func New(key string, budget *Budget) *Client {
	return &Client{
		Key:     key,
		URL:     DefaultURL,
		Model:   "jev-latest",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Retries: 4,
		Backoff: 2 * time.Second,
		Budget:  budget,
	}
}

type answer struct {
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

type response struct {
	Answers map[string]answer `json:"answers"`
	Usage   struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// Ask asks the questions about the state in one request and spends its
// tokens from the budget. It fails when any answer is missing or is not one
// of its question's options, and with ErrBudget, without a request, once the
// budget is spent.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Question) (map[string]Answer, error) {
	if c.Budget != nil && !c.Budget.Left() {
		return nil, ErrBudget
	}
	questions := map[string]any{}
	for id, q := range qs {
		questions[id] = map[string]any{"type": "choice", "instructions": q.Instructions, "criteria": q.Options}
	}
	body, err := json.Marshal(map[string]any{"model": c.Model, "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	r, err := c.request(ctx, body)
	if err != nil {
		return nil, err
	}
	if c.Budget != nil {
		c.Budget.Spend(r.Usage.InputTokens + r.Usage.OutputTokens)
	}
	out := map[string]Answer{}
	for id, q := range qs {
		a, ok := r.Answers[id]
		if !ok {
			return nil, fmt.Errorf("typesafe: no answer to %s", id)
		}
		if _, ok := q.Options[a.Choice]; !ok {
			return nil, fmt.Errorf("typesafe: answer %q to %s is not one of its options", a.Choice, id)
		}
		out[id] = Answer{Choice: a.Choice, Confidence: a.Confidence}
	}
	return out, nil
}

func (c *Client) request(ctx context.Context, body []byte) (response, error) {
	wait := c.Backoff
	for attempt := 0; ; attempt++ {
		res, err := c.post(ctx, body)
		var apiErr *APIError
		if err == nil || !errors.As(err, &apiErr) || !retryable(apiErr.Status) || attempt >= c.Retries {
			return res, err
		}
		select {
		case <-ctx.Done():
			return response{}, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func (c *Client) post(ctx context.Context, body []byte) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1000))
		return response{}, &APIError{Status: res.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	var r response
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return response{}, fmt.Errorf("typesafe: decode answer: %w", err)
	}
	return r, nil
}

// Budget counts the tokens spent per UTC day, in memory. It is safe for
// concurrent use.
type Budget struct {
	// Daily caps the tokens per day; requests in flight when it is reached
	// may overshoot it.
	Daily int64
	Now   func() time.Time

	mu   sync.Mutex
	day  string
	used int64
}

// Usage is the day's spending.
type Usage struct {
	Day   string `json:"day"`
	Used  int64  `json:"used"`
	Daily int64  `json:"daily"`
}

func (b *Budget) today() string {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	d := now().UTC().Format(time.DateOnly)
	if d != b.day {
		b.day, b.used = d, 0
	}
	return d
}

// Left reports whether any of the day's tokens are left.
func (b *Budget) Left() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.today()
	return b.used < b.Daily
}

// Spend counts n tokens against the day.
func (b *Budget) Spend(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.today()
	b.used += n
}

// Usage returns the day's spending.
func (b *Budget) Usage() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Usage{Day: b.today(), Used: b.used, Daily: b.Daily}
}
