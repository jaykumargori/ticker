package candles

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTradeAggregates(t *testing.T) {
	s := NewStore()
	base := float64(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli())
	s.Trade(1, 100, 1, base)
	s.Trade(1, 105, 2, base+10_000)
	s.Trade(1, 98, 1, base+50_000)
	s.Trade(1, 101, 1, base+70_000) // next 1m bucket
	s.Trade(1, 99, 1, base+5_000)   // late trade for an old bucket: ignored

	bars, err := s.Get(context.Background(), 1, IntervalIndex("1m"), 10)
	if err != nil || len(bars) != 2 {
		t.Fatalf("1m bars = %+v err=%v", bars, err)
	}
	if b := bars[0]; b.O != 100 || b.H != 105 || b.L != 98 || b.C != 98 || b.V != 4 {
		t.Fatalf("first bar = %+v", b)
	}
	five, _ := s.Get(context.Background(), 1, IntervalIndex("5m"), 10)
	// The "late" trade is still inside the current 5m bucket, so it counts there.
	if len(five) != 1 || five[0].H != 105 || five[0].L != 98 || five[0].V != 6 {
		t.Fatalf("5m bars = %+v", five)
	}
	if _, err := s.Get(context.Background(), 99, 0, 10); err != ErrUnknown {
		t.Fatalf("unknown token err = %v", err)
	}
}

func TestBackfillMergesOnceUnderConcurrency(t *testing.T) {
	s := NewStore()
	now := time.Now().Unix() / 60 * 60
	s.Trade(1, 50, 1, float64(now*1000)) // live bar for the current minute

	var calls atomic.Int32
	s.SetLoader(1, func(ctx context.Context, interval string, limit int) ([]Candle, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // make concurrent callers overlap
		return []Candle{
			{T: now - 120, O: 40, H: 41, L: 39, C: 40, V: 10},
			{T: now - 60, O: 40, H: 45, L: 40, C: 44, V: 10},
			{T: now, O: 44, H: 60, L: 44, C: 49, V: 7}, // overlaps the live bar
		}, nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Get(context.Background(), 1, 0, 100); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if c := calls.Load(); c != 1 {
		t.Fatalf("loader called %d times, want 1 (single-flight)", c)
	}
	bars, _ := s.Get(context.Background(), 1, 0, 100)
	if len(bars) != 3 {
		t.Fatalf("merged bars = %+v", bars)
	}
	if last := bars[2]; last.O != 44 || last.H != 60 || last.L != 44 || last.C != 50 || last.V != 7 {
		t.Fatalf("overlap bar = %+v (want hist open/high, live close)", last)
	}
}
