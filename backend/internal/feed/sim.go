package feed

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"ticker/internal/candles"
	"ticker/internal/hub"
)

type simStock struct {
	Sym, Name string
	Price     float64 // reference (previous close), INR
}

// simSymbols: NIFTY 50-style large caps first, then NIFTY Next 50 / midcaps.
// Exchange is labelled "NSE-SIM" so nobody mistakes this for real NSE data.
var simSymbols = []simStock{
	{"RELIANCE", "Reliance Industries", 1380}, {"TCS", "Tata Consultancy Services", 3050},
	{"HDFCBANK", "HDFC Bank", 970}, {"INFY", "Infosys", 1480}, {"ICICIBANK", "ICICI Bank", 1360},
	{"HINDUNILVR", "Hindustan Unilever", 2520}, {"ITC", "ITC", 405}, {"SBIN", "State Bank of India", 870},
	{"BHARTIARTL", "Bharti Airtel", 1940}, {"KOTAKBANK", "Kotak Mahindra Bank", 2080},
	{"LT", "Larsen & Toubro", 3650}, {"AXISBANK", "Axis Bank", 1180}, {"ASIANPAINT", "Asian Paints", 2450},
	{"MARUTI", "Maruti Suzuki", 16200}, {"SUNPHARMA", "Sun Pharma", 1610}, {"TITAN", "Titan", 3550},
	{"BAJFINANCE", "Bajaj Finance", 1010}, {"WIPRO", "Wipro", 245}, {"ULTRACEMCO", "UltraTech Cement", 12300},
	{"NESTLEIND", "Nestle India", 1210}, {"TATAMOTORS", "Tata Motors", 680}, {"TATASTEEL", "Tata Steel", 170},
	{"POWERGRID", "Power Grid", 290}, {"NTPC", "NTPC", 340}, {"ONGC", "ONGC", 240}, {"HCLTECH", "HCL Tech", 1450},
	{"TECHM", "Tech Mahindra", 1460}, {"ADANIENT", "Adani Enterprises", 2400}, {"ADANIPORTS", "Adani Ports", 1420},
	{"JSWSTEEL", "JSW Steel", 1150}, {"COALINDIA", "Coal India", 390}, {"DRREDDY", "Dr Reddy's", 1270},
	{"CIPLA", "Cipla", 1530}, {"GRASIM", "Grasim", 2800}, {"BAJAJFINSV", "Bajaj Finserv", 2010},
	{"HEROMOTOCO", "Hero MotoCorp", 5400}, {"EICHERMOT", "Eicher Motors", 6900}, {"BRITANNIA", "Britannia", 6000},
	{"DIVISLAB", "Divi's Labs", 6200}, {"APOLLOHOSP", "Apollo Hospitals", 7600}, {"INDUSINDBK", "IndusInd Bank", 750},
	{"HDFCLIFE", "HDFC Life", 760}, {"SBILIFE", "SBI Life", 1820}, {"BPCL", "BPCL", 340}, {"TATACONSUM", "Tata Consumer", 1130},
	{"M&M", "Mahindra & Mahindra", 3400}, {"SHRIRAMFIN", "Shriram Finance", 620}, {"TRENT", "Trent", 4900},
	{"BEL", "Bharat Electronics", 400}, {"ETERNAL", "Eternal", 330},
	// ---- NIFTY Next 50 / midcaps ----
	{"BANKBARODA", "Bank of Baroda", 245}, {"PNB", "Punjab National Bank", 105}, {"CANBK", "Canara Bank", 110},
	{"FEDERALBNK", "Federal Bank", 200}, {"IDFCFIRSTB", "IDFC First Bank", 70}, {"AUBANK", "AU Small Finance Bank", 750},
	{"LTIM", "LTIMindtree", 5300}, {"PERSISTENT", "Persistent Systems", 5600}, {"COFORGE", "Coforge", 1700},
	{"MPHASIS", "Mphasis", 2800}, {"OFSS", "Oracle Financial Services", 8600}, {"DMART", "Avenue Supermarts", 4200},
	{"PIDILITIND", "Pidilite Industries", 3000}, {"SIEMENS", "Siemens", 3200}, {"ABB", "ABB India", 5600},
	{"HAL", "Hindustan Aeronautics", 4700}, {"BHEL", "BHEL", 240}, {"IRCTC", "IRCTC", 750}, {"IRFC", "IRFC", 130},
	{"ZYDUSLIFE", "Zydus Lifesciences", 980}, {"LUPIN", "Lupin", 2000}, {"TORNTPHARM", "Torrent Pharma", 3400},
	{"AUROPHARMA", "Aurobindo Pharma", 1150}, {"GODREJCP", "Godrej Consumer", 1200}, {"DABUR", "Dabur", 520},
	{"MARICO", "Marico", 720}, {"COLPAL", "Colgate-Palmolive", 2300}, {"TATAPOWER", "Tata Power", 400},
	{"ADANIGREEN", "Adani Green", 1000}, {"ADANIPOWER", "Adani Power", 600}, {"VEDL", "Vedanta", 450},
	{"HINDALCO", "Hindalco", 700}, {"JINDALSTEL", "Jindal Steel", 1000}, {"SAIL", "SAIL", 130}, {"GAIL", "GAIL", 180},
	{"IOC", "Indian Oil", 145}, {"HAVELLS", "Havells", 1550}, {"VOLTAS", "Voltas", 1350}, {"POLYCAB", "Polycab", 7000},
	{"DLF", "DLF", 800}, {"GODREJPROP", "Godrej Properties", 2200}, {"INDIGO", "InterGlobe Aviation", 5800},
	{"NAUKRI", "Info Edge", 1400}, {"PAYTM", "One97 Communications", 1200}, {"NYKAA", "FSN E-Commerce", 220},
	{"POLICYBZR", "PB Fintech", 1800}, {"CHOLAFIN", "Cholamandalam Finance", 1600}, {"MUTHOOTFIN", "Muthoot Finance", 2900},
	{"BAJAJ-AUTO", "Bajaj Auto", 8600}, {"TVSMOTOR", "TVS Motor", 3400}, {"ASHOKLEY", "Ashok Leyland", 130},
	{"MOTHERSON", "Samvardhana Motherson", 100}, {"VBL", "Varun Beverages", 480}, {"JIOFIN", "Jio Financial", 320},
	{"LICI", "LIC of India", 900}, {"PFC", "Power Finance Corp", 410}, {"RECLTD", "REC", 400},
	{"ICICIGI", "ICICI Lombard", 1900}, {"HDFCAMC", "HDFC AMC", 5500},
}

// simIndices are derived from member prices, as a real index is.
var simIndices = []struct {
	Sym, Name string
	Base      float64
	Members   []string // nil = first 50 (NIFTY 50 universe)
}{
	{"NIFTY 50", "Nifty 50 Index", 24800, nil},
	{"NIFTY BANK", "Nifty Bank Index", 55000, []string{"HDFCBANK", "ICICIBANK", "SBIN", "KOTAKBANK", "AXISBANK", "INDUSINDBK", "BANKBARODA", "PNB", "FEDERALBNK", "IDFCFIRSTB", "AUBANK", "CANBK"}},
	{"NIFTY IT", "Nifty IT Index", 36000, []string{"TCS", "INFY", "HCLTECH", "WIPRO", "TECHM", "LTIM", "PERSISTENT", "COFORGE", "MPHASIS", "OFSS"}},
}

type simIndex struct {
	token   uint32
	base    float64
	members []int
	weights []float64 // normalised, sum = 1
}

// Sim generates NSE-shaped ticks (₹0.05 tick size) with a mean-reverting
// random walk. Useful offline and for load tests at arbitrary rates.
type Sim struct {
	TPS     int            // total stock ticks per second across all symbols
	Candles *candles.Store // optional: seeded with synthetic history

	h       *hub.Hub
	tokens  []uint32
	ref     []float64 // previous close
	price   []float64
	indices []simIndex
}

func (s *Sim) Name() string { return "sim" }

func (s *Sim) Init(_ context.Context, h *hub.Hub) error {
	s.h = h
	ticks := make([]hub.Tick, 0, len(simSymbols)+len(simIndices))
	pos := make(map[string]int, len(simSymbols))
	for i, sym := range simSymbols {
		t := h.Register(hub.Instrument{Exchange: "NSE-SIM", Segment: "EQ", Symbol: sym.Sym, Name: sym.Name, Decimals: 2, Currency: "INR"})
		open := roundTick(sym.Price * (1 + rand.NormFloat64()*0.004))
		s.tokens = append(s.tokens, t)
		s.ref = append(s.ref, sym.Price)
		s.price = append(s.price, open)
		pos[sym.Sym] = i
		ticks = append(ticks, hub.Tick{Token: t, Fields: hub.FLTP | hub.FOHLC | hub.FVolume,
			LTP: open, Open: open, High: open, Low: open, Close: sym.Price, TS: nowMs()})
	}

	for _, ix := range simIndices {
		members := ix.Members
		if members == nil {
			members = make([]string, 0, 50)
			for _, s := range simSymbols[:min(50, len(simSymbols))] {
				members = append(members, s.Sym)
			}
		}
		si := simIndex{token: h.Register(hub.Instrument{Exchange: "NSE-SIM", Segment: "INDEX", Symbol: ix.Sym, Name: ix.Name, Decimals: 2, Currency: "INR"}), base: ix.Base}
		for _, m := range members {
			if p, ok := pos[m]; ok {
				si.members = append(si.members, p)
				h.Tag(s.tokens[p], ix.Sym) // index membership, used by the heatmap
			}
		}
		// Deterministic pseudo free-float weights, heavier for earlier (larger)
		// names, so a few heavyweights move the index like on the real NIFTY.
		sum := 0.0
		for k := range si.members {
			w := 1 / math.Sqrt(float64(k+1))
			si.weights = append(si.weights, w)
			sum += w
		}
		for k := range si.weights {
			si.weights[k] /= sum
		}
		s.indices = append(s.indices, si)
		v := s.indexValue(&si)
		ticks = append(ticks, hub.Tick{Token: si.token, Fields: hub.FLTP | hub.FOHLC,
			LTP: v, Open: v, High: v, Low: v, Close: ix.Base, TS: nowMs()})
	}
	h.Apply(ticks...)

	if s.Candles != nil {
		// σ_1m = daily vol / √1440 (the sim runs around the clock): ~1.5%/day
		// for a large-cap stock, ~0.9%/day for an index.
		for i, t := range s.tokens {
			s.seedHistory(t, s.price[i], s.ref[i], simSigma1m, 20000, true)
		}
		for i := range s.indices {
			ix := &s.indices[i]
			s.seedHistory(ix.token, s.indexValue(ix), ix.base, simSigma1m*0.6, 0, false)
		}
	}
	slog.Info("sim initialised", "stocks", len(s.tokens), "indices", len(s.indices), "tps", s.TPS)
	return nil
}

// indexValue = base × Σ wᵢ·(pᵢ / refᵢ): a weighted price-relative index.
func (s *Sim) indexValue(ix *simIndex) float64 {
	v := 0.0
	for k, m := range ix.members {
		v += ix.weights[k] * s.price[m] / s.ref[m]
	}
	return math.Round(ix.base*v*100) / 100
}

func (s *Sim) Run(ctx context.Context) {
	if s.TPS <= 0 {
		return
	}
	const step = 10 * time.Millisecond
	const indexEvery = 10 // recompute indices every 100ms (server conflates at FLUSH_MS anyway)
	tk := time.NewTicker(step)
	defer tk.Stop()
	perStep := float64(s.TPS) * step.Seconds()
	carry := 0.0
	// Calibrate per-tick volatility so it compounds to simSigma1m per stock
	// (a typical large cap, and what seedHistory uses), whatever SIM_TPS is:
	// σ_tick = σ_1m / √(ticks per stock per minute).
	ticksPerMin := float64(s.TPS) * 60 / float64(len(s.tokens))
	sigmaTick := simSigma1m / math.Sqrt(max(ticksPerMin, 1))
	batch := make([]hub.Tick, 0, int(perStep)+len(s.indices)+1)
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		carry += perStep
		k := int(carry)
		carry -= float64(k)
		batch = batch[:0]
		ts := nowMs()
		// A shared "market" shock makes stocks co-move, so indices trend
		// instead of averaging out to a flat line.
		market := rand.NormFloat64() * sigmaTick * 0.4
		for range k {
			i := rand.IntN(len(s.tokens))
			p := s.price[i]
			drift := -0.0002 * math.Log(p/s.ref[i]) // weak pull toward the reference price
			p = roundTick(p * math.Exp(drift+market+rand.NormFloat64()*sigmaTick))
			if p < 0.05 {
				p = 0.05
			}
			s.price[i] = p
			batch = append(batch, hub.Tick{Token: s.tokens[i], Fields: hub.FLTP | hub.FAddQty,
				LTP: p, Qty: float64(1 + rand.IntN(500)), TS: ts})
		}
		if n%indexEvery == 0 {
			for i := range s.indices {
				batch = append(batch, hub.Tick{Token: s.indices[i].token, Fields: hub.FLTP,
					LTP: s.indexValue(&s.indices[i]), TS: ts})
			}
		}
		if len(batch) > 0 {
			s.h.Apply(batch...) // one lock acquisition per batch
		}
	}
}

// simSigma1m is per-minute volatility: 1.5%/day ÷ √1440 minutes.
const simSigma1m = 0.015 / 37.947

func roundTick(p float64) float64 { return math.Round(p*20) / 20 }

// seedHistory generates candles.Keep synthetic bars per interval that end at
// the current price, so charts aren't empty on a fresh server. It walks
// backwards from `last` with a weak pull toward `ref`. Volatility per bar
// scales with √interval (random-walk scaling). Each interval gets its own
// independent walk, so intervals won't agree exactly; fine for simulated data.
func (s *Sim) seedHistory(token uint32, last, ref, sigma1m, vol1m float64, tick bool) {
	now := time.Now().Unix()
	round := func(p float64) float64 {
		if tick {
			return roundTick(p)
		}
		return math.Round(p*100) / 100
	}
	for ii, iv := range candles.Intervals {
		n := candles.Keep
		sigma := sigma1m * math.Sqrt(float64(iv.Sec)/60)
		closes := make([]float64, n)
		closes[n-1] = last
		for i := n - 2; i >= 0; i-- {
			p := closes[i+1]
			closes[i] = p * math.Exp(-0.01*math.Log(p/ref)+rand.NormFloat64()*sigma)
		}
		cur := now / iv.Sec * iv.Sec // current bucket is left to live trades
		bars := make([]candles.Candle, n)
		for i := range n {
			c := closes[i]
			o := c * math.Exp(rand.NormFloat64()*sigma*0.5)
			if i > 0 {
				o = closes[i-1]
			}
			wick := math.Abs(rand.NormFloat64()) * sigma * 0.6
			v := 0.0
			if vol1m > 0 {
				v = math.Round(vol1m * float64(iv.Sec) / 60 * math.Exp(rand.NormFloat64()*0.5))
			}
			bars[i] = candles.Candle{
				T: cur - int64(n-i)*iv.Sec,
				O: round(o), C: round(c),
				H: round(max(o, c) * (1 + wick)), L: round(min(o, c) * (1 - wick)),
				V: v,
			}
		}
		s.Candles.Seed(token, ii, bars)
	}
}
