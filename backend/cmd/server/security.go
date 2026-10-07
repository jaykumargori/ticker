package main

// HTTP hardening: security headers, client-IP resolution, per-IP rate and
// connection limits, timeouts, access logging and a locked-down static server.
// Every limit is configurable (see securityConfig) so ops can tune it per
// deployment without code changes.

import (
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type securityConfig struct {
	rateLimit     bool    // RATE_LIMIT=off disables per-IP limits (load tests only)
	trustProxy    bool    // TRUST_PROXY=true: take client IP from X-Forwarded-For (only behind ALB/CloudFront)
	hsts          bool    // HSTS=true, or implied by direct TLS
	apiRPS        float64 // general API, per IP
	upstreamRPS   float64 // endpoints that can trigger upstream fetches (news, candles)
	imageRPS      float64 // thumbnail proxy
	wsConnectRPS  float64 // new WebSocket connections, per IP
	maxConnsPerIP int64
	wsMsgsPerSec  int // client→server control messages per connection
}

func loadSecurityConfig() securityConfig {
	return securityConfig{
		rateLimit:     !strings.EqualFold(env("RATE_LIMIT", "on"), "off"),
		trustProxy:    strings.EqualFold(env("TRUST_PROXY", "false"), "true"),
		hsts:          strings.EqualFold(env("HSTS", "false"), "true"),
		apiRPS:        float64(envInt("API_RPS", 20)),
		upstreamRPS:   float64(envInt("UPSTREAM_RPS", 3)),
		imageRPS:      float64(envInt("IMAGE_RPS", 30)),
		wsConnectRPS:  float64(envInt("WS_CONNECT_RPS", 2)),
		maxConnsPerIP: int64(envInt("MAX_CONNS_PER_IP", 20)),
		wsMsgsPerSec:  envInt("WS_MSGS_PER_SEC", 20),
	}
}

// ---------- security headers ----------

// securityHeaders applies a strict CSP and browser hardening headers to every
// response. connect-src names the WebSocket origin explicitly because older
// Safari versions don't treat ws(s): as covered by 'self'.
func securityHeaders(next http.Handler, sec securityConfig, tlsOn bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		host := r.Host
		csp := strings.Join([]string{
			"default-src 'self'",
			"script-src 'self'",
			// Inline style *attributes* only (chart/heatmap positioning); no inline <style> injection risk from user data.
			"style-src 'self' 'unsafe-inline'",
			"img-src 'self' data:",
			"font-src 'self'",
			"connect-src 'self' ws://" + host + " wss://" + host,
			"object-src 'none'",
			"base-uri 'none'",
			"form-action 'self'",
			"frame-ancestors 'none'",
		}, "; ")
		if tlsOn || sec.hsts {
			csp += "; upgrade-insecure-requests"
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY") // legacy browsers; CSP frame-ancestors is the modern control
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ---------- client IP ----------

// clientIP returns the caller's IP. X-Forwarded-For is trusted only when
// TRUST_PROXY=true (i.e. the server is reachable solely through a proxy that
// overwrites the header); otherwise any client could spoof it to dodge limits.
// The right-most entry is the one our proxy appended.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- per-IP rate limiting ----------

type visitor struct {
	lim  *rate.Limiter
	seen time.Time
}

// ipLimiter is a token bucket per client IP, with idle entries evicted so
// the map can't grow without bound under a spray of source addresses.
type ipLimiter struct {
	mu    sync.Mutex
	ips   map[string]*visitor
	rps   rate.Limit
	burst int
}

func newIPLimiter(rps float64, burst int) *ipLimiter {
	l := &ipLimiter{ips: map[string]*visitor{}, rps: rate.Limit(rps), burst: burst}
	go func() {
		for range time.Tick(time.Minute) {
			l.mu.Lock()
			for ip, v := range l.ips {
				if time.Since(v.seen) > 5*time.Minute {
					delete(l.ips, ip)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	v, ok := l.ips[ip]
	if !ok {
		v = &visitor{lim: rate.NewLimiter(l.rps, l.burst)}
		l.ips[ip] = v
	}
	v.seen = time.Now()
	l.mu.Unlock()
	return v.lim.Allow()
}

func limit(next http.Handler, l *ipLimiter, sec securityConfig) http.Handler {
	if !sec.rateLimit {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r, sec.trustProxy)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- WebSocket connection admission ----------

// connGate enforces the global and per-IP concurrent connection caps with an
// atomic reserve-then-check, so concurrent upgrades can't overshoot the cap
// (the old check-then-act read a counter and raced).
type connGate struct {
	max, perIP int64
	total      atomic.Int64
	mu         sync.Mutex
	byIP       map[string]int64
}

func newConnGate(max, perIP int64) *connGate {
	return &connGate{max: max, perIP: perIP, byIP: map[string]int64{}}
}

// acquire returns a release func, or a reason the connection is refused.
func (g *connGate) acquire(ip string, enforcePerIP bool) (release func(), reason string) {
	if g.total.Add(1) > g.max {
		g.total.Add(-1)
		return nil, "server at capacity"
	}
	g.mu.Lock()
	if enforcePerIP && g.byIP[ip] >= g.perIP {
		g.mu.Unlock()
		g.total.Add(-1)
		return nil, "too many connections from your network"
	}
	g.byIP[ip]++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.byIP[ip]--; g.byIP[ip] <= 0 {
				delete(g.byIP, ip)
			}
			g.mu.Unlock()
			g.total.Add(-1)
		})
	}, ""
}

// ---------- access log ----------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController (and the WebSocket hijack) reach the real writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// accessLog records API/WS requests for security auditing. It logs the path
// only, never the query string or headers (no tokens, cookies or user data in logs).
func accessLog(next http.Handler, sec securityConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /ws is not wrapped: the WebSocket upgrade needs the raw http.Hijacker,
		// so the WS handler logs its own admission decisions.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		level := slog.LevelDebug
		if sw.status >= 400 {
			level = slog.LevelWarn // denials and errors are always visible
		}
		slog.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ip", clientIP(r, sec.trustProxy), "ms", time.Since(start).Milliseconds())
	})
}

// ---------- static files ----------

// staticFiles serves the built SPA without directory listings or dotfiles.
func staticFiles(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		if strings.Contains(p, "/.") || (strings.HasSuffix(r.URL.Path, "/") && p != "/") {
			http.NotFound(w, r) // dotfiles (.env, .git) and directory listings
			return
		}
		// Vite emits content-hashed /assets/* (safe to cache forever). index.html
		// must revalidate, or browsers keep loading the previous build after a deploy.
		if strings.HasPrefix(p, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// apiTimeout bounds handler time for JSON endpoints (never wrap /ws with it).
func apiTimeout(next http.Handler) http.Handler {
	return http.TimeoutHandler(next, 15*time.Second, `{"error":"timeout"}`)
}
