package insights

import (
	"math"
	"testing"
	"time"

	"ticker/internal/candles"
	"ticker/internal/news"
	"ticker/internal/proto"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func series(n int, f func(i int) float64) []candles.Candle {
	out := make([]candles.Candle, n)
	for i := range out {
		c := f(i)
		out[i] = candles.Candle{T: int64(i * 3600), O: c, H: c, L: c, C: c, V: 100}
	}
	return out
}

func find(r Result, key string) Signal {
	for _, s := range r.Signals {
		if s.Key == key {
			return s
		}
	}
	return Signal{}
}

func TestUptrendAndDowntrend(t *testing.T) {
	up := series(150, func(i int) float64 { return 100 + float64(i)*0.2 + math.Sin(float64(i))*0.3 })
	r := Compute(Input{Segment: "EQ", Hourly: up, Quote: proto.Quote{LTP: up[149].C}, Now: now, News: []news.Item{}})
	if s := find(r, "trend"); s.Tone != Positive {
		t.Fatalf("uptrend tone = %+v", s)
	}
	if s := find(r, "performance"); s.Tone != Positive {
		t.Fatalf("performance tone = %+v", s)
	}

	down := series(150, func(i int) float64 { return 200 - float64(i)*0.3 + math.Sin(float64(i))*0.3 })
	r = Compute(Input{Segment: "EQ", Hourly: down, Quote: proto.Quote{LTP: down[149].C}, Now: now, News: []news.Item{}})
	if s := find(r, "trend"); s.Tone != Negative {
		t.Fatalf("downtrend tone = %+v", s)
	}
	if r.Lean.Summary != "Signals lean negative" {
		t.Fatalf("lean = %+v", r.Lean)
	}
}

func TestRSIExtremesAreNotDirectional(t *testing.T) {
	// A one-way rally saturates RSI. "Overbought" must be reported, but as
	// neutral: it is both strength and a stretch warning, not a call.
	up := series(60, func(i int) float64 { return 100 + float64(i) })
	if v, _ := rsi(closesOf(up), 14); v < 99 {
		t.Fatalf("rsi of monotonic rally = %v", v)
	}
	if s := momentum(closesOf(up)); s.Tone != Neutral {
		t.Fatalf("overbought should be neutral, got %+v", s)
	}
}

func closesOf(bs []candles.Candle) []float64 {
	out := make([]float64, len(bs))
	for i, b := range bs {
		out[i] = b.C
	}
	return out
}

func TestShortHistoryIsUnknownNotGuessed(t *testing.T) {
	r := Compute(Input{Segment: "EQ", Hourly: series(10, func(int) float64 { return 100 }), Now: now})
	for _, k := range []string{"trend", "momentum", "volatility", "performance", "activity", "news"} {
		if s := find(r, k); s.Tone != Unknown {
			t.Errorf("%s with 10 bars = %+v, want unknown", k, s)
		}
	}
	if r.Lean.Summary != "Not enough data yet to read the signals" {
		t.Fatalf("lean = %+v", r.Lean)
	}
	// Empty input must not panic either.
	_ = Compute(Input{Now: now})
}

func TestVolatilityThresholdsByAssetClass(t *testing.T) {
	wiggle := series(130, func(i int) float64 { return 100 * (1 + 0.006*math.Sin(float64(i)*2.1)) })
	eq := volatility(closesOf(wiggle), "EQ")
	cr := volatility(closesOf(wiggle), "CRYPTO")
	if eq.Detail == cr.Detail {
		t.Fatalf("same volatility should read differently for stocks vs crypto: %q", eq.Detail)
	}
}

func TestHeadlineTone(t *testing.T) {
	cases := map[string]string{
		"Titan shares fall 5% after Q2 update":              Negative,
		"Reliance surges to record high on Jio IPO buzz":    Positive,
		"Jefferies increases weight in Reliance":            Neutral,
		"HDFC Bank downgraded; profit misses estimates":     Negative,
		"Bitcoin rallies as ETF inflows climb":              Positive,
		"Infosys falls despite strong growth outlook beats": Positive, // 3 pos vs 1 neg: a word count, by design
	}
	for title, want := range cases {
		if got := HeadlineTone(title); got != want {
			t.Errorf("%q = %s, want %s", title, got, want)
		}
	}
}

func TestNewsWindowAndOrdering(t *testing.T) {
	items := []news.Item{
		{Title: "Neutral note on the company", Published: now.Add(-time.Hour)},
		{Title: "Stock plunges after probe", Published: now.Add(-2 * time.Hour)},
		{Title: "Shares surge on upgrade", Published: now.Add(-3 * time.Hour)},
		{Title: "Old: shares surge", Published: now.Add(-100 * time.Hour)}, // outside 72h
	}
	s, heads := newsTone(items, now)
	if len(heads) != 3 || heads[0].Tone != Positive || heads[1].Tone != Negative || heads[2].Tone != Neutral {
		t.Fatalf("heads = %+v", heads)
	}
	if s.Value != "1 positive · 1 negative · 1 neutral" || s.Tone != Neutral {
		t.Fatalf("news signal = %+v", s)
	}
}
