// Package candles aggregates trades into OHLCV bars at fixed intervals and
// serves history for charts.
//
// Live bars are built from every trade the hub sees. History older than the
// server's uptime comes from a per-instrument Loader (Binance klines), fetched
// lazily on first request, de-duplicated across concurrent requests, and
// merged under the live bars. Simulated instruments are seeded at startup.
package candles

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Candle is one OHLCV bar. T is the bucket start in unix seconds.
type Candle struct {
	T int64   `json:"t"`
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"`
}

// Intervals supported, in seconds. Order matters (index into series arrays).
var Intervals = []struct {
	Name string
	Sec  int64
}{{"1m", 60}, {"5m", 300}, {"15m", 900}, {"1h", 3600}}

// Keep is the number of bars retained per series.
const Keep = 500

// IntervalIndex returns the index for a name, or -1.
func IntervalIndex(name string) int {
	for i, iv := range Intervals {
		if iv.Name == name {
			return i
		}
	}
	return -1
}

// Loader fetches up to limit historical bars for an interval, oldest first.
type Loader func(ctx context.Context, interval string, limit int) ([]Candle, error)

type series struct {
	bars        []Candle
	backfilled  bool
	lastAttempt time.Time // throttles retries after a failed backfill
}

type token struct {
	s      [4]series
	loader Loader
}

type call struct {
	done chan struct{}
	err  error
}

// Store is safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	tokens map[uint32]*token

	flightMu sync.Mutex
	inflight map[[2]uint32]*call // (token, interval) → running backfill
}

func NewStore() *Store {
	return &Store{tokens: map[uint32]*token{}, inflight: map[[2]uint32]*call{}}
}

func (s *Store) get(t uint32) *token {
	tk := s.tokens[t]
	if tk == nil {
		tk = &token{}
		s.tokens[t] = tk
	}
	return tk
}

// SetLoader registers a history source for a token.
func (s *Store) SetLoader(t uint32, l Loader) {
	s.mu.Lock()
	s.get(t).loader = l
	s.mu.Unlock()
}

// Seed installs history (oldest first) and marks the series backfilled.
func (s *Store) Seed(t uint32, interval int, bars []Candle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := &s.get(t).s[interval]
	sr.bars = merge(bars, sr.bars)
	sr.backfilled = true
}

// Trade folds one trade into every interval. tsMs is exchange time in ms.
func (s *Store) Trade(t uint32, price, qty, tsMs float64) {
	if price <= 0 {
		return
	}
	sec := int64(tsMs / 1000)
	if sec <= 0 {
		sec = time.Now().Unix()
	}
	s.mu.Lock()
	tk := s.get(t)
	for i, iv := range Intervals {
		sr := &tk.s[i]
		b := sec / iv.Sec * iv.Sec
		n := len(sr.bars)
		switch {
		case n == 0 || b > sr.bars[n-1].T:
			sr.bars = append(sr.bars, Candle{T: b, O: price, H: price, L: price, C: price, V: qty})
			if len(sr.bars) > 2*Keep { // amortised trim
				sr.bars = append(sr.bars[:0:0], sr.bars[len(sr.bars)-Keep:]...)
			}
		case b == sr.bars[n-1].T:
			c := &sr.bars[n-1]
			c.H = max(c.H, price)
			c.L = min(c.L, price)
			c.C = price
			c.V += qty
		default:
			// late trade for an older bucket: ignore (bars are append-only)
		}
	}
	s.mu.Unlock()
}

// ErrUnknown is returned for tokens with no data at all.
var ErrUnknown = errors.New("no candles for token")

// Get returns up to limit bars, backfilling history on first use.
func (s *Store) Get(ctx context.Context, t uint32, interval, limit int) ([]Candle, error) {
	s.mu.RLock()
	tk := s.tokens[t]
	need := tk != nil && tk.loader != nil && !tk.s[interval].backfilled &&
		time.Since(tk.s[interval].lastAttempt) > 30*time.Second
	s.mu.RUnlock()
	if tk == nil {
		return nil, ErrUnknown
	}
	if need {
		// History is optional: on failure, serve live bars only (retry later).
		_ = s.backfill(ctx, t, interval)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	bars := tk.s[interval].bars
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return append([]Candle(nil), bars...), nil
}

// backfill runs the loader once per (token, interval) even under concurrent requests.
func (s *Store) backfill(ctx context.Context, t uint32, interval int) error {
	key := [2]uint32{t, uint32(interval)}
	s.flightMu.Lock()
	if c, ok := s.inflight[key]; ok {
		s.flightMu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[key] = c
	s.flightMu.Unlock()

	defer func() {
		s.flightMu.Lock()
		delete(s.inflight, key)
		s.flightMu.Unlock()
		close(c.done)
	}()

	s.mu.RLock()
	loader := s.tokens[t].loader
	s.mu.RUnlock()

	// Detached from the request context so one impatient client can't
	// cancel the fetch other waiters depend on.
	lctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	hist, err := loader(lctx, Intervals[interval].Name, Keep)

	s.mu.Lock()
	defer s.mu.Unlock()
	sr := &s.tokens[t].s[interval]
	sr.lastAttempt = time.Now()
	if err != nil {
		c.err = err
		return err
	}
	sr.bars = merge(hist, sr.bars)
	sr.backfilled = true
	return nil
}

// merge puts history under live bars. The bucket both cover takes the
// history's open, the extremes of both, the live close and the larger volume
// (history already includes trades before the server started).
func merge(hist, live []Candle) []Candle {
	if len(live) == 0 {
		return trim(hist)
	}
	first := live[0].T
	out := make([]Candle, 0, len(hist)+len(live))
	for _, h := range hist {
		if h.T < first {
			out = append(out, h)
		} else if h.T == first {
			l := live[0]
			live = append([]Candle{{T: l.T, O: h.O, H: max(h.H, l.H), L: min(h.L, l.L), C: l.C, V: max(h.V, l.V)}}, live[1:]...)
		}
	}
	return trim(append(out, live...))
}

func trim(b []Candle) []Candle {
	if len(b) > Keep {
		return append([]Candle(nil), b[len(b)-Keep:]...)
	}
	return b
}
