package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikluko/unstuckarr/internal/arr"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(env(map[string]string{"RADARR_URL": "http://radarr:7878/", "RADARR_API_KEY": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.apps) != 1 || c.apps[0] != (app{kind: arr.Radarr, url: "http://radarr:7878", key: "k"}) {
		t.Fatalf("apps %+v", c.apps)
	}
	if c.addr != ":8080" || c.apply || c.interval != 10*time.Minute || c.minSize != 50e6 || c.dailyTokens != 200000 || c.webhookToken != "" {
		t.Fatalf("defaults %+v", c)
	}
	c, err = loadConfig(env(map[string]string{"SONARR_URL": "http://s", "SONARR_API_KEY": "k", "UNSTUCKARR_APPLY": "true",
		"UNSTUCKARR_MIN_SIZE": "100MiB", "UNSTUCKARR_INTERVAL": "1m"}))
	if err != nil || !c.apply || c.minSize != 100<<20 || c.interval != time.Minute || c.apps[0].kind != arr.Sonarr {
		t.Fatalf("%+v, %v", c, err)
	}
	for _, bad := range []map[string]string{
		{},
		{"RADARR_URL": "http://r"},
		{"SONARR_API_KEY": "k"},
		{"RADARR_URL": "http://r", "RADARR_API_KEY": "k", "UNSTUCKARR_APPLY": "yes please"},
		{"RADARR_URL": "http://r", "RADARR_API_KEY": "k", "UNSTUCKARR_MIN_SIZE": "big"},
		{"RADARR_URL": "http://r", "RADARR_API_KEY": "k", "UNSTUCKARR_INTERVAL": "0s"},
	} {
		if _, err := loadConfig(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestParseSize(t *testing.T) {
	for s, want := range map[string]int64{"0": 0, "123": 123, "50MB": 50e6, "50 MB": 50e6, "1GiB": 1 << 30, "2KB": 2000, "7B": 7} {
		if got, err := parseSize(s); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", s, got, err)
		}
	}
}

type status string

func (s status) StatusJSON() ([]byte, error) { return []byte(s), nil }

func TestHandler(t *testing.T) {
	radarr := make(chan struct{}, 1)
	h := handler("secret", status(`{"apply":false}`), map[arr.Kind]chan struct{}{arr.Radarr: radarr}, slog.New(slog.DiscardHandler))
	do := func(method, target, pass, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if pass != "" {
			r.SetBasicAuth("radarr", pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/healthz", "", ""); w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Fatalf("healthz %d %q", w.Code, w.Body)
	}
	if w := do("GET", "/status", "", ""); w.Code != http.StatusOK || w.Body.String() != `{"apply":false}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d %q", w.Code, w.Body)
	}
	if w := do("POST", "/webhook?app=radarr", "", `{"eventType":"Test"}`); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no auth: %d", w.Code)
	}
	if w := do("POST", "/webhook?app=radarr", "wrong", `{"eventType":"Test"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := do("POST", "/webhook?app=sonarr", "secret", `{"eventType":"Test"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured app: %d", w.Code)
	}
	if w := do("POST", "/webhook?app=radarr", "secret", `{"eventType":"Grab"}`); w.Code != http.StatusNoContent || len(radarr) != 0 {
		t.Fatalf("grab: %d, %d triggers", w.Code, len(radarr))
	}
	for range 2 {
		if w := do("POST", "/webhook?app=radarr", "secret", `{"eventType":"ManualInteractionRequired","downloadId":"X"}`); w.Code != http.StatusAccepted {
			t.Fatalf("manual interaction: %d", w.Code)
		}
	}
	if len(radarr) != 1 {
		t.Fatalf("%d triggers, want one pending run", len(radarr))
	}

	h = handler("", status(`{}`), map[arr.Kind]chan struct{}{arr.Radarr: radarr}, slog.New(slog.DiscardHandler))
	if w := do("POST", "/webhook?app=radarr", "", `{"eventType":"Test"}`); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("no token, no webhook: %d", w.Code)
	}
}
