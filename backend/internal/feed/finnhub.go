package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/hub"
)

// Finnhub streams US equity trades. Needs a free API key (FINNHUB_TOKEN).
// Data only flows during US market hours. The token is never logged.
type Finnhub struct {
	Token   string
	Symbols []string

	h      *hub.Hub
	tokens map[string]uint32
	log    *slog.Logger
}

func (f *Finnhub) Name() string { return "finnhub" }

func (f *Finnhub) Init(ctx context.Context, h *hub.Hub) error {
	f.h, f.tokens = h, map[string]uint32{}
	f.log = slog.With("feed", "finnhub")
	for _, s := range f.Symbols {
		f.tokens[s] = h.Register(hub.Instrument{Exchange: "US", Segment: "EQ", Symbol: s, Name: s, Decimals: 2, Currency: "USD"})
		var q struct {
			C, H, L, O, PC float64
			T              int64
		}
		u := "https://finnhub.io/api/v1/quote?symbol=" + url.QueryEscape(s) + "&token=" + url.QueryEscape(f.Token)
		if err := getJSON(ctx, u, &q, f.Token); err != nil {
			f.log.Warn("seed failed", "symbol", s, "err", err)
			continue
		}
		h.Apply(hub.Tick{Token: f.tokens[s], Fields: hub.FLTP | hub.FOHLC,
			LTP: q.C, Open: q.O, High: q.H, Low: q.L, Close: q.PC, TS: float64(q.T * 1000)})
		time.Sleep(100 * time.Millisecond) // stay well under the free tier's 60 req/min burst
	}
	return nil
}

func (f *Finnhub) Run(ctx context.Context) { runWithBackoff(ctx, f.log, f.stream) }

func (f *Finnhub) stream(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, "wss://ws.finnhub.io?token="+url.QueryEscape(f.Token), nil)
	if err != nil {
		return sanitize(err, f.Token)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	for s := range f.tokens {
		msg, _ := json.Marshal(map[string]string{"type": "subscribe", "symbol": s})
		if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
			return sanitize(err, f.Token)
		}
	}
	f.log.Info("connected", "symbols", len(f.tokens))

	var msg struct {
		Type string `json:"type"`
		Data []struct {
			P float64 `json:"p"`
			S string  `json:"s"`
			T int64   `json:"t"`
			V float64 `json:"v"`
		} `json:"data"`
	}
	ticks := make([]hub.Tick, 0, 64)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return sanitize(err, f.Token)
		}
		msg.Data = msg.Data[:0]
		if json.Unmarshal(data, &msg) != nil || msg.Type != "trade" {
			continue
		}
		ticks = ticks[:0]
		for _, d := range msg.Data {
			if tok, ok := f.tokens[d.S]; ok {
				ticks = append(ticks, hub.Tick{Token: tok, Fields: hub.FLTP | hub.FAddQty, LTP: d.P, Qty: d.V, TS: float64(d.T)})
			}
		}
		f.h.Apply(ticks...)
	}
}
