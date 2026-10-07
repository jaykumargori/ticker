package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"ticker/internal/proto"
)

// ClientOptions bounds per-connection resources.
type ClientOptions struct {
	WriteTimeout time.Duration // slow consumers are disconnected after this
	Heartbeat    time.Duration // idle interval before a 1-byte heartbeat
	MaxSubs      int
	// MaxMsgsPerSec caps client→server control messages (subscribe spam forces
	// snapshot writes). 0 disables the limit.
	MaxMsgsPerSec int
}

type opKind uint8

const (
	opSub opKind = iota
	opUnsub
	opMode
	opError
)

type command struct {
	op     opKind
	mode   uint8
	tokens []uint32
	err    string
}

// ErrSlowClient is returned when a write exceeds WriteTimeout.
var ErrSlowClient = errors.New("slow client: write timeout")

// ErrRateLimited is returned when a client sends control messages too fast.
var ErrRateLimited = errors.New("client exceeded message rate")

// Serve runs one WebSocket connection until it closes. A reader goroutine
// parses control messages; this goroutine is the only writer.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn, opt ClientOptions) error {
	h.clients.Add(1)
	defer h.clients.Add(-1)

	// The reader cancels with a cause (e.g. ErrRateLimited) so the caller
	// can pick the right close code.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	conn.SetReadLimit(64 << 10) // ~3000 tokens per subscribe; anything larger is abuse
	cmds := make(chan command, 16)
	go h.readLoop(ctx, cancel, conn, cmds, opt.MaxMsgsPerSec)

	w := writer{h: h, conn: conn, opt: opt, subs: make(map[uint32]uint8)}
	frame := h.latest.Load()
	w.lastSeq = frame.Seq
	hb := time.NewTicker(opt.Heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case c := <-cmds:
			if err := w.handle(ctx, c); err != nil {
				return err
			}
		case <-frame.next:
			frame = h.latest.Load()
			if err := w.sendFrame(ctx, frame); err != nil {
				return err
			}
		case <-hb.C:
			if time.Since(w.lastWrite) >= opt.Heartbeat {
				if err := w.write(ctx, websocket.MessageBinary, proto.Heartbeat); err != nil {
					return err
				}
			}
		}
	}
}

type writer struct {
	h         *Hub
	conn      *websocket.Conn
	opt       ClientOptions
	subs      map[uint32]uint8 // token → mode
	lastSeq   uint64           // last frame seq delivered
	lastWrite time.Time
	buf       []byte // reused across writes
}

// sendFrame delivers every subscribed quote that changed after lastSeq.
// If frames were skipped (client was busy), seq comparison conflates them.
func (w *writer) sendFrame(ctx context.Context, f *Frame) error {
	since := w.lastSeq
	w.lastSeq = f.Seq
	if len(w.subs) == 0 {
		return nil
	}
	w.buf = proto.BeginFrame(w.buf)
	count := 0
	if f.Seq == since+1 && len(f.Dirty) < len(w.subs) {
		// Consecutive frame with few changes: walk the dirty list.
		for _, t := range f.Dirty {
			if mode, ok := w.subs[t]; ok {
				w.buf = proto.AppendPacket(w.buf, proto.PacketFor(f.entries[t].full[:], mode))
				count++
			}
		}
	} else {
		for t, mode := range w.subs {
			if e := f.entry(t); e != nil && e.seq > since {
				w.buf = proto.AppendPacket(w.buf, proto.PacketFor(e.full[:], mode))
				count++
			}
		}
	}
	return w.flush(ctx, count)
}

// sendSnapshot pushes current state for tokens immediately (on subscribe/mode).
func (w *writer) sendSnapshot(ctx context.Context, tokens []uint32) error {
	f := w.h.latest.Load()
	w.buf = proto.BeginFrame(w.buf)
	count := 0
	for _, t := range tokens {
		if e := f.entry(t); e != nil {
			w.buf = proto.AppendPacket(w.buf, proto.PacketFor(e.full[:], w.subs[t]))
			count++
		}
	}
	return w.flush(ctx, count)
}

func (w *writer) flush(ctx context.Context, count int) error {
	if count == 0 {
		return nil
	}
	proto.FinishFrame(w.buf, count, float64(time.Now().UnixMicro())/1000)
	return w.write(ctx, websocket.MessageBinary, w.buf)
}

func (w *writer) write(ctx context.Context, typ websocket.MessageType, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, w.opt.WriteTimeout)
	defer cancel()
	if err := w.conn.Write(wctx, typ, b); err != nil {
		if errors.Is(wctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			w.h.dropped.Add(1)
			return ErrSlowClient
		}
		return err
	}
	w.lastWrite = time.Now()
	w.h.msgsOut.Add(1)
	w.h.bytesOut.Add(uint64(len(b)))
	return nil
}

func (w *writer) sendError(ctx context.Context, msg string) error {
	b, _ := json.Marshal(map[string]string{"type": "error", "data": msg})
	return w.write(ctx, websocket.MessageText, b)
}

func (w *writer) handle(ctx context.Context, c command) error {
	switch c.op {
	case opError:
		return w.sendError(ctx, c.err)
	case opUnsub:
		for _, t := range c.tokens {
			delete(w.subs, t)
		}
		return nil
	case opSub, opMode:
		mode := c.mode
		if c.op == opSub {
			mode = proto.ModeFull
		}
		fresh := make([]uint32, 0, len(c.tokens))
		for _, t := range c.tokens {
			cur, exists := w.subs[t]
			if !exists && len(w.subs) >= w.opt.MaxSubs {
				if err := w.sendError(ctx, fmt.Sprintf("subscription limit %d reached", w.opt.MaxSubs)); err != nil {
					return err
				}
				break
			}
			if c.op == opMode && !exists {
				continue // mode applies only to existing subscriptions (Kite semantics)
			}
			if !exists || cur != mode {
				w.subs[t] = mode
				fresh = append(fresh, t)
			}
		}
		return w.sendSnapshot(ctx, fresh)
	}
	return nil
}

func (h *Hub) readLoop(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn, out chan<- command, maxPerSec int) {
	defer func() {
		// This goroutine sits outside net/http's per-request recovery; a panic
		// here would otherwise crash the whole process.
		if p := recover(); p != nil {
			cancel(fmt.Errorf("reader panic: %v", p))
			return
		}
		cancel(nil)
	}()
	var lim *rate.Limiter
	if maxPerSec > 0 {
		lim = rate.NewLimiter(rate.Limit(maxPerSec), maxPerSec*2)
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if lim != nil && !lim.Allow() {
			cancel(ErrRateLimited)
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		c := h.parseCommand(data)
		select {
		case out <- c:
		case <-ctx.Done():
			return
		}
	}
}

type rawCmd struct {
	A string          `json:"a"`
	V json.RawMessage `json:"v"`
}

func (h *Hub) parseCommand(data []byte) command {
	var m rawCmd
	if err := json.Unmarshal(data, &m); err != nil {
		return command{op: opError, err: "invalid json"}
	}
	var c command
	var raw json.RawMessage
	switch m.A {
	case "subscribe":
		c.op, raw = opSub, m.V
	case "unsubscribe":
		c.op, raw = opUnsub, m.V
	case "mode":
		var parts []json.RawMessage
		if err := json.Unmarshal(m.V, &parts); err != nil || len(parts) != 2 {
			return command{op: opError, err: `mode expects ["ltp"|"full",[tokens]]`}
		}
		var mode string
		_ = json.Unmarshal(parts[0], &mode)
		switch mode {
		case "ltp":
			c.mode = proto.ModeLTP
		case "full", "quote":
			c.mode = proto.ModeFull
		default:
			return command{op: opError, err: "unknown mode " + mode}
		}
		c.op, raw = opMode, parts[1]
	default:
		return command{op: opError, err: "unknown action " + m.A}
	}
	var tokens []uint32
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return command{op: opError, err: "tokens must be an array of uint32"}
	}
	c.tokens = tokens[:0]
	for _, t := range tokens {
		if h.validToken(t) {
			c.tokens = append(c.tokens, t)
		}
	}
	return c
}
