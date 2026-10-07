package proto

import "testing"

func TestFrameRoundTrip(t *testing.T) {
	q := &Quote{LTP: 101.5, LastQty: 2, Open: 100, High: 102, Low: 99, Close: 98, Volume: 1e6, TS: 1700000000000}
	full := EncodeFull(42, q)
	if len(full) != FullLen {
		t.Fatalf("full len %d", len(full))
	}

	buf := BeginFrame(nil)
	buf = AppendPacket(buf, PacketFor(full, ModeFull))
	buf = AppendPacket(buf, PacketFor(full, ModeLTP))
	FinishFrame(buf, 2, 123)

	var got []int
	ts, ok := DecodeFrame(buf, func(token uint32, pkt []byte) {
		if token != 42 {
			t.Errorf("token %d", token)
		}
		if F64At(pkt, 4) != 101.5 {
			t.Errorf("ltp %v", F64At(pkt, 4))
		}
		got = append(got, len(pkt))
	})
	if !ok || ts != 123 {
		t.Fatalf("decode ok=%v ts=%v", ok, ts)
	}
	if len(got) != 2 || got[0] != FullLen || got[1] != LTPLen {
		t.Fatalf("packet lens %v", got)
	}
	// Field offsets are part of the public contract with the frontend.
	full2 := EncodeFull(1, q)
	for off, want := range map[int]float64{12: 2, 20: 100, 28: 102, 36: 99, 44: 98, 52: 1e6, 60: 1700000000000} {
		if v := F64At(full2, off); v != want {
			t.Errorf("offset %d = %v want %v", off, v, want)
		}
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	buf := BeginFrame(nil)
	buf = AppendPacket(buf, EncodeFull(1, &Quote{}))
	FinishFrame(buf, 1, 0)
	if _, ok := DecodeFrame(buf[:len(buf)-1], nil); ok {
		t.Fatal("expected failure on truncated frame")
	}
}
