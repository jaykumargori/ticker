package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"ticker/internal/hub"
)

// fakeBinance serves ticker/24hr (MINI) and exchangeInfo from an editable
// volume table, so tests can simulate the ranking changing over time.
type fakeBinance struct {
	mu     sync.Mutex
	volume map[string]float64 // symbol → 24h quote volume
	price  map[string]string
}

func (f *fakeBinance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v3/ticker/24hr":
		var rows []bnTicker24
		for s, v := range f.volume {
			p := f.price[s]
			if p == "" {
				p = "10"
			}
			hi := "11"
			if p == "1.0001" { // stablecoin: flat around 1.00
				hi = "1.0003"
			}
			rows = append(rows, bnTicker24{Symbol: s, Last: p, Open: p, High: hi, Low: p, Volume: "5", QuoteVolume: fmt.Sprint(v)})
		}
		_ = json.NewEncoder(w).Encode(rows)
	case "/api/v3/exchangeInfo":
		var syms []string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("symbols")), &syms)
		var out struct {
			Symbols []bnSymbolInfo `json:"symbols"`
		}
		for _, s := range syms {
			out.Symbols = append(out.Symbols, bnSymbolInfo{Symbol: s, Status: "TRADING", BaseAsset: s[:len(s)-4], QuoteAsset: "USDT"})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func TestBinanceDynamicDiscovery(t *testing.T) {
	fb := &fakeBinance{
		volume: map[string]float64{
			"USDCUSDT":  9e9, // stablecoin by name: excluded despite top volume
			"NEWSTUSDT": 8e9, // unknown stablecoin, caught by the price≈1 heuristic
			"BTCUSDT":   5e9, "ETHUSDT": 4e9, "SOLUSDT": 3e9, "DOGEUSDT": 1e9,
			"ETHBTC": 9e9, // not a USDT pair
		},
		price: map[string]string{"USDCUSDT": "1.0001", "NEWSTUSDT": "1.0001"},
	}
	srv := httptest.NewServer(fb)
	defer srv.Close()

	h := hub.New()
	b := &Binance{REST: srv.URL, Top: 3, Pinned: []string{"DOGEUSDT"}}
	if err := b.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, in := range h.Instruments() {
		got = append(got, in.Symbol)
	}
	slices.Sort(got)
	if want := []string{"BTCUSDT", "DOGEUSDT", "ETHUSDT", "SOLUSDT"}; !slices.Equal(got, want) {
		t.Fatalf("instruments = %v, want %v (top 3 non-stable + pinned)", got, want)
	}
	if q, _ := h.Quote(1); q.LTP == 0 {
		t.Fatal("instruments should be seeded from discovery rows")
	}

	// The ranking shifts: a new pair surges into the top 3.
	v0 := h.CatalogVersion()
	fb.mu.Lock()
	fb.volume["PEPEUSDT"] = 6e9
	fb.mu.Unlock()
	b.log = slog.Default()
	top, rows, err := b.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	added := b.add(context.Background(), top, rows)
	if !slices.Equal(added, []string{"PEPEUSDT"}) {
		t.Fatalf("refresh added %v, want [PEPEUSDT]", added)
	}
	if h.CatalogVersion() == v0 {
		t.Fatal("catalog version must change when instruments are added")
	}
	if _, ok := b.token("SOLUSDT"); !ok {
		t.Fatal("pairs that drop out of the top N must stay listed (stable tokens/watchlists)")
	}

	// No change → nothing added, version stable.
	v1 := h.CatalogVersion()
	if added := b.add(context.Background(), top, rows); len(added) != 0 || h.CatalogVersion() != v1 {
		t.Fatalf("idempotent refresh added %v", added)
	}
}

func TestBinanceDiscoveryFailureFallsBack(t *testing.T) {
	fb := &fakeBinance{volume: map[string]float64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/ticker/24hr", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "MINI" {
			http.Error(w, "down", http.StatusBadGateway) // discovery fails
			return
		}
		fb.ServeHTTP(w, r)
	})
	mux.Handle("/", fb)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := hub.New()
	b := &Binance{REST: srv.URL, Top: 50}
	if err := b.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Instruments()); n != len(DefaultBinanceSymbols) {
		t.Fatalf("fallback registered %d instruments, want %d", n, len(DefaultBinanceSymbols))
	}
}
