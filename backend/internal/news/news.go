// Package news fetches recent headlines per instrument from public RSS feeds.
//
// Routing: crypto and US tickers → Yahoo Finance RSS (symbol-exact), falling
// back to Google News search; NSE stocks and indices → Google News search
// (Yahoo returns nothing for .NS symbols). Results are cached per query for
// TTL, concurrent requests share one upstream fetch, and upstream
// concurrency is capped. Stale results are served if a refresh fails.
//
// Everything from upstream is untrusted: HTML is stripped, lengths are
// capped, and only http(s) links survive.
//
// Licensing: Google News / Yahoo RSS suit demos and personal use. A
// commercial product should use a licensed feed (exchange announcements,
// a paid news API) behind the same Provider shape.
package news

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Item is one sanitized headline.
type Item struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	Source    string    `json:"source"`
	URL       string    `json:"url"`
	Image     string    `json:"image,omitempty"` // same-origin proxy path, never a third-party URL
	Published time.Time `json:"published"`
}

// Result is what /api/news returns.
type Result struct {
	Items     []Item    `json:"items"`
	Provider  string    `json:"provider"`
	Query     string    `json:"query"`
	FetchedAt time.Time `json:"fetchedAt"`
	Stale     bool      `json:"stale,omitempty"` // refresh failed; serving the last good result
}

// Query describes what to fetch: tried in order until one returns items.
type Query struct {
	Key   string // cache key
	Label string // human-readable search
	Feeds []Feed
	// Terms: a headline is kept only if its title mentions one of these
	// (whole word, case-insensitive). Search engines match article bodies,
	// so "Eternal" alone also returns essays about eternity.
	Terms []string
}

type Feed struct {
	Provider string
	URL      string
}

const (
	TTL         = 3 * time.Minute
	maxItems    = 20
	maxAge      = 14 * 24 * time.Hour
	maxEntries  = 500 // cache bound
	maxUpstream = 4   // concurrent upstream fetches
)

type entry struct {
	res         *Result
	expires     time.Time
	failedUntil time.Time     // negative cache: don't hammer a failing upstream
	flight      chan struct{} // non-nil while a fetch is running
}

type Service struct {
	mu     sync.Mutex
	cache  map[string]*entry
	sem    chan struct{}
	client *http.Client
	now    func() time.Time
}

func New() *Service {
	return &Service{
		cache:  map[string]*entry{},
		sem:    make(chan struct{}, maxUpstream),
		client: &http.Client{Timeout: 8 * time.Second},
		now:    time.Now,
	}
}

// ErrUnavailable means no provider answered and nothing is cached.
var ErrUnavailable = errors.New("news unavailable")

// Get returns cached headlines, refreshing when expired.
func (s *Service) Get(ctx context.Context, q Query) (*Result, error) {
	for {
		s.mu.Lock()
		e := s.cache[q.Key]
		if e != nil && e.res != nil && s.now().Before(e.expires) {
			res := e.res
			s.mu.Unlock()
			return res, nil
		}
		if e != nil && e.res == nil && e.flight == nil && s.now().Before(e.failedUntil) {
			s.mu.Unlock()
			return nil, ErrUnavailable
		}
		if e != nil && e.flight != nil { // someone is fetching: wait, then re-check
			ch := e.flight
			s.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if e == nil {
			s.evictLocked()
			e = &entry{}
			s.cache[q.Key] = e
		}
		e.flight = make(chan struct{})
		s.mu.Unlock()

		res, err := s.fetch(q)

		s.mu.Lock()
		close(e.flight)
		e.flight = nil
		switch {
		case err == nil:
			e.res, e.expires = res, s.now().Add(TTL)
		case e.res != nil: // keep serving stale data; retry in a minute
			stale := *e.res
			stale.Stale = true
			e.res, e.expires = &stale, s.now().Add(time.Minute)
		default:
			e.failedUntil = s.now().Add(30 * time.Second)
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		res = e.res
		s.mu.Unlock()
		return res, nil
	}
}

// evictLocked drops the entry expiring soonest once the cache is full.
func (s *Service) evictLocked() {
	if len(s.cache) < maxEntries {
		return
	}
	var oldest string
	var at time.Time
	for k, e := range s.cache {
		if e.flight == nil && (oldest == "" || e.expires.Before(at)) {
			oldest, at = k, e.expires
		}
	}
	delete(s.cache, oldest)
}

// fetch queries every feed concurrently and merges the results: Bing brings
// thumbnails, Google/Yahoo bring breadth. Duplicates (same headline from
// several feeds) collapse, preferring the copy that has an image. Detached
// from the request context so a client navigating away doesn't cancel a
// fetch others await.
func (s *Service) fetch(q Query) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	type out struct {
		items []Item
		err   error
	}
	results := make([]out, len(q.Feeds))
	var wg sync.WaitGroup
	for i, f := range q.Feeds {
		wg.Go(func() {
			items, err := s.fetchFeed(ctx, f)
			results[i] = out{relevant(items, q.Terms), err}
		})
	}
	wg.Wait()

	var errs []error
	var providers []string
	byKey := map[string]int{}
	merged := make([]Item, 0, maxItems*2)
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", q.Feeds[i].Provider, r.err))
			continue
		}
		if len(r.items) > 0 && !slices.Contains(providers, q.Feeds[i].Provider) {
			providers = append(providers, q.Feeds[i].Provider)
		}
		for _, it := range r.items {
			k := alnum(it.Title)
			if j, dup := byKey[k]; dup {
				if merged[j].Image == "" && it.Image != "" {
					merged[j] = it // keep the richer copy
				}
				continue
			}
			byKey[k] = len(merged)
			merged = append(merged, it)
		}
	}
	if len(errs) == len(q.Feeds) {
		return nil, errors.Join(errs...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Published.After(merged[j].Published) })
	if len(merged) > maxItems {
		merged = merged[:maxItems]
	}
	label := strings.Join(providers, " + ")
	if label == "" {
		label = q.Feeds[0].Provider
	}
	return &Result{Items: merged, Provider: label, Query: q.Label, FetchedAt: s.now().UTC()}, nil
}

func (s *Service) fetchFeed(ctx context.Context, f Feed) ([]Item, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ticker-demo/1.0)")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return ParseRSS(io.LimitReader(resp.Body, 2<<20), s.now())
}

// relevant keeps items whose title mentions a term (all items if no terms).
func relevant(items []Item, terms []string) []Item {
	if len(terms) == 0 {
		return items
	}
	alts := make([]string, len(terms))
	for i, t := range terms {
		alts[i] = regexp.QuoteMeta(t)
	}
	//  fails next to symbols like "&", so use explicit non-word boundaries.
	re := regexp.MustCompile(`(?i)(^|[^\pL\pN])(` + strings.Join(alts, "|") + `)([^\pL\pN]|$)`)
	out := items[:0:0]
	for _, it := range items {
		if re.MatchString(it.Title) {
			out = append(out, it)
		}
	}
	return out
}

// ---------- parsing & sanitizing ----------

type rss struct {
	Items []struct {
		Title       string   `xml:"title"`
		Link        string   `xml:"link"`
		Description string   `xml:"description"`
		PubDate     string   `xml:"pubDate"`
		Source      struct { // Google News: <source url="…">Publisher</source>
			Name string `xml:",chardata"`
			URL  string `xml:"url,attr"`
		} `xml:"source"`
		BingSource string `xml:"Source"` // Bing: <News:Source>
		BingImage  string `xml:"Image"`  // Bing: <News:Image>
	} `xml:"channel>item"`
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// clean strips markup and control characters and caps the length.
func clean(s string, max int) string {
	s = html.UnescapeString(tagRe.ReplaceAllString(s, " "))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

func safeURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

// alnum lowercases s and keeps only letters and digits.
func alnum(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// ParseRSS turns an RSS 2.0 document into sanitized, de-duplicated items,
// newest first.
func ParseRSS(r io.Reader, now time.Time) ([]Item, error) {
	var doc rss
	dec := xml.NewDecoder(r)
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse rss: %w", err)
	}
	seen := map[string]bool{}
	out := make([]Item, 0, len(doc.Items))
	for _, it := range doc.Items {
		link, ok := safeURL(unwrapBing(it.Link))
		if !ok {
			continue
		}
		title := clean(it.Title, 240)
		source := clean(it.Source.Name, 60)
		if source == "" {
			source = strings.TrimSuffix(clean(it.BingSource, 60), " on MSN")
		}
		// Google News titles end with " - Publisher"; split it out.
		if source != "" {
			// "Headline - BusinessToday" vs source "Business Today": compare loosely.
			if i := strings.LastIndex(title, " - "); i > 0 && alnum(title[i+3:]) == alnum(source) {
				title = title[:i]
			}
		} else if u, err := url.Parse(link); err == nil {
			source = strings.TrimPrefix(u.Hostname(), "www.")
		}
		if title == "" {
			continue
		}
		pub, err := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate))
		if err != nil {
			pub, err = time.Parse(time.RFC1123, strings.TrimSpace(it.PubDate))
		}
		if err != nil || pub.After(now.Add(time.Hour)) || now.Sub(pub) > maxAge {
			continue // undated, from the future, or too old
		}
		norm := strings.ToLower(title)
		if seen[norm] {
			continue // same story syndicated twice
		}
		seen[norm] = true
		h := sha1.Sum([]byte(norm))
		out = append(out, Item{
			ID: hex.EncodeToString(h[:8]), Title: title, Summary: clean(it.Description, 220),
			Source: source, URL: link, Image: ProxyPath(it.BingImage), Published: pub.UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	if len(out) > maxItems {
		out = out[:maxItems]
	}
	return out, nil
}
