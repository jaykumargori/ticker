// Package insights turns price history, the live quote and recent headlines
// into transparent, rule-based signals.
//
// Deliberately descriptive, not prescriptive: every signal reports a number
// and what it conventionally indicates. There is no buy/sell verdict. In India
// buy/sell recommendations to the public are regulated (SEBI Research Analyst /
// Investment Adviser rules), and these indicators don't predict returns well
// enough to justify one.
package insights

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"ticker/internal/candles"
	"ticker/internal/news"
	"ticker/internal/proto"
)

// Tone of a signal or headline.
const (
	Positive = "positive"
	Negative = "negative"
	Neutral  = "neutral"
	Unknown  = "unknown" // not enough data
)

type Signal struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  string `json:"value"`  // the number, formatted
	Detail string `json:"detail"` // what it conventionally indicates, in plain English
	Tone   string `json:"tone"`
}

type Headline struct {
	Title  string    `json:"title"`
	Source string    `json:"source"`
	URL    string    `json:"url"`
	Tone   string    `json:"tone"`
	Time   time.Time `json:"published"`
}

type Lean struct {
	Positive int    `json:"positive"`
	Negative int    `json:"negative"`
	Neutral  int    `json:"neutral"`
	Summary  string `json:"summary"`
}

type Result struct {
	Signals     []Signal   `json:"signals"`
	Lean        Lean       `json:"lean"`
	Headlines   []Headline `json:"headlines"`
	Basis       string     `json:"basis"`     // what the numbers were computed from
	Simulated   bool       `json:"simulated"` // prices are simulated: signals are a demo only
	GeneratedAt time.Time  `json:"generatedAt"`
}

// Input is everything Compute needs (pure function: easy to test).
type Input struct {
	Segment   string // "EQ", "INDEX", "CRYPTO"
	Simulated bool
	Quote     proto.Quote
	Hourly    []candles.Candle // oldest first
	News      []news.Item      // nil = news unavailable
	Now       time.Time
}

// Compute derives all signals. It never panics on short or empty input; signals
// lacking data report Unknown instead of a guess.
func Compute(in Input) Result {
	closes := make([]float64, len(in.Hourly))
	for i, b := range in.Hourly {
		closes[i] = b.C
	}
	last := in.Quote.LTP
	if last == 0 && len(closes) > 0 {
		last = closes[len(closes)-1]
	}

	sig := []Signal{
		trend(closes, last),
		momentum(closes),
		volatility(closes, in.Segment),
		performance(closes, last),
		activity(in.Hourly, in.Segment),
	}
	newsSig, heads := newsTone(in.News, in.Now)
	sig = append(sig, newsSig)

	var lean Lean
	for _, s := range sig {
		switch s.Tone {
		case Positive:
			lean.Positive++
		case Negative:
			lean.Negative++
		case Neutral:
			lean.Neutral++
		}
	}
	switch d := lean.Positive - lean.Negative; {
	case lean.Positive+lean.Negative+lean.Neutral < 3:
		lean.Summary = "Not enough data yet to read the signals"
	case d >= 2:
		lean.Summary = "Signals lean positive"
	case d <= -2:
		lean.Summary = "Signals lean negative"
	default:
		lean.Summary = "Signals are mixed"
	}

	return Result{
		Signals:     sig,
		Lean:        lean,
		Headlines:   heads,
		Basis:       fmt.Sprintf("%d hourly bars · %d recent headlines", len(in.Hourly), len(in.News)),
		Simulated:   in.Simulated,
		GeneratedAt: in.Now.UTC(),
	}
}

// ---------- price signals ----------

func sma(xs []float64, n int) float64 {
	if len(xs) < n || n == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs[len(xs)-n:] {
		s += x
	}
	return s / float64(n)
}

// trend compares price with its 20- and 50-hour simple moving averages.
func trend(closes []float64, last float64) Signal {
	s := Signal{Key: "trend", Label: "Trend (20h vs 50h average)"}
	if len(closes) < 50 || last == 0 {
		return unknown(s, "Needs at least 50 hourly bars")
	}
	s20, s50 := sma(closes, 20), sma(closes, 50)
	s.Value = fmt.Sprintf("%+.2f%% vs 50h avg", (last/s50-1)*100)
	switch {
	case last > s20 && s20 > s50:
		s.Tone, s.Detail = Positive, "Price is above both averages and the short average is above the long one, a conventional uptrend pattern."
	case last < s20 && s20 < s50:
		s.Tone, s.Detail = Negative, "Price is below both averages and the short average is below the long one, a conventional downtrend pattern."
	default:
		s.Tone, s.Detail = Neutral, "Price and averages are crossing each other, so there's no clear trend in this window."
	}
	return s
}

// rsi is Wilder's 14-period Relative Strength Index.
func rsi(closes []float64, n int) (float64, bool) {
	if len(closes) <= n {
		return 0, false
	}
	var gain, loss float64
	for i := 1; i <= n; i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	gain /= float64(n)
	loss /= float64(n)
	for i := n + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		g, l := math.Max(d, 0), math.Max(-d, 0)
		gain = (gain*float64(n-1) + g) / float64(n)
		loss = (loss*float64(n-1) + l) / float64(n)
	}
	if loss == 0 {
		return 100, true
	}
	return 100 - 100/(1+gain/loss), true
}

func momentum(closes []float64) Signal {
	s := Signal{Key: "momentum", Label: "Momentum (RSI 14h)"}
	v, ok := rsi(closes, 14)
	if !ok {
		return unknown(s, "Needs at least 15 hourly bars")
	}
	s.Value = fmt.Sprintf("%.0f", v)
	switch {
	case v >= 70:
		// High RSI is "strong momentum" but also "stretched": report both readings, tone neutral.
		s.Tone, s.Detail = Neutral, "Above 70: strong recent buying. Traders call this 'overbought', meaning the move may be stretched."
	case v <= 30:
		s.Tone, s.Detail = Neutral, "Below 30: strong recent selling. Traders call this 'oversold', meaning the move may be stretched."
	case v >= 55:
		s.Tone, s.Detail = Positive, "Gains have outweighed losses recently, without being stretched."
	case v <= 45:
		s.Tone, s.Detail = Negative, "Losses have outweighed gains recently, without being stretched."
	default:
		s.Tone, s.Detail = Neutral, "Gains and losses are roughly balanced."
	}
	return s
}

// volatility is the standard deviation of hourly log returns, scaled to a day.
func volatility(closes []float64, segment string) Signal {
	s := Signal{Key: "volatility", Label: "Volatility (daily, est.)"}
	if len(closes) < 25 {
		return unknown(s, "Needs at least 25 hourly bars")
	}
	window := closes[max(0, len(closes)-120):] // ~5 days
	var rets []float64
	for i := 1; i < len(window); i++ {
		if window[i-1] > 0 && window[i] > 0 {
			rets = append(rets, math.Log(window[i]/window[i-1]))
		}
	}
	mean := 0.0
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	v := 0.0
	for _, r := range rets {
		v += (r - mean) * (r - mean)
	}
	daily := math.Sqrt(v/float64(len(rets)-1)) * math.Sqrt(24) * 100
	s.Value = fmt.Sprintf("±%.1f%% / day", daily)
	// Thresholds differ by asset class: 3%/day is calm for crypto, wild for a large-cap stock.
	lo, hi := 1.0, 2.5
	if segment == "CRYPTO" {
		lo, hi = 2.5, 6
	}
	s.Tone = Neutral // volatility is risk, not direction
	switch {
	case daily >= hi:
		s.Detail = "High for this asset class: expect large swings both ways. Size positions more cautiously."
	case daily <= lo:
		s.Detail = "Low for this asset class: price has been moving calmly."
	default:
		s.Detail = "Typical for this asset class."
	}
	return s
}

func performance(closes []float64, last float64) Signal {
	s := Signal{Key: "performance", Label: "Performance (24h · 5d)"}
	if len(closes) < 25 || last == 0 {
		return unknown(s, "Needs at least 25 hourly bars")
	}
	d1 := (last/closes[len(closes)-25] - 1) * 100
	d5 := math.NaN()
	if len(closes) >= 121 {
		d5 = (last/closes[len(closes)-121] - 1) * 100
	}
	if math.IsNaN(d5) {
		s.Value = fmt.Sprintf("%+.2f%% · n/a", d1)
	} else {
		s.Value = fmt.Sprintf("%+.2f%% · %+.2f%%", d1, d5)
	}
	ref := d1
	if !math.IsNaN(d5) {
		ref = d5
	}
	switch {
	case ref > 1:
		s.Tone, s.Detail = Positive, "Up over the period. Past performance does not indicate future results."
	case ref < -1:
		s.Tone, s.Detail = Negative, "Down over the period. Past performance does not indicate future results."
	default:
		s.Tone, s.Detail = Neutral, "Roughly flat over the period."
	}
	return s
}

// activity compares the last 24h of volume with the average of earlier 24h windows.
func activity(bars []candles.Candle, segment string) Signal {
	s := Signal{Key: "activity", Label: "Trading activity (24h volume)"}
	if segment == "INDEX" {
		return unknown(s, "Indices have no traded volume")
	}
	if len(bars) < 48 {
		return unknown(s, "Needs at least 48 hourly bars")
	}
	sum := func(bs []candles.Candle) float64 {
		t := 0.0
		for _, b := range bs {
			t += b.V
		}
		return t
	}
	recent := sum(bars[len(bars)-24:])
	var prior []float64
	for end := len(bars) - 24; end-24 >= 0 && len(prior) < 5; end -= 24 {
		prior = append(prior, sum(bars[end-24:end]))
	}
	avg := 0.0
	for _, p := range prior {
		avg += p
	}
	avg /= float64(len(prior))
	if avg == 0 {
		return unknown(s, "No volume history")
	}
	ratio := recent / avg
	s.Value = fmt.Sprintf("%.1f× usual", ratio)
	s.Tone = Neutral // volume confirms moves; it isn't a direction on its own
	switch {
	case ratio >= 1.5:
		s.Detail = "Unusually busy. Heavy volume often accompanies news or a decisive move."
	case ratio <= 0.6:
		s.Detail = "Quieter than usual. Price moves on thin volume can be less reliable."
	default:
		s.Detail = "In line with recent days."
	}
	return s
}

func unknown(s Signal, why string) Signal {
	s.Tone, s.Value, s.Detail = Unknown, "—", why
	return s
}

// ---------- headline tone ----------

// A small, auditable finance lexicon. Crude by design: it reads headline
// wording, not meaning, and the UI says so and shows the headlines behind it.
var (
	posWords = []string{"surge", "surges", "soar", "soars", "jump", "jumps", "rally", "rallies", "gain", "gains", "rise", "rises", "rose",
		"climb", "climbs", "record high", "all-time high", "upgrade", "upgrades", "upgraded", "beat", "beats", "outperform", "outperforms",
		"strong", "growth", "profit rises", "profit jumps", "buy rating", "bullish", "boost", "boosts", "approval", "approved", "wins", "expands", "inflows"}
	negWords = []string{"fall", "falls", "fell", "drop", "drops", "dropped", "plunge", "plunges", "slump", "slumps", "slide", "slides", "tumble", "tumbles",
		"crash", "crashes", "sink", "sinks", "downgrade", "downgrades", "downgraded", "miss", "misses", "loss", "losses", "weak", "probe", "penalty",
		"fine", "fraud", "lawsuit", "sell-off", "selloff", "bearish", "cut", "cuts", "outflows", "default", "warning", "decline", "declines", "lower"}
	posRe = wordsRe(posWords)
	negRe = wordsRe(negWords)
)

func wordsRe(ws []string) *regexp.Regexp {
	q := make([]string, len(ws))
	for i, w := range ws {
		q[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(q, "|") + `)\b`)
}

// HeadlineTone scores one headline by counting lexicon hits.
func HeadlineTone(title string) string {
	p := len(posRe.FindAllString(title, -1))
	n := len(negRe.FindAllString(title, -1))
	switch {
	case p > n:
		return Positive
	case n > p:
		return Negative
	default:
		return Neutral
	}
}

func newsTone(items []news.Item, now time.Time) (Signal, []Headline) {
	s := Signal{Key: "news", Label: "News tone (last 72h)"}
	if items == nil {
		return unknown(s, "News is unavailable right now"), nil
	}
	var heads []Headline
	pos, neg := 0, 0
	for _, it := range items {
		if now.Sub(it.Published) > 72*time.Hour {
			continue
		}
		t := HeadlineTone(it.Title)
		switch t {
		case Positive:
			pos++
		case Negative:
			neg++
		}
		heads = append(heads, Headline{Title: it.Title, Source: it.Source, URL: it.URL, Tone: t, Time: it.Published})
	}
	if len(heads) == 0 {
		return unknown(s, "No headlines in the last 72 hours"), nil
	}
	s.Value = fmt.Sprintf("%d positive · %d negative · %d neutral", pos, neg, len(heads)-pos-neg)
	switch {
	case pos >= neg+2:
		s.Tone, s.Detail = Positive, "Recent headlines use mostly positive wording. This is a word count, not analysis; read the stories."
	case neg >= pos+2:
		s.Tone, s.Detail = Negative, "Recent headlines use mostly negative wording. This is a word count, not analysis; read the stories."
	default:
		s.Tone, s.Detail = Neutral, "Headline wording is balanced or mostly neutral."
	}
	// Surface the clearest examples first: toned headlines before neutral ones.
	ordered := make([]Headline, 0, len(heads))
	for _, want := range []string{Positive, Negative, Neutral} {
		for _, h := range heads {
			if h.Tone == want {
				ordered = append(ordered, h)
			}
		}
	}
	if len(ordered) > 6 {
		ordered = ordered[:6]
	}
	return s, ordered
}
