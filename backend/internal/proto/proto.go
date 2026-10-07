// Package proto defines the binary wire format pushed to browsers.
// Layout mirrors Kite Ticker: big-endian, [u16 count][f64 serverTs] then
// repeated [u16 len][packet]. Packet length identifies the mode.
package proto

import (
	"encoding/binary"
	"math"
)

const (
	ModeLTP  uint8 = 1
	ModeFull uint8 = 2

	LTPLen    = 12 // token u32 + ltp f64
	FullLen   = 68 // token + 8 × f64
	HeaderLen = 10 // count u16 + serverTs f64
)

// Heartbeat is a 1-byte frame sent on idle connections.
var Heartbeat = []byte{0}

// Quote is the full state of one instrument.
type Quote struct {
	LTP, LastQty, Open, High, Low, Close, Volume, TS float64
}

// EncodeFull returns a FULL packet. The first LTPLen bytes form a valid LTP
// packet, so callers slice instead of encoding twice.
func EncodeFull(token uint32, q *Quote) []byte {
	b := make([]byte, FullLen)
	PutFull(b, token, q)
	return b
}

// PutFull encodes a FULL packet into b (len(b) >= FullLen).
func PutFull(b []byte, token uint32, q *Quote) {
	_ = b[FullLen-1] // bounds check hint
	binary.BigEndian.PutUint32(b[0:], token)
	putF64(b[4:], q.LTP)
	putF64(b[12:], q.LastQty)
	putF64(b[20:], q.Open)
	putF64(b[28:], q.High)
	putF64(b[36:], q.Low)
	putF64(b[44:], q.Close)
	putF64(b[52:], q.Volume)
	putF64(b[60:], q.TS)
}

// PacketFor returns the packet bytes for the given mode (zero-copy).
func PacketFor(full []byte, mode uint8) []byte {
	if mode == ModeLTP {
		return full[:LTPLen]
	}
	return full
}

// BeginFrame resets buf and reserves the header. Returns the buffer to append to.
func BeginFrame(buf []byte) []byte {
	return append(buf[:0], make([]byte, HeaderLen)...)
}

// AppendPacket appends a length-prefixed packet.
func AppendPacket(buf, pkt []byte) []byte {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(pkt)))
	return append(buf, pkt...)
}

// FinishFrame writes count and server timestamp into the reserved header.
func FinishFrame(buf []byte, count int, serverMs float64) {
	binary.BigEndian.PutUint16(buf[0:], uint16(count))
	putF64(buf[2:], serverMs)
}

// DecodeFrame parses a data frame (used by tests and the load tester).
// fn receives each packet; returns false on malformed input.
func DecodeFrame(b []byte, fn func(token uint32, pkt []byte)) (serverMs float64, ok bool) {
	if len(b) < HeaderLen {
		return 0, false
	}
	n := int(binary.BigEndian.Uint16(b))
	serverMs = math.Float64frombits(binary.BigEndian.Uint64(b[2:]))
	off := HeaderLen
	for range n {
		if off+2 > len(b) {
			return serverMs, false
		}
		l := int(binary.BigEndian.Uint16(b[off:]))
		off += 2
		if l < 4 || off+l > len(b) {
			return serverMs, false
		}
		pkt := b[off : off+l]
		if fn != nil {
			fn(binary.BigEndian.Uint32(pkt), pkt)
		}
		off += l
	}
	return serverMs, off == len(b)
}

// F64At reads a big-endian float64 at offset.
func F64At(b []byte, off int) float64 {
	return math.Float64frombits(binary.BigEndian.Uint64(b[off:]))
}

func putF64(b []byte, v float64) {
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
}
