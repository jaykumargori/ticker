package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/candles"
	"ticker/internal/fx"
	"ticker/internal/hub"
	"ticker/internal/news"
)

func testConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>t</title>"), 0o600))
	must(os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o600))
	must(os.MkdirAll(filepath.Join(dir, "assets"), 0o700))
	must(os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("1"), 0o600))
	return config{
		writeTimeout: time.Second, maxSubs: 100, maxClients: 100, staticDir: dir,
		origins: nil, // same-origin only
		sec: securityConfig{rateLimit: true, apiRPS: 5, upstreamRPS: 1, imageRPS: 5,
			wsConnectRPS: 100, maxConnsPerIP: 2, wsMsgsPerSec: 5},
	}
}

func testServer(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	h := hub.New()
	h.Register(hub.Instrument{Exchange: "X", Segment: "EQ", Symbol: "A", Name: "A", Decimals: 2, Currency: "USD"})
	srv := httptest.NewServer(routes(h, candles.NewStore(), fx.New(), news.New(), cfg, false))
	t.Cleanup(srv.Close)
	return srv
}

func TestSecurityHeaders(t *testing.T) {
	srv := testServer(t, testConfig(t))
	for _, p := range []string{"/", "/api/instruments"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "script-src 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP missing %q: %s", p, want, csp)
			}
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: hardening headers missing: %v", p, resp.Header)
		}
		if resp.Header.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS must not be sent over plain HTTP", p)
		}
	}
}

func TestStaticLockdown(t *testing.T) {
	srv := testServer(t, testConfig(t))
	for path, want := range map[string]int{"/": 200, "/assets/app.js": 200, "/assets/": 404, "/.env": 404, "/assets/../.env": 404} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestAPIRateLimit(t *testing.T) {
	srv := testServer(t, testConfig(t)) // apiRPS 5, burst 10
	got429 := false
	for i := 0; i < 30 && !got429; i++ {
		resp, err := http.Get(srv.URL + "/api/instruments")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = resp.Header.Get("Retry-After") != ""
		}
	}
	if !got429 {
		t.Fatal("expected 429 with Retry-After after exceeding the API budget")
	}
}

func TestClientIPTrust(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4444"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if ip := clientIP(r, false); ip != "10.0.0.5" {
		t.Errorf("untrusted proxy: got %s, want socket address (XFF is spoofable)", ip)
	}
	if ip := clientIP(r, true); ip != "203.0.113.9" {
		t.Errorf("trusted proxy: got %s, want right-most XFF entry", ip)
	}
}

func TestConnGate(t *testing.T) {
	g := newConnGate(3, 2)
	r1, _ := g.acquire("a", true)
	r2, _ := g.acquire("a", true)
	if r, reason := g.acquire("a", true); r != nil || !strings.Contains(reason, "your network") {
		t.Fatalf("third conn from same IP should be refused, got %q", reason)
	}
	r3, _ := g.acquire("b", true)
	if r, reason := g.acquire("c", true); r != nil || reason != "server at capacity" {
		t.Fatalf("global cap not enforced: %q", reason)
	}
	r1()
	r1() // idempotent release
	if r, _ := g.acquire("c", true); r == nil {
		t.Fatal("slot not freed after release")
	}
	r2()
	r3()
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws" }

func TestWebSocketOriginAndFlood(t *testing.T) {
	srv := testServer(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Cross-site WebSocket hijacking: a foreign Origin must be refused.
	_, resp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err=%v status=%v, want 403", err, resp)
	}

	// Same-origin works, but a message flood closes the socket with 1008.
	conn, _, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	go func() {
		for range 200 {
			if conn.Write(ctx, websocket.MessageText, []byte(`{"a":"unsubscribe","v":[1]}`)) != nil {
				return
			}
		}
	}()
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("flood: closed with %v, want policy violation (1008)", err)
			}
			return
		}
	}
}

func TestWebSocketPerIPCap(t *testing.T) {
	srv := testServer(t, testConfig(t)) // maxConnsPerIP 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}}
	var conns []*websocket.Conn
	for range 2 {
		c, _, err := websocket.Dial(ctx, wsURL(srv), opts)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	if _, resp, err := websocket.Dial(ctx, wsURL(srv), opts); err == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third connection from one IP should be refused with 503, got err=%v", err)
	}
	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}
}
