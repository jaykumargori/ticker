// Command loadtest opens many WebSocket clients against the server and
// reports throughput and server→client latency (valid on the same host).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"ticker/internal/proto"
)

func main() {
	base := flag.String("url", "http://localhost:8080", "server base URL")
	clients := flag.Int("clients", 1000, "concurrent connections")
	subs := flag.Int("subs", 50, "instruments per client")
	mode := flag.String("mode", "full", "ltp|full")
	dur := flag.Duration("duration", 30*time.Second, "test duration")
	ramp := flag.Duration("ramp", 5*time.Second, "time to open all connections")
	flag.Parse()

	tokens, err := fetchTokens(*base)
	if err != nil || len(tokens) == 0 {
		fmt.Fprintln(os.Stderr, "fetch instruments:", err)
		os.Exit(1)
	}
	wsURL := "ws" + (*base)[len("http"):] + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), *dur+*ramp)
	defer cancel()

	var connected, failed, retries, msgs, pkts, bytes atomic.Int64
	var latMu sync.Mutex
	lat := make([]float64, 0, 1<<16)

	var wg sync.WaitGroup
	gap := *ramp / time.Duration(*clients)
	for i := range *clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(gap * time.Duration(i))
			// Retry with jittered backoff like the real frontend: a connect storm
			// can overflow the kernel accept backlog (RST) before the app sees it.
			var conn *websocket.Conn
			var err error
			for attempt := range 6 {
				conn, _, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {*base}}})
				if err == nil || ctx.Err() != nil {
					break
				}
				retries.Add(1)
				time.Sleep(time.Duration(rand.Int64N(int64(200*time.Millisecond) << attempt)))
			}
			if err != nil {
				if failed.Add(1) <= 3 {
					fmt.Fprintln(os.Stderr, "dial error:", err)
				}
				return
			}
			defer conn.CloseNow()
			conn.SetReadLimit(1 << 20)
			connected.Add(1)
			pick := make([]uint32, 0, *subs)
			for _, j := range rand.Perm(len(tokens))[:min(*subs, len(tokens))] {
				pick = append(pick, tokens[j])
			}
			msg, _ := json.Marshal(map[string]any{"a": "subscribe", "v": pick})
			if conn.Write(ctx, websocket.MessageText, msg) != nil {
				return
			}
			if *mode == "ltp" {
				msg, _ = json.Marshal(map[string]any{"a": "mode", "v": []any{"ltp", pick}})
				_ = conn.Write(ctx, websocket.MessageText, msg)
			}
			sample := i%20 == 0 // sample latency on 5% of clients to bound memory
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				msgs.Add(1)
				bytes.Add(int64(len(data)))
				if len(data) <= 1 {
					continue
				}
				n := 0
				ts, _ := proto.DecodeFrame(data, func(uint32, []byte) { n++ })
				pkts.Add(int64(n))
				if sample {
					d := float64(time.Now().UnixMicro())/1000 - ts
					latMu.Lock()
					if len(lat) < cap(lat) {
						lat = append(lat, d)
					}
					latMu.Unlock()
				}
			}
		}()
	}

	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	var lm, lp, lb int64
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			report(lat, connected.Load(), failed.Load())
			fmt.Printf("dial retries: %d\n", retries.Load())
			return
		case <-tk.C:
			m, p, b := msgs.Load(), pkts.Load(), bytes.Load()
			fmt.Printf("conns=%d failed=%d msgs/s=%d packets/s=%d MB/s=%.2f\n",
				connected.Load(), failed.Load(), m-lm, p-lp, float64(b-lb)/1e6)
			lm, lp, lb = m, p, b
		}
	}
}

func report(lat []float64, ok, failed int64) {
	fmt.Printf("\nconnections ok=%d failed=%d\n", ok, failed)
	if len(lat) == 0 {
		return
	}
	slices.Sort(lat)
	q := func(p float64) float64 { return lat[int(p*float64(len(lat)-1))] }
	fmt.Printf("latency ms (server send → client decode, %d samples): p50=%.2f p95=%.2f p99=%.2f max=%.2f\n",
		len(lat), q(.5), q(.95), q(.99), lat[len(lat)-1])
}

func fetchTokens(base string) ([]uint32, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(base + "/api/instruments")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ins []struct {
		Token uint32 `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ins); err != nil {
		return nil, err
	}
	out := make([]uint32, len(ins))
	for i, in := range ins {
		out[i] = in.Token
	}
	return out, nil
}
