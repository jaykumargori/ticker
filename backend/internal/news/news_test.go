package news

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func rssDoc(items ...string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>` + strings.Join(items, "") + `</channel></rss>`
}

func item(title, link, date, source string) string {
	src := ""
	if source != "" {
		src = fmt.Sprintf(`<source url="https://pub.example">%s</source>`, source)
	}
	return fmt.Sprintf(`<item><title>%s</title><link>%s</link><pubDate>%s</pubDate>%s</item>`, title, link, date, src)
}

func TestParseRSSSanitizes(t *testing.T) {
	doc := rssDoc(
		item("Reliance raises ₹13,000 crore - Business Standard", "https://news.example/a", "Wed, 07 Oct 2026 09:00:00 GMT", "Business Standard"),
		item("&lt;b&gt;Bold&lt;/b&gt; &amp; markup", "https://www.coindesk.com/x", "Wed, 07 Oct 2026 10:00:00 +0000", ""),
		item("XSS link", "javascript:alert(1)", "Wed, 07 Oct 2026 10:00:00 GMT", ""),
		item("Too old", "https://news.example/old", "Wed, 01 Jan 2025 10:00:00 GMT", ""),
		item("No date", "https://news.example/nodate", "", ""),
		item("Titan targets cut - BusinessToday", "https://news.example/t", "Wed, 07 Oct 2026 07:00:00 GMT", "Business Today"),
		item("reliance raises ₹13,000 crore", "https://dup.example/a", "Wed, 07 Oct 2026 08:00:00 GMT", ""), // duplicate story
	)
	items, err := ParseRSS(strings.NewReader(doc), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	if items[2].Title != "Titan targets cut" {
		t.Fatalf("loose publisher suffix not stripped: %q", items[2].Title)
	}
	if items[0].Title != "Bold & markup" || items[0].Source != "coindesk.com" {
		t.Fatalf("newest item = %+v (markup stripped, source from host)", items[0])
	}
	if items[1].Title != "Reliance raises ₹13,000 crore" || items[1].Source != "Business Standard" {
		t.Fatalf("google item = %+v (publisher suffix split out)", items[1])
	}
}

func TestServiceCachesFallsBackAndServesStale(t *testing.T) {
	var primary, secondary atomic.Int32
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/empty" {
			primary.Add(1)
			fmt.Fprint(w, rssDoc()) // primary has nothing → fall back
			return
		}
		secondary.Add(1)
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, rssDoc(item("Headline", "https://n.example/1", "Wed, 07 Oct 2026 11:00:00 GMT", "Pub")))
	}))
	defer srv.Close()

	s := New()
	clock := now
	s.now = func() time.Time { return clock }
	q := Query{Key: "k", Label: "x", Feeds: []Feed{{Provider: "A", URL: srv.URL + "/empty"}, {Provider: "B", URL: srv.URL + "/full"}}}

	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			// Feed A is empty, B has the item: merged result credits only B.
			if r, err := s.Get(context.Background(), q); err != nil || len(r.Items) != 1 || r.Provider != "B" {
				t.Errorf("get = %+v, %v", r, err)
			}
		})
	}
	wg.Wait()
	if primary.Load() != 1 || secondary.Load() != 1 {
		t.Fatalf("upstream calls primary=%d secondary=%d; want 1 each (shared fetch)", primary.Load(), secondary.Load())
	}

	// After TTL with upstream down: stale result instead of an error.
	clock = clock.Add(TTL + time.Second)
	fail.Store(true)
	r, err := s.Get(context.Background(), q)
	if err != nil || !r.Stale || len(r.Items) != 1 {
		t.Fatalf("stale get = %+v, %v", r, err)
	}
}

func TestQueryFor(t *testing.T) {
	has := func(q Query, sub string) bool {
		for _, f := range q.Feeds {
			if strings.Contains(f.URL, sub) {
				return true
			}
		}
		return false
	}
	if q := QueryFor("CRYPTO", "CRYPTO", "BTCUSDT", "BTC/USDT"); !has(q, "s=BTC-USD") || !has(q, "bing.com") || q.Label != "Bitcoin crypto" {
		t.Fatalf("crypto query = %+v", q)
	}
	if q := QueryFor("NSE-SIM", "EQ", "RELIANCE", "Reliance Industries"); !has(q, "bing.com") || !has(q, "gl=IN") {
		t.Fatalf("nse query = %+v", q)
	}
}

func TestRelevantFiltersByTitle(t *testing.T) {
	items := []Item{{Title: "Eternal shares jump 5% on Blinkit growth"}, {Title: "The eternal complement"}, {Title: "Do Tutankhamun and Nefertiti share a tomb?"}, {Title: "M&M rallies"}}
	got := relevant(items, []string{"Eternal Ltd", "Blinkit", "Zomato"})
	if len(got) != 1 || got[0].Title != items[0].Title {
		t.Fatalf("eternal filter = %+v", got)
	}
	if got := relevant(items, []string{"M&M"}); len(got) != 1 {
		t.Fatalf("symbol with & = %+v", got)
	}
	if got := relevant(items, nil); len(got) != len(items) {
		t.Fatal("no terms should keep all")
	}
}

func TestBingParsingAndImageAllowlist(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0" xmlns:News="https://www.bing.com/news/search?q=x"><channel><title>t</title>
<item><title>Titan shares fall 4%</title>
<link>http://www.bing.com/news/apiclick.aspx?ref=FexRss&amp;url=https%3a%2f%2fwww.msn.com%2fen-in%2fmoney%2ftitan&amp;mkt=en-in</link>
<description>Titan Company reported &lt;b&gt;25%&lt;/b&gt; growth</description>
<pubDate>Wed, 07 Oct 2026 09:00:00 GMT</pubDate>
<News:Source>The Economic Times on MSN</News:Source>
<News:Image>http://www.bing.com/th?id=ONUT.abc&amp;pid=News</News:Image></item></channel></rss>`
	items, err := ParseRSS(strings.NewReader(doc), now)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	it := items[0]
	if it.URL != "https://www.msn.com/en-in/money/titan" {
		t.Errorf("bing click-tracking link not unwrapped: %s", it.URL)
	}
	if it.Source != "The Economic Times" || it.Summary != "Titan Company reported 25% growth" {
		t.Errorf("source/summary = %q / %q", it.Source, it.Summary)
	}
	if !strings.HasPrefix(it.Image, "/api/news/img?u=https%3A%2F%2Fwww.bing.com%2Fth") {
		t.Errorf("image not proxied over https: %s", it.Image)
	}
	for _, bad := range []string{"http://169.254.169.254/latest/meta-data", "https://evil.example/th?id=x", "https://www.bing.com/search?q=x", "file:///etc/passwd", "https://bing.com.evil.example/th?id=1"} {
		if ProxyPath(bad) != "" {
			t.Errorf("ProxyPath allowed %s", bad)
		}
	}
}
