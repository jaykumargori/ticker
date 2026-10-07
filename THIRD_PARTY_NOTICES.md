# Third-party notices

Ticker is MIT-licensed (see [LICENSE](LICENSE)) and includes or depends on the following
open-source software. Each component remains under its own license. The licenses were verified
from the installed packages.

## Shipped in the frontend bundle

| Package | Version | License | Copyright |
|---|---|---|---|
| [solid-js](https://github.com/solidjs/solid) | 1.9.16 | MIT | Ryan Carniato |
| [lightweight-charts](https://github.com/tradingview/lightweight-charts) | 5.2.1 | Apache-2.0 | TradingView, Inc. |
| [fancy-canvas](https://github.com/tradingview/fancy-canvas) | 2.1.0 | MIT | TradingView, Inc. |
| [seroval](https://github.com/lxsmnsyc/seroval) / seroval-plugins | 1.6.8 | MIT | Alexis Munsayac |
| [csstype](https://github.com/frenic/csstype) | 3.2.3 | MIT | Fredrik Nicol |

**TradingView Lightweight Charts™.** Copyright TradingView, Inc. Licensed under the Apache
License, Version 2.0 (<https://www.apache.org/licenses/LICENSE-2.0>). The library's attribution
logo is kept enabled on every chart, per TradingView's attribution requirement.

## Compiled into the backend binary

| Module | Version | License | Copyright |
|---|---|---|---|
| [github.com/coder/websocket](https://github.com/coder/websocket) | 1.8.15 | ISC | Coder |
| [golang.org/x/time](https://pkg.go.dev/golang.org/x/time) | 0.16.0 | BSD-3-Clause | The Go Authors |
| Go standard library | 1.26.6 | BSD-3-Clause | The Go Authors |

## Build-time only (not distributed)

| Package | License |
|---|---|
| [Vite](https://vitejs.dev) | MIT |
| [vite-plugin-solid](https://github.com/solidjs/vite-plugin-solid) | MIT |
| [TypeScript](https://www.typescriptlang.org) | Apache-2.0 |

## Data sources

Market data, exchange rates, news headlines and thumbnails come from third-party services
(Binance, Frankfurter/ECB, ExchangeRate-API, Bing News, Google News, Yahoo Finance, and Finnhub if
enabled). This repository's license does not cover that data. Its use is governed by each
provider's terms. A commercial deployment should use properly licensed market-data and news feeds.
