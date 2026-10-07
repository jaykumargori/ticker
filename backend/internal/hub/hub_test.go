package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/proto"
)

func TestApplyAndBuildFrameConflates(t *testing.T) {
	h := New()
	a := h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: "A", Name: "A", Decimals: 2})
	b := h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: "B", Name: "B", Decimals: 2})
	if again := h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: "A", Name: "A", Decimals: 2}); again != a {
		t.Fatalf("Register not idempotent: %d vs %d", again, a)
	}

	for _, p := range []float64{10, 11, 12} {
		h.Apply(Tick{Token: a, Fields: FLTP | FAddQty, LTP: p, Qty: 1})
	}
	h.buildFrame(nil, nil)
	f := h.latest.Load()
	if f.Seq != 1 || len(f.Dirty) != 1 || f.Dirty[0] != a {
		t.Fatalf("frame seq=%d dirty=%v", f.Seq, f.Dirty)
	}
	full := f.entry(a).full[:]
	if ltp := proto.F64At(full, 4); ltp != 12 {
		t.Fatalf("conflated ltp = %v, want 12", ltp)
	}
	if hi, lo, vol := proto.F64At(full, 28), proto.F64At(full, 36), proto.F64At(full, 52); hi != 12 || lo != 10 || vol != 3 {
		t.Fatalf("hi=%v lo=%v vol=%v", hi, lo, vol)
	}
	if f.entry(b) != nil {
		t.Fatal("untouched instrument should have no entry")
	}

	// No changes → no new frame.
	h.buildFrame(nil, nil)
	if h.latest.Load().Seq != 1 {
		t.Fatal("frame published without changes")
	}

	// Invalid tokens are ignored, not panicking.
	h.Apply(Tick{Token: 999, Fields: FLTP, LTP: 1})
}

func TestServeEndToEnd(t *testing.T) {
	h := New()
	a := h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: "A", Name: "A", Decimals: 2})
	b := h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: "B", Name: "B", Decimals: 2})
	h.Apply(Tick{Token: a, Fields: FLTP, LTP: 100}, Tick{Token: b, Fields: FLTP, LTP: 200})
	h.buildFrame(nil, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = h.Serve(r.Context(), c, ClientOptions{WriteTimeout: time.Second, Heartbeat: time.Hour, MaxSubs: 10})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	send := func(s string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	type pkt struct {
		token uint32
		n     int
		ltp   float64
	}
	recv := func() []pkt {
		t.Helper()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("unexpected text frame: %s", data)
		}
		var out []pkt
		if _, ok := proto.DecodeFrame(data, func(tok uint32, p []byte) {
			out = append(out, pkt{tok, len(p), proto.F64At(p, 4)})
		}); !ok {
			t.Fatal("malformed frame")
		}
		return out
	}

	// Subscribe → immediate FULL snapshot.
	send(`{"a":"subscribe","v":[1]}`)
	if got := recv(); len(got) != 1 || got[0].token != a || got[0].n != proto.FullLen || got[0].ltp != 100 {
		t.Fatalf("snapshot = %+v", got)
	}

	// Switch to LTP mode → 12-byte packet.
	send(`{"a":"mode","v":["ltp",[1]]}`)
	if got := recv(); len(got) != 1 || got[0].n != proto.LTPLen {
		t.Fatalf("ltp snapshot = %+v", got)
	}

	// Several frames while the client is not reading are conflated or
	// delivered in order; the final value must be the latest.
	for _, p := range []float64{101, 102, 103} {
		h.Apply(Tick{Token: a, Fields: FLTP, LTP: p})
		h.Apply(Tick{Token: b, Fields: FLTP, LTP: p}) // b is not subscribed
		h.buildFrame(nil, nil)
	}
	var last float64
	for last != 103 {
		for _, p := range recv() {
			if p.token != a {
				t.Fatalf("received unsubscribed token %d", p.token)
			}
			last = p.ltp
		}
	}

	// Errors come back as text frames.
	send(`{"a":"nope"}`)
	typ, data, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageText || !strings.Contains(string(data), "unknown action") {
		t.Fatalf("error frame: %v %s %v", typ, data, err)
	}
}

func BenchmarkApply(b *testing.B) {
	h := New()
	for i := range 1000 {
		h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: string(rune('A'+i%26)) + string(rune(i)), Name: "", Decimals: 2})
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		h.Apply(Tick{Token: uint32(1 + i%1000), Fields: FLTP, LTP: float64(i)})
	}
}

func BenchmarkBuildFrame1000Dirty(b *testing.B) {
	h := New()
	for i := range 1000 {
		h.Register(Instrument{Exchange: "X", Segment: "EQ", Symbol: string(rune(0x100 + i)), Name: "", Decimals: 2})
	}
	var scratch []dirtyQuote
	var spare []uint32
	b.ReportAllocs()
	for b.Loop() {
		for t := uint32(1); t <= 1000; t++ {
			h.Apply(Tick{Token: t, Fields: FLTP, LTP: 1})
		}
		scratch, spare = h.buildFrame(scratch, spare)
	}
}
