// Package hub owns the quote table and the conflated fan-out to clients.
//
// Data flow: feeds call Apply → quotes mutate under mu and are marked dirty →
// every flush interval the builder encodes each dirty quote once, publishes an
// immutable Frame (copy-on-write snapshot) and closes the previous frame's
// `next` channel, waking every client writer in O(1).
package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"ticker/internal/proto"
)

// Tick field flags: which fields of a Tick are meaningful.
const (
	FLTP    uint8 = 1 << iota // trade: LTP, Qty
	FOHLC                     // Open/High/Low/Close are authoritative
	FVolume                   // Volume is absolute
	FAddQty                   // add Qty to running volume (feeds without cumulative volume)
)

// Tick is the normalised update produced by feed adapters.
type Tick struct {
	Token                                        uint32
	Fields                                       uint8
	LTP, Qty, Open, High, Low, Close, Volume, TS float64
}

// Instrument metadata exposed via /api/instruments.
type Instrument struct {
	Token    uint32 `json:"token"`
	Exchange string `json:"exchange"`
	Segment  string `json:"segment"` // "EQ", "INDEX", "CRYPTO"
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Decimals int    `json:"decimals"`
	// Currency prices are quoted in (ISO 4217). USD-pegged stablecoins
	// (USDT, USDC, FDUSD) are reported as USD for conversion purposes.
	Currency string `json:"currency"`
	// Tags carry index membership ("NIFTY 50", "NIFTY BANK", …) for heatmaps.
	Tags []string `json:"tags,omitempty"`
}

// QuoteSummary is the compact shape served by /api/quotes (movers, heatmap).
type QuoteSummary struct {
	Token  uint32  `json:"k"`
	LTP    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
}

type entry struct {
	seq  uint64              // frame seq in which this quote last changed
	full [proto.FullLen]byte // encoded FULL packet; LTP packet is a prefix
}

// Frame is an immutable snapshot. Never mutate after publish.
type Frame struct {
	Seq     uint64
	Dirty   []uint32 // tokens changed in this frame
	entries []*entry // indexed by token; nil = no data yet
	next    chan struct{}
}

func (f *Frame) entry(token uint32) *entry {
	if int(token) >= len(f.entries) {
		return nil
	}
	return f.entries[token]
}

// Stats are cumulative counters plus per-second rates.
type Stats struct {
	Clients      int64   `json:"clients"`
	Instruments  int     `json:"instruments"`
	FrameSeq     uint64  `json:"frameSeq"`
	TicksPerSec  float64 `json:"ticksPerSec"`
	FramesPerSec float64 `json:"framesPerSec"`
	MsgsPerSec   float64 `json:"msgsPerSec"`
	BytesPerSec  float64 `json:"bytesPerSec"`
	Dropped      int64   `json:"slowClientsDropped"`
}

type Hub struct {
	mu          sync.Mutex
	instruments []Instrument // index == token; [0] is a reserved invalid slot
	byKey       map[string]uint32
	quotes      []proto.Quote
	isDirty     []bool
	dirty       []uint32

	latest atomic.Pointer[Frame]

	// catalog bumps whenever an instrument is registered (feeds may add
	// instruments at runtime); clients use it to refresh their list cheaply.
	catalog atomic.Uint64

	// onTrade sees every trade (e.g. candle aggregation). Set before feeds start.
	onTrade func(token uint32, price, qty, tsMs float64)

	ticksIn, frames, msgsOut, bytesOut atomic.Uint64
	clients, dropped                   atomic.Int64

	rateMu sync.RWMutex
	rates  Stats
}

func New() *Hub {
	h := &Hub{
		instruments: make([]Instrument, 1),
		byKey:       map[string]uint32{},
		quotes:      make([]proto.Quote, 1),
		isDirty:     make([]bool, 1),
	}
	h.latest.Store(&Frame{next: make(chan struct{})})
	return h
}

// Register adds an instrument (idempotent per exchange+symbol) and returns its
// token. Token and Tags in the argument are ignored.
func (h *Hub) Register(in Instrument) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := in.Exchange + ":" + in.Symbol
	if t, ok := h.byKey[key]; ok {
		return t
	}
	t := uint32(len(h.instruments))
	in.Token, in.Tags = t, nil
	h.instruments = append(h.instruments, in)
	h.quotes = append(h.quotes, proto.Quote{})
	h.isDirty = append(h.isDirty, false)
	h.byKey[key] = t
	h.catalog.Add(1)
	return t
}

// CatalogVersion changes whenever the instrument list grows.
func (h *Hub) CatalogVersion() uint64 { return h.catalog.Load() }

// OnTrade installs a trade observer. Call before feeds start.
func (h *Hub) OnTrade(fn func(token uint32, price, qty, tsMs float64)) { h.onTrade = fn }

// Tag adds a tag (e.g. index membership) to an instrument.
func (h *Hub) Tag(token uint32, tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if token > 0 && int(token) < len(h.instruments) {
		h.instruments[token].Tags = append(h.instruments[token].Tags, tag)
	}
}

// Quotes returns a summary of every instrument that has a price.
func (h *Hub) Quotes() []QuoteSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]QuoteSummary, 0, len(h.quotes))
	for t := 1; t < len(h.quotes); t++ {
		if q := &h.quotes[t]; q.LTP > 0 {
			out = append(out, QuoteSummary{uint32(t), q.LTP, q.Close, q.Volume})
		}
	}
	return out
}

// Quote returns the current state of one instrument.
func (h *Hub) Quote(token uint32) (proto.Quote, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if token == 0 || int(token) >= len(h.quotes) {
		return proto.Quote{}, false
	}
	return h.quotes[token], true
}

// Instrument looks up one instrument by token.
func (h *Hub) Instrument(token uint32) (Instrument, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if token == 0 || int(token) >= len(h.instruments) {
		return Instrument{}, false
	}
	return h.instruments[token], true
}

// Instruments returns a copy of all registered instruments.
func (h *Hub) Instruments() []Instrument {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Instrument, len(h.instruments)-1)
	copy(out, h.instruments[1:])
	return out
}

func (h *Hub) validToken(t uint32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return t > 0 && int(t) < len(h.instruments)
}

// Apply merges ticks into the quote table. Safe for concurrent feeds.
func (h *Hub) Apply(ticks ...Tick) {
	h.mu.Lock()
	for i := range ticks {
		t := &ticks[i]
		if t.Token == 0 || int(t.Token) >= len(h.quotes) {
			continue
		}
		q := &h.quotes[t.Token]
		applyTick(q, t)
		if !h.isDirty[t.Token] {
			h.isDirty[t.Token] = true
			h.dirty = append(h.dirty, t.Token)
		}
	}
	h.mu.Unlock()
	h.ticksIn.Add(uint64(len(ticks)))
	if h.onTrade != nil { // outside the hub lock: observers never block feeds' quote updates
		for i := range ticks {
			if t := &ticks[i]; t.Fields&FLTP != 0 && t.LTP > 0 {
				h.onTrade(t.Token, t.LTP, t.Qty, t.TS)
			}
		}
	}
}

func applyTick(q *proto.Quote, t *Tick) {
	if t.Fields&FOHLC != 0 {
		q.Open, q.High, q.Low, q.Close = t.Open, t.High, t.Low, t.Close
	}
	if t.Fields&FVolume != 0 {
		q.Volume = t.Volume
	}
	if t.Fields&FLTP != 0 && t.LTP > 0 {
		q.LTP, q.LastQty = t.LTP, t.Qty
		if t.Fields&FAddQty != 0 {
			q.Volume += t.Qty
		}
		if q.High == 0 || t.LTP > q.High {
			q.High = t.LTP
		}
		if q.Low == 0 || t.LTP < q.Low {
			q.Low = t.LTP
		}
		if q.Open == 0 {
			q.Open = t.LTP
		}
		if q.Close == 0 { // no reference yet: avoid div-by-zero change% on the client
			q.Close = t.LTP
		}
	}
	if q.LTP == 0 && q.Close > 0 { // stats arrived before the first trade
		q.LTP = q.Close
	}
	if t.TS > 0 {
		q.TS = t.TS
	}
}

type dirtyQuote struct {
	token uint32
	q     proto.Quote
}

// Run builds frames every interval until ctx is cancelled.
func (h *Hub) Run(ctx context.Context, interval time.Duration) {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	rt := time.NewTicker(time.Second)
	defer rt.Stop()
	var scratch []dirtyQuote
	var spare []uint32
	var last [4]uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			scratch, spare = h.buildFrame(scratch, spare)
		case <-rt.C:
			cur := [4]uint64{h.ticksIn.Load(), h.frames.Load(), h.msgsOut.Load(), h.bytesOut.Load()}
			h.rateMu.Lock()
			h.rates.TicksPerSec = float64(cur[0] - last[0])
			h.rates.FramesPerSec = float64(cur[1] - last[1])
			h.rates.MsgsPerSec = float64(cur[2] - last[2])
			h.rates.BytesPerSec = float64(cur[3] - last[3])
			h.rateMu.Unlock()
			last = cur
		}
	}
}

// buildFrame publishes a new frame if anything changed. scratch/spare are
// reusable buffers owned by the single builder goroutine.
func (h *Hub) buildFrame(scratch []dirtyQuote, spare []uint32) ([]dirtyQuote, []uint32) {
	h.mu.Lock()
	if len(h.dirty) == 0 {
		h.mu.Unlock()
		return scratch, spare
	}
	dirty := h.dirty
	h.dirty = spare[:0]
	scratch = scratch[:0]
	for _, t := range dirty {
		h.isDirty[t] = false
		scratch = append(scratch, dirtyQuote{t, h.quotes[t]})
	}
	n := len(h.quotes)
	h.mu.Unlock()

	// Encode outside the lock: feeds are never blocked by encoding.
	prev := h.latest.Load()
	seq := prev.Seq + 1
	entries := make([]*entry, n)
	copy(entries, prev.entries)
	// One allocation per changed quote (packet stored inline). A shared slab
	// would be fewer allocs but lets one illiquid instrument pin a whole old
	// frame's slab in memory.
	changed := make([]uint32, len(scratch)) // owned by the frame (immutable)
	for i := range scratch {
		d := &scratch[i]
		e := &entry{seq: seq}
		proto.PutFull(e.full[:], d.token, &d.q)
		entries[d.token] = e
		changed[i] = d.token
	}
	f := &Frame{Seq: seq, Dirty: changed, entries: entries, next: make(chan struct{})}
	h.latest.Store(f)
	close(prev.next) // wakes all writers waiting on prev
	h.frames.Add(1)
	return scratch, dirty
}

// Snapshot returns the current stats.
func (h *Hub) Snapshot() Stats {
	h.rateMu.RLock()
	s := h.rates
	h.rateMu.RUnlock()
	s.Clients = h.clients.Load()
	s.Dropped = h.dropped.Load()
	s.FrameSeq = h.latest.Load().Seq
	h.mu.Lock()
	s.Instruments = len(h.instruments) - 1
	h.mu.Unlock()
	return s
}

// Clients returns the current connection count.
func (h *Hub) Clients() int64 { return h.clients.Load() }
