// Command unstuckarr imports the completed downloads Radarr and Sonarr leave
// in their queues because they cannot tell what the files are, through their
// own manual import. It is configured by the environment; see the README.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mikluko/unstuckarr/internal/arr"
	"github.com/mikluko/unstuckarr/internal/telemetry"
	"github.com/mikluko/unstuckarr/internal/typesafe"
	"github.com/mikluko/unstuckarr/internal/unstuck"
)

func main() {
	log := slog.New(telemetry.Handler{Handler: slog.NewJSONHandler(os.Stderr, nil)})
	if err := run(log); err != nil {
		log.Error("unstuckarr", "err", err)
		os.Exit(1)
	}
}

type app struct {
	kind     arr.Kind
	url, key string
}

type config struct {
	addr         string
	apps         []app
	apply        bool
	interval     time.Duration
	webhookToken string
	typesafeKey  string
	dailyTokens  int64
	minSize      int64
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		addr:         or(getenv("UNSTUCKARR_ADDR"), ":8080"),
		webhookToken: getenv("UNSTUCKARR_WEBHOOK_TOKEN"),
		typesafeKey:  getenv("TYPESAFE_API_KEY"),
	}
	for _, a := range []struct {
		kind arr.Kind
		env  string
	}{{arr.Radarr, "RADARR"}, {arr.Sonarr, "SONARR"}} {
		u, key := strings.TrimRight(getenv(a.env+"_URL"), "/"), getenv(a.env+"_API_KEY")
		switch {
		case u == "" && key == "":
			continue
		case u == "":
			return c, fmt.Errorf("%s_API_KEY is set, but not %s_URL", a.env, a.env)
		case key == "":
			return c, fmt.Errorf("%s_URL is set, but not %s_API_KEY", a.env, a.env)
		}
		c.apps = append(c.apps, app{kind: a.kind, url: u, key: key})
	}
	if len(c.apps) == 0 {
		return c, errors.New("neither RADARR_URL nor SONARR_URL is set")
	}
	var err error
	if c.apply, err = strconv.ParseBool(or(getenv("UNSTUCKARR_APPLY"), "false")); err != nil {
		return c, fmt.Errorf("UNSTUCKARR_APPLY: %w", err)
	}
	if c.interval, err = time.ParseDuration(or(getenv("UNSTUCKARR_INTERVAL"), "10m")); err != nil {
		return c, fmt.Errorf("UNSTUCKARR_INTERVAL: %w", err)
	}
	if c.interval <= 0 {
		return c, errors.New("UNSTUCKARR_INTERVAL: not positive")
	}
	if c.dailyTokens, err = strconv.ParseInt(or(getenv("UNSTUCKARR_DAILY_TOKENS"), "200000"), 10, 64); err != nil {
		return c, fmt.Errorf("UNSTUCKARR_DAILY_TOKENS: %w", err)
	}
	if c.minSize, err = parseSize(or(getenv("UNSTUCKARR_MIN_SIZE"), "50MB")); err != nil {
		return c, fmt.Errorf("UNSTUCKARR_MIN_SIZE: %w", err)
	}
	return c, nil
}

var units = []struct {
	suffix string
	n      int64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1},
}

// parseSize reads a byte count with an optional unit: B, KB, MB, GB in
// powers of 1000, KiB, MiB, GiB in powers of 1024.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.n
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("negative")
	}
	return n * mult, nil
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.Setup(ctx, "unstuckarr", log)
	if err != nil {
		return err
	}
	defer telemetry.Flush(ctx, shutdown, log)

	transport := otelhttp.NewTransport(http.DefaultTransport)
	budget := &typesafe.Budget{Daily: cfg.dailyTokens}
	eng := &unstuck.Engine{
		Apply:          cfg.apply,
		MinSize:        cfg.minSize,
		MinConfidence:  0.5,
		Budget:         budget,
		CommandPoll:    5 * time.Second,
		CommandTimeout: 6 * time.Hour,
		Log:            log,
	}
	if cfg.typesafeKey != "" {
		c := typesafe.New(cfg.typesafeKey, budget)
		c.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: transport}
		eng.LLM = c
	}
	httpc := &http.Client{Timeout: 5 * time.Minute, Transport: transport}
	triggers := map[arr.Kind]chan struct{}{}
	for _, a := range cfg.apps {
		triggers[a.kind] = make(chan struct{}, 1)
		go eng.Loop(ctx, a.kind, &arr.Client{Kind: a.kind, URL: a.url, Key: a.key, HTTP: httpc}, cfg.interval, triggers[a.kind])
	}

	hs := &http.Server{Addr: cfg.addr, Handler: handler(cfg.webhookToken, eng, triggers, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	kinds := make([]arr.Kind, len(cfg.apps))
	for i, a := range cfg.apps {
		kinds[i] = a.kind
	}
	log.Info("listening", "addr", cfg.addr, "apps", kinds, "apply", cfg.apply, "interval", cfg.interval.String(),
		"webhook", cfg.webhookToken != "", "llm", eng.LLM != nil)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type statusSource interface {
	StatusJSON() ([]byte, error)
}

// handler serves the health check, the status and, when token is set, the
// webhook that schedules a run of the application it names.
func handler(token string, status statusSource, triggers map[arr.Kind]chan struct{}, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, otelhttp.NewHandler(h, pattern)) }
	route("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	route("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		b, err := status.StatusJSON()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	if token != "" {
		route("POST /webhook", webhook(token, triggers, log))
	}
	return mux
}

// webhook schedules a run of the application named by the app query
// parameter on its ManualInteractionRequired and Test events, which it
// answers 202; other events are answered 204.
func webhook(token string, triggers map[arr.Kind]chan struct{}, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="unstuckarr"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		kind := arr.Kind(r.URL.Query().Get("app"))
		trigger, ok := triggers[kind]
		if !ok {
			http.Error(w, "app is not radarr or sonarr, or is not configured", http.StatusNotFound)
			return
		}
		var ev struct {
			EventType string `json:"eventType"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if ev.EventType != "ManualInteractionRequired" && ev.EventType != "Test" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case trigger <- struct{}{}:
		default:
		}
		log.InfoContext(r.Context(), "webhook", "app", kind, "event", ev.EventType)
		w.WriteHeader(http.StatusAccepted)
	}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
