package news

import (
	"fmt"
	"net/url"
	"strings"
)

// coinNames gives searchable names for common base assets ("SOL" alone is ambiguous).
var coinNames = map[string]string{
	"BTC": "Bitcoin", "ETH": "Ethereum", "BNB": "BNB", "SOL": "Solana", "XRP": "XRP", "DOGE": "Dogecoin",
	"ADA": "Cardano", "TRX": "TRON", "AVAX": "Avalanche", "LINK": "Chainlink", "DOT": "Polkadot", "LTC": "Litecoin",
	"BCH": "Bitcoin Cash", "NEAR": "NEAR Protocol", "UNI": "Uniswap", "APT": "Aptos", "ATOM": "Cosmos",
	"ETC": "Ethereum Classic", "FIL": "Filecoin", "ARB": "Arbitrum", "OP": "Optimism", "SUI": "Sui", "INJ": "Injective",
	"AAVE": "Aave", "PEPE": "Pepe coin", "SHIB": "Shiba Inu", "XLM": "Stellar", "HBAR": "Hedera", "ICP": "Internet Computer",
	"TON": "Toncoin", "SEI": "Sei", "TIA": "Celestia", "WIF": "dogwifhat", "RENDER": "Render", "FET": "Fetch.ai",
	"ALGO": "Algorand", "VET": "VeChain", "LDO": "Lido DAO", "ENA": "Ethena", "POL": "Polygon", "ONDO": "Ondo",
	"JUP": "Jupiter", "PENDLE": "Pendle", "GALA": "Gala", "SAND": "The Sandbox", "MANA": "Decentraland",
	"AXS": "Axie Infinity", "CRV": "Curve", "COMP": "Compound", "GRT": "The Graph", "THETA": "Theta Network",
	"EGLD": "MultiversX", "CHZ": "Chiliz", "ZEC": "Zcash", "DASH": "Dash", "XTZ": "Tezos", "QNT": "Quant",
	"STX": "Stacks", "IMX": "Immutable", "FLOKI": "Floki", "BONK": "Bonk", "WLD": "Worldcoin", "PYTH": "Pyth Network",
	"STRK": "Starknet", "TAO": "Bittensor", "TRUMP": "TRUMP memecoin", "ENS": "Ethereum Name Service",
	"RUNE": "THORChain", "AR": "Arweave", "CAKE": "PancakeSwap",
}

// nseAliases disambiguate company names that are common words or that
// people know by another brand. Value: search phrase plus extra title terms.
var nseAliases = map[string]struct {
	Search string
	Terms  []string
}{
	"ETERNAL":    {`("Eternal Ltd" OR Zomato OR Blinkit)`, []string{"Eternal", "Zomato", "Blinkit"}},
	"ITC":        {`"ITC Ltd"`, []string{"ITC"}},
	"TITAN":      {`"Titan Company"`, []string{"Titan"}},
	"TRENT":      {`("Trent Ltd" OR Zudio OR Westside)`, []string{"Trent", "Zudio"}},
	"M&M":        {`("Mahindra & Mahindra" OR "M&M")`, []string{"Mahindra", "M&M"}},
	"LT":         {`("Larsen & Toubro" OR "L&T")`, []string{"Larsen", "L&T"}},
	"RECLTD":     {`"REC Ltd"`, []string{"REC"}},
	"GAIL":       {`"GAIL India"`, []string{"GAIL"}},
	"SIEMENS":    {`"Siemens Ltd"`, []string{"Siemens"}},
	"ABB":        {`"ABB India"`, []string{"ABB"}},
	"IOC":        {`("Indian Oil" OR IOCL)`, []string{"Indian Oil", "IOC", "IOCL"}},
	"PAYTM":      {`(Paytm OR "One97")`, []string{"Paytm", "One97"}},
	"NYKAA":      {`(Nykaa OR "FSN E-Commerce")`, []string{"Nykaa", "FSN"}},
	"POLICYBZR":  {`(Policybazaar OR "PB Fintech")`, []string{"Policybazaar", "PB Fintech"}},
	"NAUKRI":     {`("Info Edge" OR Naukri)`, []string{"Info Edge", "Naukri"}},
	"DMART":      {`(DMart OR "Avenue Supermarts")`, []string{"DMart", "D-Mart", "Avenue Supermarts"}},
	"INDIGO":     {`(IndiGo OR "InterGlobe Aviation")`, []string{"IndiGo", "InterGlobe"}},
	"LICI":       {`("LIC" OR "Life Insurance Corporation")`, []string{"LIC", "Life Insurance Corporation"}},
	"JIOFIN":     {`"Jio Financial"`, []string{"Jio Financial", "JFSL"}},
	"MOTHERSON":  {`"Samvardhana Motherson"`, []string{"Motherson"}},
	"BAJAJ-AUTO": {`"Bajaj Auto"`, []string{"Bajaj Auto"}},
}

// firstWord: "Reliance Industries" → "Reliance" (the term headlines actually use).
func firstWord(name string) string {
	f := strings.Fields(name)
	if len(f) == 0 {
		return name
	}
	return strings.Trim(f[0], "'’")
}

var stableQuotes = []string{"USDT", "USDC", "FDUSD", "TUSD"}

func googleNews(q, hl, gl string) Feed {
	v := url.Values{"q": {q}, "hl": {hl}, "gl": {gl}, "ceid": {gl + ":" + strings.SplitN(hl, "-", 2)[0]}}
	return Feed{Provider: "Google News", URL: "https://news.google.com/rss/search?" + v.Encode()}
}

// bing has thumbnails and publisher links; preferred where it has coverage.
func bing(q, mkt string) Feed {
	v := url.Values{"q": {q}, "format": {"rss"}, "mkt": {mkt}}
	return Feed{Provider: "Bing News", URL: "https://www.bing.com/news/search?" + v.Encode()}
}

func yahoo(symbol string) Feed {
	v := url.Values{"s": {symbol}, "region": {"US"}, "lang": {"en-US"}}
	return Feed{Provider: "Yahoo Finance", URL: "https://feeds.finance.yahoo.com/rss/2.0/headline?" + v.Encode()}
}

// QueryFor maps an instrument to its news feeds (first non-empty wins).
func QueryFor(exchange, segment, symbol, name string) Query {
	key := exchange + ":" + symbol
	switch {
	case exchange == "CRYPTO":
		base := symbol
		for _, q := range stableQuotes {
			if b, ok := strings.CutSuffix(base, q); ok {
				base = b
				break
			}
		}
		coin := coinNames[base]
		if coin == "" {
			coin = base
		}
		label := coin + " crypto"
		return Query{Key: key, Label: label, Terms: []string{strings.TrimSuffix(coin, " coin"), base}, Feeds: []Feed{
			bing(fmt.Sprintf("%q crypto", strings.TrimSuffix(coin, " coin")), "en-US"),
			bing(strings.TrimSuffix(coin, " coin")+" price", "en-US"),
			yahoo(base + "-USD"),
			googleNews(fmt.Sprintf("%q crypto when:7d", strings.TrimSuffix(coin, " coin")), "en-US", "US"),
		}}
	case segment == "INDEX":
		short := strings.TrimSuffix(name, " Index") // "Nifty Bank"
		terms := []string{short}
		switch symbol {
		case "NIFTY 50":
			terms = append(terms, "Nifty", "Sensex", "stock market")
		case "NIFTY BANK":
			terms = append(terms, "Bank Nifty", "bank stocks", "banking stocks")
		case "NIFTY IT":
			terms = append(terms, "IT stocks", "IT shares", "IT index")
		}
		q := fmt.Sprintf("%q when:3d", short)
		return Query{Key: key, Label: name, Terms: terms, Feeds: []Feed{bing(fmt.Sprintf("%q", short), "en-IN"), bing(short+" today", "en-IN"), googleNews(q, "en-IN", "IN")}}
	case strings.HasPrefix(exchange, "NSE"):
		search := fmt.Sprintf("%q", name)
		terms := []string{name, firstWord(name), symbol}
		if a, ok := nseAliases[symbol]; ok {
			search, terms = a.Search, a.Terms
		}
		finance := `(shares OR "share price" OR stock OR NSE OR BSE OR Sensex OR Nifty)`
		// Two Bing queries: plain-name results carry the most thumbnails,
		// "<name> stock" adds market coverage. Google adds breadth.
		return Query{Key: key, Label: name, Terms: terms, Feeds: []Feed{
			bing(search, "en-IN"),
			bing(search+" stock", "en-IN"),
			googleNews(search+" "+finance+" when:7d", "en-IN", "IN"),
		}}
	default: // US equities
		return Query{Key: key, Label: symbol, Feeds: []Feed{bing(fmt.Sprintf("%q stock", name), "en-US"), yahoo(symbol), googleNews(fmt.Sprintf("%q stock when:7d", name), "en-US", "US")}}
	}
}
