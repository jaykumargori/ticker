// Command server runs the ticker: upstream feeds → hub → WebSocket fan-out.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/candles"
	"ticker/internal/feed"
	"ticker/internal/fx"
	"ticker/internal/hub"
	"ticker/internal/insights"
	"ticker/internal/news"
)

type config struct {
	addr           string
	feeds          []string
	flush          time.Duration
	writeTimeout   time.Duration
	maxSubs        int
	maxClients     int64
	binanceWS      string
	binanceREST    string
	binanceSymbols []string // pinned symbols, always streamed
	binanceTop     int      // dynamic: top N USDT pairs by 24h volume (0 = pinned/fallback only)
	binanceRefresh time.Duration
	simTPS         int
	finnhubToken   string
	finnhubSymbols []string
	origins        []string
	staticDir      string
	tlsCert        string // TLS_CERT_FILE / TLS_KEY_FILE: serve HTTPS directly (else terminate TLS at the ALB)
	tlsKey         string
	sec            securityConfig
}

func loadConfig() config {
	return config{
		addr:           env("ADDR", ":8080"),
		feeds:          list(env("FEEDS", "binance,sim")),
		flush:          time.Duration(envInt("FLUSH_MS", 50)) * time.Millisecond,
		writeTimeout:   time.Duration(envInt("WRITE_TIMEOUT_MS", 5000)) * time.Millisecond,
		maxSubs:        envInt("MAX_SUBS", 3000),
		maxClients:     int64(envInt("MAX_CLIENTS", 20000)),
		binanceWS:      env("BINANCE_WS", "wss://data-stream.binance.vision"),
		binanceREST:    env("BINANCE_REST", "https://data-api.binance.vision"),
		binanceSymbols: list(os.Getenv("BINANCE_SYMBOLS")),
		binanceTop:     envIntAllowZero("BINANCE_TOP", 100),
		binanceRefresh: time.Duration(envInt("BINANCE_REFRESH_MIN", 30)) * time.Minute,
		simTPS:         envInt("SIM_TPS", 500),
		finnhubToken:   os.Getenv("FINNHUB_TOKEN"),
		finnhubSymbols: list(env("FINNHUB_SYMBOLS", "AAPL,MSFT,NVDA,AMZN,GOOGL,META,TSLA,AMD,NFLX,AVGO")),
		origins:        list(env("ALLOWED_ORIGINS", "localhost:5173,127.0.0.1:5173")),
		staticDir:      os.Getenv("STATIC_DIR"),
		tlsCert:        os.Getenv("TLS_CERT_FILE"),
		tlsKey:         os.Getenv("TLS_KEY_FILE"),
		sec:            loadSecurityConfig(),
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cfg := loadConfig()
	if cfg.flush < 10*time.Millisecond {
		cfg.flush = 10 * time.Millisecond
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := hub.New()
	store := candles.NewStore()
	h.OnTrade(store.Trade) // every trade also builds 1m/5m/15m/1h candles
	feeds := buildFeeds(cfg, store)
	if len(feeds) == 0 {
		slog.Error("no feeds enabled; set FEEDS to binance, sim and/or finnhub")
		os.Exit(1)
	}
	started := 0
	for _, f := range feeds {
		initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := f.Init(initCtx, h)
		cancel()
		if err != nil {
			// One broken upstream must not take down the others.
			slog.Error("feed init failed; feed disabled", "feed", f.Name(), "err", err)
			continue
		}
		go runSafely(ctx, f.Name(), f.Run)
		started++
	}
	if started == 0 {
		slog.Error("all feeds failed to initialise")
		os.Exit(1)
	}
	go runSafely(ctx, "hub", func(ctx context.Context) { h.Run(ctx, cfg.flush) })

	headlines := news.New()
	rates := fx.New()
	go rates.Run(ctx) // first fetch is immediate; then hourly

	tlsOn := cfg.tlsCert != "" && cfg.tlsKey != ""
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           routes(h, store, rates, headlines, cfg, tlsOn),
		ReadHeaderTimeout: 10 * time.Second, // slowloris
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// No global Read/WriteTimeout: they would cut long-lived WebSockets.
		// JSON routes get apiTimeout instead.
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		ErrorLog:  slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("listening", "addr", cfg.addr, "tls", tlsOn, "flush", cfg.flush, "instruments", len(h.Instruments()),
		"rateLimit", cfg.sec.rateLimit, "trustProxy", cfg.sec.trustProxy)
	if !cfg.sec.rateLimit {
		slog.Warn("RATE_LIMIT=off: per-IP limits disabled. Use only for load testing.")
	}
	var err error
	if tlsOn {
		err = srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func buildFeeds(cfg config, store *candles.Store) []feed.Feed {
	var out []feed.Feed
	for _, name := range cfg.feeds {
		switch name {
		case "binance":
			out = append(out, &feed.Binance{WS: cfg.binanceWS, REST: cfg.binanceREST, Pinned: cfg.binanceSymbols,
				Top: cfg.binanceTop, Refresh: cfg.binanceRefresh, Candles: store})
		case "sim":
			out = append(out, &feed.Sim{TPS: cfg.simTPS, Candles: store})
		case "finnhub":
			if cfg.finnhubToken == "" {
				slog.Warn("finnhub requested but FINNHUB_TOKEN is empty; skipping")
				continue
			}
			out = append(out, &feed.Finnhub{Token: cfg.finnhubToken, Symbols: cfg.finnhubSymbols})
		default:
			slog.Warn("unknown feed", "name", name)
		}
	}
	return out
}

func routes(h *hub.Hub, store *candles.Store, rates *fx.Service, headlines *news.Service, cfg config, tlsOn bool) http.Handler {
	mux := http.NewServeMux()
	sec := cfg.sec
	clientOpt := hub.ClientOptions{WriteTimeout: cfg.writeTimeout, Heartbeat: time.Second, MaxSubs: cfg.maxSubs, MaxMsgsPerSec: sec.wsMsgsPerSec}

	// Per-IP token buckets. Upstream-backed endpoints get a much tighter
	// budget so the server can't be used to hammer Binance/Bing/Google.
	apiLim := newIPLimiter(sec.apiRPS, int(sec.apiRPS*2))
	upLim := newIPLimiter(sec.upstreamRPS, 10)
	imgLim := newIPLimiter(sec.imageRPS, 60)
	wsLim := newIPLimiter(sec.wsConnectRPS, 5)
	gate := newConnGate(cfg.maxClients, sec.maxConnsPerIP)
	api := func(pattern string, l *ipLimiter, fn http.HandlerFunc) {
		mux.Handle(pattern, limit(apiTimeout(fn), l, sec))
	}

	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		// TODO(auth): verify the user's session/JWT here before upgrading, once accounts exist.
		ip := clientIP(r, sec.trustProxy)
		if sec.rateLimit && !wsLim.allow(ip) {
			slog.Warn("ws rejected", "reason", "connect rate", "ip", ip)
			w.Header().Set("Retry-After", "2")
			http.Error(w, "too many connection attempts", http.StatusTooManyRequests)
			return
		}
		release, reason := gate.acquire(ip, sec.rateLimit)
		if release == nil {
			slog.Warn("ws rejected", "reason", reason, "ip", ip)
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		defer release()
		// Origin allowlist blocks cross-site WebSocket hijacking (CSWSH).
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns:  cfg.origins,
			CompressionMode: websocket.CompressionDisabled, // small binary frames; deflate costs CPU per send
		})
		if err != nil {
			return // Accept already wrote the HTTP error
		}
		defer conn.CloseNow()
		err = h.Serve(r.Context(), conn, clientOpt)
		switch {
		case errors.Is(err, hub.ErrSlowClient):
			conn.Close(websocket.StatusPolicyViolation, "too slow")
		case errors.Is(err, hub.ErrRateLimited):
			slog.Warn("ws closed", "reason", "message rate", "ip", ip)
			conn.Close(websocket.StatusPolicyViolation, "rate limit exceeded")
		case websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled):
			slog.Debug("client closed", "err", err)
		}
	})
	// The list can grow at runtime (dynamic Binance ranking). Clients poll with
	// If-None-Match; an unchanged catalog costs a bodiless 304.
	api("GET /api/instruments", apiLim, func(w http.ResponseWriter, r *http.Request) {
		etag := `"cat-` + strconv.FormatUint(h.CatalogVersion(), 10) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, h.Instruments())
	})
	api("GET /api/stats", apiLim, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, h.Snapshot())
	})
	// Low-priority panels (movers, heatmap) poll this instead of streaming 194
	// instruments over the socket: about 12 KB every 2 s versus up to ~250 KB/s.
	api("GET /api/quotes", apiLim, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, h.Quotes())
	})
	// GET /api/candles?token=1&interval=1m&limit=300
	api("GET /api/candles", upLim, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tok, err := strconv.ParseUint(q.Get("token"), 10, 32)
		if err != nil || tok == 0 {
			http.Error(w, "token must be a positive integer", http.StatusBadRequest)
			return
		}
		iv := candles.IntervalIndex(q.Get("interval"))
		if iv < 0 {
			http.Error(w, "interval must be one of 1m, 5m, 15m, 1h", http.StatusBadRequest)
			return
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit <= 0 || limit > candles.Keep {
			limit = candles.Keep
		}
		bars, err := store.Get(r.Context(), uint32(tok), iv, limit)
		if errors.Is(err, candles.ErrUnknown) {
			writeJSON(w, []candles.Candle{}) // known instrument, no trades yet
			return
		}
		if err != nil {
			http.Error(w, "candles unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, bars)
	})
	// GET /api/fx: USD-based display rates plus the viewer's country when a CDN
	// supplies it (CloudFront / Cloudflare geo headers). The country only picks a
	// default display currency, so a spoofed header is harmless.
	api("GET /api/fx", apiLim, func(w http.ResponseWriter, r *http.Request) {
		country := r.Header.Get("CloudFront-Viewer-Country")
		if country == "" {
			country = r.Header.Get("CF-IPCountry")
		}
		snap := rates.Get()
		if snap == nil {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "exchange rates not loaded yet", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, struct {
			*fx.Snapshot
			Country string `json:"country,omitempty"`
		}{snap, country})
	})
	// GET /api/news?token=1: recent headlines (cached ~3 min per instrument).
	api("GET /api/news", upLim, func(w http.ResponseWriter, r *http.Request) {
		tok, err := strconv.ParseUint(r.URL.Query().Get("token"), 10, 32)
		if err != nil {
			http.Error(w, "token must be a positive integer", http.StatusBadRequest)
			return
		}
		ins, ok := h.Instrument(uint32(tok))
		if !ok {
			http.Error(w, "unknown instrument", http.StatusNotFound)
			return
		}
		res, err := headlines.Get(r.Context(), news.QueryFor(ins.Exchange, ins.Segment, ins.Symbol, ins.Name))
		if err != nil {
			slog.Warn("news fetch failed", "symbol", ins.Symbol, "err", err)
			http.Error(w, "news temporarily unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, res)
	})
	// GET /api/insights?token=1: transparent, rule-based signals (trend,
	// momentum, volatility, performance, activity, headline tone). Descriptive
	// only: no buy/sell verdicts (see internal/insights).
	api("GET /api/insights", upLim, func(w http.ResponseWriter, r *http.Request) {
		tok, err := strconv.ParseUint(r.URL.Query().Get("token"), 10, 32)
		if err != nil {
			http.Error(w, "token must be a positive integer", http.StatusBadRequest)
			return
		}
		ins, ok := h.Instrument(uint32(tok))
		if !ok {
			http.Error(w, "unknown instrument", http.StatusNotFound)
			return
		}
		q, _ := h.Quote(ins.Token)
		bars, err := store.Get(r.Context(), ins.Token, candles.IntervalIndex("1h"), candles.Keep)
		if err != nil && !errors.Is(err, candles.ErrUnknown) {
			slog.Warn("insights: candles", "symbol", ins.Symbol, "err", err)
		}
		var items []news.Item // nil = unavailable (reported as such, not as "no news")
		if res, err := headlines.Get(r.Context(), news.QueryFor(ins.Exchange, ins.Segment, ins.Symbol, ins.Name)); err == nil {
			items = res.Items
			if items == nil {
				items = []news.Item{}
			}
		}
		writeJSON(w, insights.Compute(insights.Input{
			Segment: ins.Segment, Simulated: ins.Exchange == "NSE-SIM",
			Quote: q, Hourly: bars, News: items, Now: time.Now(),
		}))
	})
	// Same-origin, allowlisted thumbnail proxy (see internal/news/images.go).
	mux.Handle("GET /api/news/img", limit(apiTimeout(news.NewImageProxy()), imgLim, sec))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	if cfg.staticDir != "" {
		mux.Handle("/", staticFiles(cfg.staticDir))
	}
	return securityHeaders(accessLog(mux, sec), sec, tlsOn)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json", "err", err)
	}
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v, err := strconv.Atoi(os.Getenv(k))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// envIntAllowZero is envInt for settings where 0 is meaningful (e.g. "off").
func envIntAllowZero(k string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k)))
	if err != nil || v < 0 {
		return def
	}
	return v
}

func list(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runSafely runs a long-lived component, restarting it after a panic so one
// malformed upstream message can't take down the whole server.
func runSafely(ctx context.Context, name string, run func(context.Context)) {
	for ctx.Err() == nil {
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("component panicked; restarting", "component", name, "panic", p)
				}
			}()
			run(ctx)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
