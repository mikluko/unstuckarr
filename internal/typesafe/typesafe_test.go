package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fake(t *testing.T, choice string, overload int64) (*Client, *atomic.Int64) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Model     string `json:"model"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "jev-latest" || req.Questions["q"].Type != "choice" {
			http.Error(w, "bad request", http.StatusUnprocessableEntity)
			return
		}
		if n <= overload {
			http.Error(w, "overloaded", 529)
			return
		}
		_, _ = w.Write([]byte(`{"answers": {"q": {"type": "choice", "choice": "` + choice + `", "confidence": 0.9}}, "usage": {"input_tokens": 300, "output_tokens": 30}}`))
	}))
	t.Cleanup(srv.Close)
	c := New("key", &Budget{Daily: 500})
	c.URL, c.Backoff = srv.URL, time.Millisecond
	return c, &requests
}

var question = map[string]Question{"q": {Instructions: "Which?", Options: map[string]string{"a": "first", "b": "second"}}}

func TestAsk(t *testing.T) {
	c, requests := fake(t, "b", 2)
	ans, err := c.Ask(context.Background(), map[string]any{"x": 1}, question)
	if err != nil {
		t.Fatal(err)
	}
	if ans["q"] != (Answer{Choice: "b", Confidence: 0.9}) {
		t.Fatalf("answer %+v", ans["q"])
	}
	if requests.Load() != 3 {
		t.Fatalf("%d requests, want two 529s retried", requests.Load())
	}
	if u := c.Budget.Usage(); u.Used != 330 {
		t.Fatalf("used %d", u.Used)
	}
	if _, err := c.Ask(context.Background(), nil, question); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ask(context.Background(), nil, question); !errors.Is(err, ErrBudget) {
		t.Fatalf("err %v, want ErrBudget once 660 of 500 are spent", err)
	}
	if requests.Load() != 4 {
		t.Fatalf("%d requests, want none once the budget is spent", requests.Load())
	}
}

func TestAskOutsideOptions(t *testing.T) {
	c, _ := fake(t, "c", 0)
	_, err := c.Ask(context.Background(), nil, question)
	if err == nil || !strings.Contains(err.Error(), "not one of its options") {
		t.Fatalf("err %v", err)
	}
}

func TestBudgetDay(t *testing.T) {
	now := time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC)
	b := &Budget{Daily: 10, Now: func() time.Time { return now }}
	b.Spend(10)
	if b.Left() {
		t.Fatal("the day's budget is spent")
	}
	now = now.Add(2 * time.Hour)
	if !b.Left() || b.Usage().Day != "2026-10-24" {
		t.Fatalf("a new UTC day starts afresh: %+v", b.Usage())
	}
}
