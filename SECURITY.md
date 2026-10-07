# Security

This document covers what the ticker does today to protect itself and its users, how to
configure it, and what must be added before it handles accounts, orders or money.

**Current scope:** market data display only. There are no user accounts, no orders and no
personal data. Watchlists and alerts live in the user's own browser.

## Threat model (current)

| Threat | Example | Control |
|---|---|---|
| Cross-site scripting | Malicious headline text from a news feed | All feed text is HTML-stripped server-side and rendered as text (Solid escapes). Strict CSP (`script-src 'self'`) is a second layer |
| Clickjacking | The dashboard framed on a phishing site | `frame-ancestors 'none'` + `X-Frame-Options: DENY` |
| Cross-site WebSocket hijacking | A foreign site opens our socket from a visitor's browser | WebSocket `Origin` allowlist (`ALLOWED_ORIGINS`, same-origin by default) |
| SSRF | Image proxy asked to fetch `169.254.169.254` (cloud metadata) | Proxy allowlists Bing's `/th` thumbnail endpoint only, refuses redirects, relays only `image/*` ≤ 1 MB |
| Abuse amplification / DoS | One client makes us hammer Binance, Bing or Google, getting our IP banned | Per-IP token buckets, with a tighter budget on upstream-backed endpoints (news, candles). Upstream results are cached with single-flight fetches |
| Connection exhaustion | One client opens thousands of sockets | Per-IP and global connection caps with atomic admission. Connect-rate limit |
| Control-message flooding | Subscribe spam forcing snapshot writes | Per-connection message budget; violators are closed with 1008 |
| Slow clients / slowloris | Holding sockets or headers open | `ReadHeaderTimeout`, `IdleTimeout`, 16 KB header cap, a 15 s timeout on JSON handlers, and a write deadline that drops slow WebSocket readers |
| Process crash from bad upstream data | A malformed feed message panics a goroutine | Feeds, the hub and WebSocket readers recover and restart; feeds back off with jitter |
| Information disclosure | Directory listings, `.env`/dotfiles, stack traces | Static server blocks listings and dotfiles. Generic error messages to clients; detail goes to server logs only |
| Vulnerable dependencies | Known CVEs in Go stdlib or npm packages | `govulncheck` and `npm audit` both clean. The Go toolchain is pinned to a patched release (1.26.6 fixed 9 reachable stdlib CVEs, including an `encoding/xml` recursion DoS that affects our RSS parsing) |
| Secret leakage | API keys in logs or URLs | Keys only from env vars, scrubbed from upstream error messages. The access log records path and status only, never query strings or headers |

## Configuration

| Env | Default | Purpose |
|---|---|---|
| `ALLOWED_ORIGINS` | `localhost:5173,127.0.0.1:5173` | Extra WebSocket origins (same-origin is always allowed). **Set this to your domain in production** |
| `TRUST_PROXY` | `false` | Use the right-most `X-Forwarded-For` entry as the client IP. Turn on **only** when the server is reachable solely through ALB/CloudFront; otherwise clients can spoof their IP |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | — | Serve HTTPS directly (TLS 1.2+). Not needed when TLS terminates at the ALB |
| `HSTS` | `false` | Send HSTS + `upgrade-insecure-requests`. Set `true` behind an HTTPS ALB. Automatic with direct TLS |
| `RATE_LIMIT` | `on` | `off` disables per-IP limits. **Load testing only**; the server logs a warning |
| `API_RPS` | `20` | General API requests per second per IP (burst 2×) |
| `UPSTREAM_RPS` | `3` | News and candles per IP (burst 10) |
| `IMAGE_RPS` | `30` | Thumbnail proxy per IP (burst 60) |
| `WS_CONNECT_RPS` | `2` | New WebSocket connections per IP (burst 5) |
| `MAX_CONNS_PER_IP` | `20` | Concurrent sockets per IP. Raise for large offices behind one NAT IP |
| `WS_MSGS_PER_SEC` | `20` | Client→server messages per socket (burst 2×) |
| `MAX_CLIENTS` | `20000` | Global socket cap |

### Recommended AWS deployment
- **ALB** with an ACM certificate (TLS 1.2+ security policy) → this server on private subnets.
  Set `TRUST_PROXY=true` and `HSTS=true`. Security groups allow ALB → app only.
- **AWS WAF** on the ALB: the AWS managed rule groups (Core, Known Bad Inputs, IP reputation)
  plus a rate-based rule as an outer layer. The in-app limits stay as defence in depth.
  *Cost:* WAF bills per web ACL, per rule and per million requests.
- **AWS Shield Standard** is automatic. Consider Shield Advanced only once real money is
  involved; it costs a significant fixed monthly fee.
- **Secrets** (e.g. `FINNHUB_TOKEN`) go in AWS Secrets Manager or SSM Parameter Store and are
  injected as env vars, never baked into images.
- **Logs** go to CloudWatch, with alarms on spikes of 429/403 and `ws rejected` warnings.
- **Rollback:** the server is stateless. Redeploy the previous image; clients reconnect with
  backoff.

## Verification
- `go test ./cmd/server` covers:
  - CSP and hardening headers present, and no HSTS over plain HTTP;
  - directory listing and dotfiles return 404;
  - API returns 429 with `Retry-After`;
  - `X-Forwarded-For` is ignored unless trusted;
  - global and per-IP connection caps, with idempotent release;
  - a foreign WebSocket `Origin` is refused with 403;
  - message flooding closes the socket with 1008;
  - a third socket from one IP gets 503.
- `go test ./internal/news` covers SSRF allowlist rejections (metadata IP, lookalike host,
  `file://`), `javascript:` link stripping and markup stripping.
- **In the browser:** zero CSP violations across chart, news thumbnails, heatmap and WebSocket.
- **Scans:** `govulncheck ./...` reports no vulnerabilities, and `npm audit` reports 0.

Re-run the scans in CI on every change:
```bash
cd backend && go run golang.org/x/vuln/cmd/govulncheck@latest ./... && go test ./...
cd frontend && npm audit --audit-level=high
```

## Required before accounts, orders or money (not implemented)

These are deliberately **not** built yet, because there is nothing for them to protect. Each
one is a hard prerequisite for a trading product.

1. **Authentication.** Use OIDC (e.g. Amazon Cognito) with **mandatory MFA/TOTP**. Brokers in
   India are expected to use two-factor login. Use short-lived access tokens, rotating refresh
   tokens, and session revocation on logout or password change. Verify the token on the
   WebSocket upgrade, where the `TODO(auth)` sits in `cmd/server/main.go`, and on every API call.
2. **Authorization.** Every order or portfolio call must check that the resource belongs to the
   caller (no IDOR), and that the decision is enforced server-side.
3. **CSRF protection** for state-changing endpoints: `SameSite=Strict` cookies or bearer tokens,
   plus a CSRF token or origin check. The current API is GET-only.
4. **Order integrity:**
   - idempotency keys, so a retry never places an order twice;
   - server-side validation of quantity, price bands and margin;
   - per-user order rate limits;
   - re-authentication for sensitive actions such as payouts and changing bank details.
5. **Immutable audit trail:** who did what, when, and from which IP and device, kept in
   append-only storage (e.g. S3 Object Lock).
6. **Data protection:** encryption at rest (KMS), TLS everywhere internally, PII minimization,
   and retention rules.
7. **Alerts on the server.** Price alerts move server-side, so they can't be missed and can't
   be spoofed client-side.
8. **Compliance.** A SEBI-registered broker must follow SEBI's cybersecurity framework for
   regulated entities (CSCRF): VAPT, a SOC, incident reporting timelines and data localization.
   This needs review by compliance and legal, not just engineering.
9. **Independent penetration test** before launch.
