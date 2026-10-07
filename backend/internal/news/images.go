package news

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Thumbnails are served through our own origin (/api/news/img):
//   - the viewer's IP and referrer never reach the image host;
//   - no mixed-content warnings (the feeds publish http:// URLs);
//   - SSRF-safe: only allowlisted image hosts and paths can be fetched,
//     redirects are refused, and only image content types under a size cap
//     are relayed;
//   - a bounded LRU keeps hot thumbnails in memory.

const (
	maxImageBytes = 1 << 20  // per image
	maxCacheBytes = 32 << 20 // whole cache
	imgPath       = "/api/news/img"
)

// allowedImage reports whether a URL is a Bing news thumbnail.
func allowedImage(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	okHost := host == "www.bing.com" || host == "bing.com" || strings.HasSuffix(host, ".mm.bing.net")
	return okHost && (u.Scheme == "http" || u.Scheme == "https") && u.Path == "/th" && u.Query().Get("id") != ""
}

// ProxyPath turns a feed image URL into our same-origin proxy path, or ""
// if it isn't allowlisted. It also asks Bing for a 2x thumbnail, smart-cropped.
func ProxyPath(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !allowedImage(u) {
		return ""
	}
	u.Scheme = "https"
	q := u.Query()
	q.Set("w", "240")
	q.Set("h", "180")
	q.Set("c", "7")  // smart crop
	q.Set("rs", "1") // resize
	u.RawQuery = q.Encode()
	return imgPath + "?u=" + url.QueryEscape(u.String())
}

// unwrapBing extracts the publisher URL from a Bing click-tracking link, so
// readers go straight to the article.
func unwrapBing(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), "bing.com") || !strings.HasSuffix(u.Path, "/apiclick.aspx") {
		return raw
	}
	if target := u.Query().Get("url"); target != "" {
		return target
	}
	return raw
}

type image struct {
	key   string
	ctype string
	body  []byte
}

// ImageProxy is an http.Handler for /api/news/img?u=<allowlisted url>.
type ImageProxy struct {
	mu     sync.Mutex
	lru    *list.List // front = most recent
	index  map[string]*list.Element
	bytes  int
	client *http.Client
	sem    chan struct{}
}

func NewImageProxy() *ImageProxy {
	return &ImageProxy{
		lru:   list.New(),
		index: map[string]*list.Element{},
		sem:   make(chan struct{}, 8),
		client: &http.Client{
			Timeout: 6 * time.Second,
			// Never follow redirects: a redirect could point at an internal address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *ImageProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(r.URL.Query().Get("u"))
	if err != nil || !allowedImage(u) {
		http.Error(w, "image not allowed", http.StatusBadRequest)
		return
	}
	key := u.String()
	img, ok := p.get(key)
	if !ok {
		img, err = p.fetch(r.Context(), key)
		if err != nil {
			http.Error(w, "image unavailable", http.StatusBadGateway)
			return
		}
		p.put(img)
	}
	w.Header().Set("Content-Type", img.ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(img.body)
}

func (p *ImageProxy) fetch(ctx context.Context, src string) (*image, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ticker-demo/1.0)")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	ctype := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	switch ctype {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
	default:
		return nil, fmt.Errorf("unexpected content type %q", ctype)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxImageBytes {
		return nil, fmt.Errorf("image too large")
	}
	return &image{key: src, ctype: ctype, body: body}, nil
}

func (p *ImageProxy) get(key string) (*image, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.index[key]; ok {
		p.lru.MoveToFront(el)
		return el.Value.(*image), true
	}
	return nil, false
}

func (p *ImageProxy) put(img *image) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.index[img.key]; ok {
		return
	}
	p.index[img.key] = p.lru.PushFront(img)
	p.bytes += len(img.body)
	for p.bytes > maxCacheBytes {
		el := p.lru.Back()
		old := el.Value.(*image)
		p.lru.Remove(el)
		delete(p.index, old.key)
		p.bytes -= len(old.body)
	}
}
