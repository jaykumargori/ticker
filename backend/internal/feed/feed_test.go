package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"ticker/internal/hub"
)

func TestSimIndexTracksMembers(t *testing.T) {
	s := &Sim{TPS: 0}
	if err := s.Init(context.Background(), hub.New()); err != nil {
		t.Fatal(err)
	}
	if len(s.indices) != len(simIndices) {
		t.Fatalf("indices = %d", len(s.indices))
	}
	ix := &s.indices[0]
	if len(ix.members) != 50 {
		t.Fatalf("NIFTY 50 members = %d", len(ix.members))
	}
	// All members at reference price → index equals its base exactly.
	copy(s.price, s.ref)
	if v := s.indexValue(ix); v != ix.base {
		t.Fatalf("index at ref = %v, want %v", v, ix.base)
	}
	// All members +1% → index +1%.
	for i := range s.price {
		s.price[i] = s.ref[i] * 1.01
	}
	if v := s.indexValue(ix); math.Abs(v/ix.base-1.01) > 1e-4 {
		t.Fatalf("index after +1%% = %v", v)
	}
}

func TestBinanceResolveBisectsBadSymbols(t *testing.T) {
	bad := map[string]bool{"BADUSDT": true, "GONEUSDT": true}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var syms []string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("symbols")), &syms)
		out := struct {
			Symbols []bnSymbolInfo `json:"symbols"`
		}{}
		for _, s := range syms {
			if bad[s] { // Binance fails the whole batch on one invalid symbol
				http.Error(w, `{"code":-1121,"msg":"Invalid symbol."}`, http.StatusBadRequest)
				return
			}
			out.Symbols = append(out.Symbols, bnSymbolInfo{Symbol: s, Status: "TRADING"})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	syms := []string{"AUSDT", "BUSDT", "BADUSDT", "CUSDT", "DUSDT", "EUSDT", "GONEUSDT", "FUSDT", "GUSDT", "HUSDT", "IUSDT", "JUSDT", "KUSDT", "LUSDT", "MUSDT", "NUSDT"}
	b := &Binance{REST: srv.URL, log: slog.Default()}
	got := b.resolve(context.Background(), syms)

	var names []string
	for _, in := range got {
		names = append(names, in.Symbol)
	}
	if len(names) != len(syms)-2 || slices.Contains(names, "BADUSDT") || slices.Contains(names, "GONEUSDT") {
		t.Fatalf("resolved %v", names)
	}
	if c := calls.Load(); c >= int32(len(syms)) {
		t.Fatalf("bisection made %d calls; per-symbol would be %d", c, len(syms))
	}
}
