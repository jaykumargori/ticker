package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/candles"
	"ticker/internal/hub"
)

// DefaultBinanceSymbols is the fallback list, used only if discovery fails
// (e.g. the REST endpoint is unreachable at startup).
var DefaultBinanceSymbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "DOGEUSDT", "ADAUSDT", "TRXUSDT",
	"AVAXUSDT", "LINKUSDT", "DOTUSDT", "LTCUSDT", "BCHUSDT", "NEARUSDT", "UNIUSDT", "APTUSDT",
	"ATOMUSDT", "ETCUSDT", "FILUSDT", "ARBUSDT", "OPUSDT", "SUIUSDT", "INJUSDT", "AAVEUSDT",
	"PEPEUSDT", "SHIBUSDT", "XLMUSDT", "HBARUSDT", "ICPUSDT", "TONUSDT", "SEIUSDT", "TIAUSDT",
}

const (
	// Binance allows 1024 streams per connection and we use 2 per symbol.
	maxBinanceSymbols = 500
	// Binance accepts at most 5 incoming messages/second per connection.
	subscribePause = 250 * time.Millisecond
	streamsPerMsg  = 100
)

// Binance streams real trades (aggTrade) + 24h stats (miniTicker) from the
// public market-data endpoint. No API key required.
//
// The instrument list is dynamic: the top N USDT pairs by 24h quote volume
// (stablecoin pairs excluded) plus any pinned symbols. It is re-ranked every
// Refresh. Newly popular pairs are registered and subscribed on the live
// socket. Pairs that drop out stay listed, so tokens and saved watchlists
// remain valid.
type Binance struct {
	WS, REST string
	Pinned   []string      // always included (BINANCE_SYMBOLS)
	Top      int           // top-N by 24h quote volume; 0 = pinned (or fallback) only
	Refresh  time.Duration // re-rank interval; 0 = never
	Candles  *candles.Store

	h   *hub.Hub
	log *slog.Logger

	mu     sync.RWMutex
	tokens map[string]uint32 // upper-case symbol → token

	connMu sync.Mutex
	conn   *websocket.Conn // live socket, for subscribing new symbols
	msgID  int
}

func (b *Binance) Name() string { return "binance" }

type bnSymbolInfo struct {
	Symbol     string `json:"symbol"`
	Status     string `json:"status"`
	BaseAsset  string `json:"baseAsset"`
	QuoteAsset string `json:"quoteAsset"`
	Filters    []struct {
		FilterType string `json:"filterType"`
		TickSize   string `json:"tickSize"`
	} `json:"filters"`
}

// bnTicker24 is one row of /api/v3/ticker/24hr (MINI or FULL).
type bnTicker24 struct {
	Symbol      string `json:"symbol"`
	Last        string `json:"lastPrice"`
	Open        string `json:"openPrice"`
	High        string `json:"highPrice"`
	Low         string `json:"lowPrice"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quoteVolume"`
	CloseTime   int64  `json:"closeTime"`
}

func (b *Binance) Init(ctx context.Context, h *hub.Hub) error {
	b.h, b.tokens = h, map[string]uint32{}
	b.log = slog.With("feed", "binance")

	want := slices.Clone(b.Pinned)
	var rows map[string]bnTicker24
	if b.Top > 0 {
		top, r, err := b.discover(ctx)
		if err != nil {
			b.log.Warn("discovery failed; using fallback list", "err", err)
			want = append(want, DefaultBinanceSymbols...)
		} else {
			want, rows = append(want, top...), r
		}
	} else if len(want) == 0 {
		want = DefaultBinanceSymbols
	}

	b.add(ctx, want, rows)
	if b.count() == 0 {
		return fmt.Errorf("no tradable symbols")
	}
	b.log.Info("initialised", "symbols", b.count(), "dynamic", b.Top > 0)
	return nil
}

func (b *Binance) count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.tokens)
}

func (b *Binance) token(sym string) (uint32, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.tokens[sym]
	return t, ok
}

// stableBases are USD/EUR-pegged assets: their USDT pairs sit at ~1.00 and
// would crowd the top of a volume ranking with flat lines.
var stableBases = map[string]bool{
	"USDC": true, "USD1": true, "RLUSD": true, "FDUSD": true, "TUSD": true, "USDP": true, "DAI": true,
	"BUSD": true, "PYUSD": true, "EUR": true, "EURI": true, "AEUR": true, "XUSD": true, "BFUSD": true,
	"USDE": true, "FRAX": true, "USDS": true, "USDD": true, "GUSD": true, "LUSD": true,
}

// isStable flags pegged pairs by name, or by behaviour: price ≈ 1 with a
// < 0.5% daily range. The heuristic catches new stablecoins the list misses.
func isStable(base string, r bnTicker24) bool {
	if stableBases[base] {
		return true
	}
	last, hi, lo := pf(r.Last), pf(r.High), pf(r.Low)
	return last > 0.97 && last < 1.03 && (hi-lo)/last < 0.005
}

// discover ranks USDT pairs by 24h quote volume. A single ~1 MB request
// (type=MINI) covers every symbol, and the same rows seed initial quotes.
func (b *Binance) discover(ctx context.Context) ([]string, map[string]bnTicker24, error) {
	var all []bnTicker24
	if err := getJSON(ctx, b.REST+"/api/v3/ticker/24hr?type=MINI", &all, ""); err != nil {
		return nil, nil, err
	}
	rows := make(map[string]bnTicker24, len(all))
	cands := make([]bnTicker24, 0, len(all)/4)
	for _, r := range all {
		base, ok := strings.CutSuffix(r.Symbol, "USDT")
		if !ok || base == "" || pf(r.QuoteVolume) <= 0 || isStable(base, r) {
			continue
		}
		cands = append(cands, r)
		rows[r.Symbol] = r
	}
	sort.Slice(cands, func(i, j int) bool { return pf(cands[i].QuoteVolume) > pf(cands[j].QuoteVolume) })
	n := min(b.Top, len(cands))
	out := make([]string, n)
	for i := range n {
		out[i] = cands[i].Symbol
	}
	return out, rows, nil
}

// add validates, registers and seeds symbols not yet known. It returns the
// symbols actually added.
func (b *Binance) add(ctx context.Context, syms []string, rows map[string]bnTicker24) []string {
	b.mu.RLock()
	room := maxBinanceSymbols - len(b.tokens)
	var fresh []string
	seen := map[string]bool{}
	for _, s := range syms {
		s = strings.ToUpper(strings.TrimSpace(s))
		if _, known := b.tokens[s]; !known && s != "" && !seen[s] {
			seen[s] = true
			fresh = append(fresh, s)
		}
	}
	b.mu.RUnlock()
	if len(fresh) > room {
		b.log.Warn("symbol cap reached; skipping extras", "cap", maxBinanceSymbols, "skipped", len(fresh)-room)
		fresh = fresh[:max(room, 0)]
	}
	if len(fresh) == 0 {
		return nil
	}

	// exchangeInfo answers in its own order; register in ranked order instead,
	// so lists (Explore, search) show the most active pairs first.
	infos := b.resolve(ctx, fresh)
	rank := make(map[string]int, len(fresh))
	for i, s := range fresh {
		rank[s] = i
	}
	sort.SliceStable(infos, func(i, j int) bool { return rank[infos[i].Symbol] < rank[infos[j].Symbol] })

	var added []string
	for _, in := range infos {
		if in.Status != "TRADING" {
			continue
		}
		dec := 2
		for _, f := range in.Filters {
			if f.FilterType == "PRICE_FILTER" {
				dec = decimalsOf(f.TickSize)
			}
		}
		tok := b.h.Register(hub.Instrument{Exchange: "CRYPTO", Segment: "CRYPTO", Symbol: in.Symbol,
			Name: in.BaseAsset + "/" + in.QuoteAsset, Decimals: dec, Currency: quoteCurrency(in.QuoteAsset)})
		if b.Candles != nil {
			b.Candles.SetLoader(tok, b.klines(in.Symbol)) // fetched lazily, on first chart view
		}
		b.mu.Lock()
		b.tokens[in.Symbol] = tok
		b.mu.Unlock()
		added = append(added, in.Symbol)
	}
	b.seed(ctx, added, rows)
	return added
}

// klines returns a history loader backed by GET /api/v3/klines.
// Row format: [openTimeMs, "o", "h", "l", "c", "v", closeTimeMs, ...].
func (b *Binance) klines(symbol string) candles.Loader {
	return func(ctx context.Context, interval string, limit int) ([]candles.Candle, error) {
		var rows [][]json.RawMessage
		u := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d", b.REST, url.QueryEscape(symbol), url.QueryEscape(interval), limit)
		if err := getJSON(ctx, u, &rows, ""); err != nil {
			return nil, err
		}
		out := make([]candles.Candle, 0, len(rows))
		for _, r := range rows {
			if len(r) < 6 {
				continue
			}
			var openMs int64
			var o, hi, lo, c, v string
			if json.Unmarshal(r[0], &openMs) != nil || json.Unmarshal(r[1], &o) != nil || json.Unmarshal(r[2], &hi) != nil ||
				json.Unmarshal(r[3], &lo) != nil || json.Unmarshal(r[4], &c) != nil || json.Unmarshal(r[5], &v) != nil {
				continue // skip malformed rows rather than failing the whole chart
			}
			out = append(out, candles.Candle{T: openMs / 1000, O: pf(o), H: pf(hi), L: pf(lo), C: pf(c), V: pf(v)})
		}
		return out, nil
	}
}

// resolve validates symbols. Binance rejects the whole batch if any symbol is
// unknown or delisted, so on failure the list is bisected: k bad symbols cost
// O(k·log n) requests instead of n.
func (b *Binance) resolve(ctx context.Context, syms []string) []bnSymbolInfo {
	if len(syms) == 0 || ctx.Err() != nil {
		return nil
	}
	infos, err := b.exchangeInfo(ctx, syms)
	if err == nil {
		return infos
	}
	if len(syms) == 1 {
		b.log.Warn("skipping symbol", "symbol", syms[0], "err", err)
		return nil
	}
	mid := len(syms) / 2
	return append(b.resolve(ctx, syms[:mid]), b.resolve(ctx, syms[mid:])...)
}

func (b *Binance) exchangeInfo(ctx context.Context, syms []string) ([]bnSymbolInfo, error) {
	q, _ := json.Marshal(syms)
	var resp struct {
		Symbols []bnSymbolInfo `json:"symbols"`
	}
	err := getJSON(ctx, b.REST+"/api/v3/exchangeInfo?symbols="+url.QueryEscape(string(q)), &resp, "")
	return resp.Symbols, err
}

// seed applies 24h stats so new instruments show values before their first
// trade. It uses discovery rows when available, else fetches them.
func (b *Binance) seed(ctx context.Context, syms []string, rows map[string]bnTicker24) {
	if len(syms) == 0 {
		return
	}
	var missing []string
	for _, s := range syms {
		if _, ok := rows[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		q, _ := json.Marshal(missing)
		var fetched []bnTicker24
		if err := getJSON(ctx, b.REST+"/api/v3/ticker/24hr?symbols="+url.QueryEscape(string(q)), &fetched, ""); err != nil {
			b.log.Warn("seed failed; quotes fill in from the stream", "err", err)
		}
		if rows == nil {
			rows = map[string]bnTicker24{}
		}
		for _, r := range fetched {
			rows[r.Symbol] = r
		}
	}
	ticks := make([]hub.Tick, 0, len(syms))
	for _, s := range syms {
		r, ok := rows[s]
		tok, known := b.token(s)
		if !ok || !known {
			continue
		}
		open := pf(r.Open)
		ticks = append(ticks, hub.Tick{
			Token:  tok,
			Fields: hub.FLTP | hub.FOHLC | hub.FVolume,
			LTP:    pf(r.Last), Open: open, High: pf(r.High), Low: pf(r.Low),
			Close:  open, // 24h change reference, matching Binance's priceChangePercent
			Volume: pf(r.Volume), TS: float64(r.CloseTime),
		})
	}
	b.h.Apply(ticks...)
}

func (b *Binance) Run(ctx context.Context) {
	if b.Top > 0 && b.Refresh > 0 {
		go b.refreshLoop(ctx)
	}
	runWithBackoff(ctx, b.log, b.stream)
}

// refreshLoop re-ranks periodically and adds newly popular pairs live.
func (b *Binance) refreshLoop(ctx context.Context) {
	tk := time.NewTicker(b.Refresh)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		top, rows, err := b.discover(ctx)
		if err != nil {
			b.log.Warn("re-rank failed; keeping current list", "err", err)
			continue
		}
		if added := b.add(ctx, top, rows); len(added) > 0 {
			b.log.Info("new pairs added", "symbols", added, "total", b.count())
			b.subscribeLive(ctx, added)
		}
	}
}

// subscribeLive adds streams to the current socket. If the socket is down, the
// next reconnect subscribes everything anyway.
func (b *Binance) subscribeLive(ctx context.Context, syms []string) {
	b.connMu.Lock()
	conn := b.conn
	b.connMu.Unlock()
	if conn != nil {
		if err := b.subscribe(ctx, conn, syms); err != nil {
			b.log.Warn("live subscribe failed; will apply on reconnect", "err", err)
		}
	}
}

// subscribe sends SUBSCRIBE messages in rate-limited chunks.
func (b *Binance) subscribe(ctx context.Context, conn *websocket.Conn, syms []string) error {
	streams := make([]string, 0, len(syms)*2)
	for _, s := range syms {
		ls := strings.ToLower(s)
		streams = append(streams, ls+"@aggTrade", ls+"@miniTicker")
	}
	for i := 0; i < len(streams); i += streamsPerMsg {
		chunk := streams[i:min(i+streamsPerMsg, len(streams))]
		b.connMu.Lock()
		b.msgID++
		msg, _ := json.Marshal(map[string]any{"method": "SUBSCRIBE", "params": chunk, "id": b.msgID})
		err := conn.Write(ctx, websocket.MessageText, msg)
		b.connMu.Unlock()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(subscribePause):
		}
	}
	return nil
}

type bnEnvelope struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// Only declare the fields we read. encoding/json matches keys case-insensitively,
// so declaring e.g. "e" would also swallow Binance's numeric "E".
type bnAggTrade struct {
	S string `json:"s"`
	P string `json:"p"`
	Q string `json:"q"`
	T int64  `json:"T"`
}

type bnMiniTicker struct {
	S string `json:"s"`
	O string `json:"o"`
	H string `json:"h"`
	L string `json:"l"`
	V string `json:"v"`
}

func (b *Binance) stream(ctx context.Context) error {
	// Connect bare and SUBSCRIBE in chunks: hundreds of streams don't fit in
	// a URL, and the same path serves live additions.
	conn, _, err := websocket.Dial(ctx, b.WS+"/stream", nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	b.connMu.Lock()
	b.conn = conn
	b.connMu.Unlock()
	defer func() {
		b.connMu.Lock()
		b.conn = nil
		b.connMu.Unlock()
	}()

	b.mu.RLock()
	all := make([]string, 0, len(b.tokens))
	for s := range b.tokens {
		all = append(all, s)
	}
	b.mu.RUnlock()
	subCtx, cancelSub := context.WithCancel(ctx)
	defer cancelSub()
	go func() { // subscribe while the read loop below drains (and answers pings)
		if err := b.subscribe(subCtx, conn, all); err != nil && subCtx.Err() == nil {
			b.log.Warn("subscribe failed", "err", err)
			conn.CloseNow() // forces a reconnect with a full subscribe
		}
	}()
	b.log.Info("connected", "symbols", len(all))

	var env bnEnvelope
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		env.Stream, env.Data = "", env.Data[:0]
		if err := json.Unmarshal(data, &env); err != nil || env.Stream == "" {
			continue // SUBSCRIBE acks ({"result":null,"id":n}) and anything unexpected
		}
		switch {
		case strings.HasSuffix(env.Stream, "@aggTrade"):
			var t bnAggTrade
			if json.Unmarshal(env.Data, &t) != nil {
				continue
			}
			if tok, ok := b.token(t.S); ok {
				b.h.Apply(hub.Tick{Token: tok, Fields: hub.FLTP, LTP: pf(t.P), Qty: pf(t.Q), TS: float64(t.T)})
			}
		case strings.HasSuffix(env.Stream, "@miniTicker"):
			var m bnMiniTicker
			if json.Unmarshal(env.Data, &m) != nil {
				continue
			}
			if tok, ok := b.token(m.S); ok {
				// LTP deliberately not taken from here: aggTrade is fresher and
				// mixing the two would make the price flicker backwards.
				o := pf(m.O)
				b.h.Apply(hub.Tick{Token: tok, Fields: hub.FOHLC | hub.FVolume,
					Open: o, High: pf(m.H), Low: pf(m.L), Close: o, Volume: pf(m.V)})
			}
		}
	}
}

func pf(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// decimalsOf("0.00010000") == 4
func decimalsOf(tick string) int {
	v := pf(tick)
	if v <= 0 {
		return 2
	}
	d := int(math.Round(-math.Log10(v)))
	return max(0, min(d, 10))
}

// quoteCurrency maps a Binance quote asset to an ISO currency for display
// conversion. USD stablecoins are treated as USD (≈1:1, ignoring small depegs).
func quoteCurrency(asset string) string {
	switch asset {
	case "USDT", "USDC", "FDUSD", "TUSD", "BUSD":
		return "USD"
	}
	return asset
}
