// Package feed contains upstream market-data adapters. Each adapter
// registers its instruments with the hub, then streams normalised ticks.
package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"ticker/internal/hub"
)

// Feed is an upstream source of ticks.
type Feed interface {
	Name() string
	// Init registers instruments and seeds initial quotes. Called before Run.
	Init(ctx context.Context, h *hub.Hub) error
	// Run streams until ctx is cancelled, reconnecting on failure.
	Run(ctx context.Context)
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// getJSON fetches url into v. redact hides secrets (e.g. API tokens) from errors.
func getJSON(ctx context.Context, url string, v any, redact string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return sanitize(err, redact)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return sanitize(err, redact)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func sanitize(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), secret, "***"))
}

// runWithBackoff calls connect until ctx ends, backing off (with jitter) after
// failures. A session that lasted > 30s resets the backoff.
func runWithBackoff(ctx context.Context, log *slog.Logger, connect func(context.Context) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		log.Warn("feed disconnected, reconnecting", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func nowMs() float64 { return float64(time.Now().UnixMilli()) }
