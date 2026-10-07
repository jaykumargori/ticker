// Package fx keeps USD-based currency rates fresh for display conversion.
//
// Rates are fetched server-side (one upstream call shared by all users, no
// browser CORS issues), refreshed hourly, and the last good snapshot is kept
// if every provider fails. These are daily reference rates, good for display,
// not for settlement.
package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Supported display currencies (served only if the provider returns them).
var Supported = []string{"INR", "USD", "EUR", "GBP", "JPY", "AED", "SGD", "AUD", "CAD", "CHF", "CNY", "HKD"}

// Snapshot is what /api/fx serves. Rates[X] = units of X per 1 USD.
type Snapshot struct {
	Base      string             `json:"base"`
	Rates     map[string]float64 `json:"rates"`
	Source    string             `json:"source"`
	SourceURL string             `json:"sourceUrl"`
	AsOf      string             `json:"asOf"`      // provider's publication date
	FetchedAt time.Time          `json:"fetchedAt"` // when this server last refreshed
}

type provider struct {
	name, url, home string
	parse           func([]byte) (map[string]float64, string, error)
}

var providers = []provider{
	{
		// ECB reference rates via Frankfurter: free, no key, no attribution required.
		name: "European Central Bank (via Frankfurter)", url: "https://api.frankfurter.dev/v1/latest?base=USD", home: "https://frankfurter.dev",
		parse: func(b []byte) (map[string]float64, string, error) {
			var r struct {
				Date  string             `json:"date"`
				Rates map[string]float64 `json:"rates"`
			}
			if err := json.Unmarshal(b, &r); err != nil {
				return nil, "", err
			}
			return r.Rates, r.Date, nil
		},
	},
	{
		// Fallback with wider coverage (e.g. AED). Its terms require this attribution.
		name: "Rates By Exchange Rate API", url: "https://open.er-api.com/v6/latest/USD", home: "https://www.exchangerate-api.com",
		parse: func(b []byte) (map[string]float64, string, error) {
			var r struct {
				Result  string             `json:"result"`
				Updated string             `json:"time_last_update_utc"`
				Rates   map[string]float64 `json:"rates"`
			}
			if err := json.Unmarshal(b, &r); err != nil {
				return nil, "", err
			}
			if r.Result != "success" {
				return nil, "", fmt.Errorf("provider result %q", r.Result)
			}
			return r.Rates, r.Updated, nil
		},
	},
}

type Service struct {
	mu     sync.RWMutex
	snap   *Snapshot
	client *http.Client
}

func New() *Service { return &Service{client: &http.Client{Timeout: 8 * time.Second}} }

// Get returns the latest snapshot, or nil if no fetch has succeeded yet.
func (s *Service) Get() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Run refreshes hourly until ctx ends. Failures keep the previous snapshot
// and retry sooner (5 min).
func (s *Service) Run(ctx context.Context) {
	for {
		wait := time.Hour
		if err := s.Refresh(ctx); err != nil {
			slog.Warn("fx refresh failed; keeping last rates", "err", err)
			wait = 5 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Refresh tries providers in order and installs the first valid result.
func (s *Service) Refresh(ctx context.Context) error {
	var errs []error
	for _, p := range providers {
		snap, err := s.fetch(ctx, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
			continue
		}
		s.mu.Lock()
		s.snap = snap
		s.mu.Unlock()
		slog.Info("fx rates updated", "source", p.name, "asOf", snap.AsOf, "INR", snap.Rates["INR"])
		return nil
	}
	return errors.Join(errs...)
}

func (s *Service) fetch(ctx context.Context, p provider) (*Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	all, asOf, err := p.parse(body)
	if err != nil {
		return nil, err
	}
	return sanitize(all, p, asOf)
}

// sanitize keeps supported currencies with sane values; USD is always 1.
func sanitize(all map[string]float64, p provider, asOf string) (*Snapshot, error) {
	rates := map[string]float64{"USD": 1}
	for _, c := range Supported {
		if v, ok := all[c]; ok && v > 0 && v < 1e6 {
			rates[c] = v
		}
	}
	if _, ok := rates["INR"]; !ok {
		return nil, errors.New("response missing INR")
	}
	return &Snapshot{Base: "USD", Rates: rates, Source: p.name, SourceURL: p.home, AsOf: asOf, FetchedAt: time.Now().UTC()}, nil
}
