// Command limiterd serves admission decisions under a configurable strategy.
//
// The strategy is chosen by environment variable so that a benchmark sweep is
// a configuration change, never a code change or a rebuild.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/sankalpcb/distributed-rate-limiter/internal/limiter"
	"github.com/sankalpcb/distributed-rate-limiter/internal/redisx"
)

type config struct {
	port         string
	redisAddr    string
	strategy     string
	rate         float64
	burst        int64
	window       time.Duration
	syncInterval time.Duration
	ttl          time.Duration
	failMode     limiter.FailMode
	replicaID    string
}

func loadConfig() (config, error) {
	fm, err := limiter.ParseFailMode(env("FAIL_MODE", "closed"))
	if err != nil {
		return config{}, err
	}
	return config{
		port:         env("PORT", "8080"),
		redisAddr:    env("REDIS_ADDR", "127.0.0.1:6379"),
		strategy:     env("LIMITER_STRATEGY", "centralized"),
		rate:         envFloat("RATE", 1000),
		burst:        int64(envFloat("BURST", 1000)),
		window:       envDuration("WINDOW", time.Second),
		syncInterval: envDuration("SYNC_INTERVAL", 100*time.Millisecond),
		ttl:          envDuration("KEY_TTL", 10*time.Minute),
		failMode:     fm,
		replicaID:    env("REPLICA_ID", env("K_REVISION", "local")),
	}, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	rdb := redisx.New(redisx.Options{Addr: cfg.redisAddr})
	defer func() { _ = rdb.Close() }()

	lcfg := limiter.Config{Rate: cfg.rate, Burst: cfg.burst, TTL: cfg.ttl}

	var lim limiter.Limiter
	var syncer *limiter.LocalSync
	switch cfg.strategy {
	case "centralized":
		lim = limiter.NewCentralized(rdb, lcfg, cfg.failMode)
	case "localsync":
		syncer = limiter.NewLocalSync(redisx.NewCounter(rdb), lcfg, cfg.window)
		lim = syncer
	case "slidingwindow":
		lim = limiter.NewSlidingWindow(rdb, lcfg, cfg.window, cfg.failMode)
	default:
		log.Error("unknown strategy", "strategy", cfg.strategy,
			"valid", "centralized|localsync|slidingwindow")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if syncer != nil {
		go runSyncLoop(ctx, syncer, cfg.syncInterval, log)
	}

	srv := newServer(lim, syncer, cfg, log)
	httpSrv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("limiterd listening",
			"port", cfg.port, "strategy", cfg.strategy, "rate", cfg.rate,
			"fail_mode", cfg.failMode.String(), "replica", cfg.replicaID)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// runSyncLoop reconciles localsync with the shared counter. A final sync on
// shutdown keeps a terminating replica's admissions from vanishing, which
// would otherwise let the fleet quietly over-admit during a scale-down.
func runSyncLoop(ctx context.Context, s *limiter.LocalSync, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := s.Sync(flushCtx, time.Now()); err != nil {
				log.Warn("final sync failed", "err", err)
			}
			cancel()
			return
		case now := <-t.C:
			if err := s.Sync(ctx, now); err != nil {
				log.Warn("sync failed", "err", err)
			}
		}
	}
}

type server struct {
	lim    limiter.Limiter
	syncer *limiter.LocalSync
	cfg    config
	log    *slog.Logger

	mu       sync.Mutex
	hist     *hdr.Histogram // server-side handler duration, microseconds
	admitted int64
	denied   int64
	errors   int64
}

func newServer(lim limiter.Limiter, syncer *limiter.LocalSync, cfg config, log *slog.Logger) *server {
	return &server{
		lim:    lim,
		syncer: syncer,
		cfg:    cfg,
		log:    log,
		hist:   hdr.New(1, 10_000_000, 3),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/allow", s.handleAllow)
	// Deliberately /health and not /healthz: Google's front end reserves
	// /healthz on Cloud Run and answers it itself with a 404 before the
	// request reaches the container. The failure is quiet and easy to
	// misread -- the returned 404 is Google's HTML error page rather than
	// Go's plain "404 page not found", which is the only clue that the
	// request never arrived.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /stats", s.handleStats)
	// The harness zeroes counters between runs so a warm-up does not
	// contaminate the reported distribution.
	mux.HandleFunc("POST /stats/reset", s.handleReset)
	return mux
}

type allowRequest struct {
	Key  string `json:"key"`
	Cost int64  `json:"cost"`
}

type allowResponse struct {
	Allowed    bool   `json:"allowed"`
	Remaining  int64  `json:"remaining"`
	RetryAfter int64  `json:"retry_after_ms"`
	Replica    string `json:"replica"`
}

func (s *server) handleAllow(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req allowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "bad request: expected {\"key\":string,\"cost\":int}", http.StatusBadRequest)
		return
	}
	if req.Cost <= 0 {
		req.Cost = 1
	}

	d, err := s.lim.Allow(r.Context(), req.Key, req.Cost, start)
	elapsed := time.Since(start)

	s.record(elapsed, d, err)

	if err != nil {
		// Fail-closed surfaces the outage rather than silently denying, so the
		// load generator can separate "limiter said no" from "limiter is down".
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !d.Allowed {
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(d.RetryAfter), 10))
		w.WriteHeader(http.StatusTooManyRequests)
	}
	_ = json.NewEncoder(w).Encode(allowResponse{
		Allowed:    d.Allowed,
		Remaining:  d.Remaining,
		RetryAfter: d.RetryAfter.Milliseconds(),
		Replica:    s.cfg.replicaID,
	})
}

func (s *server) record(elapsed time.Duration, d limiter.Decision, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.hist.RecordValue(elapsed.Microseconds())
	switch {
	case err != nil:
		s.errors++
	case d.Allowed:
		s.admitted++
	default:
		s.denied++
	}
}

type statsResponse struct {
	Replica      string  `json:"replica"`
	Strategy     string  `json:"strategy"`
	Admitted     int64   `json:"admitted"`
	Denied       int64   `json:"denied"`
	Errors       int64   `json:"errors"`
	SyncFailures int64   `json:"sync_failures"`
	P50US        float64 `json:"server_p50_us"`
	P99US        float64 `json:"server_p99_us"`
	P999US       float64 `json:"server_p999_us"`
	MaxUS        float64 `json:"server_max_us"`
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	resp := statsResponse{
		Replica:  s.cfg.replicaID,
		Strategy: s.cfg.strategy,
		Admitted: s.admitted,
		Denied:   s.denied,
		Errors:   s.errors,
		P50US:    float64(s.hist.ValueAtQuantile(50)),
		P99US:    float64(s.hist.ValueAtQuantile(99)),
		P999US:   float64(s.hist.ValueAtQuantile(99.9)),
		MaxUS:    float64(s.hist.Max()),
	}
	s.mu.Unlock()
	if s.syncer != nil {
		resp.SyncFailures = s.syncer.SyncFailures()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.hist.Reset()
	s.admitted, s.denied, s.errors = 0, 0, 0
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// retryAfterSeconds renders a duration for the Retry-After header, which is
// specified in whole seconds. Round up, and never emit 0 -- telling a client
// to retry immediately is how a thundering herd starts.
func retryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
