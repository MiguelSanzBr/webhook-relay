# webhook-relay

[![ci](https://github.com/MiguelSanzBr/webhook-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/MiguelSanzBr/webhook-relay/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/MiguelSanzBr/webhook-relay)](go.mod)
[![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](.github/workflows/ci.yml)
[![license](https://img.shields.io/github/license/MiguelSanzBr/webhook-relay)](LICENSE)

A Go service that receives webhooks, checks they come from who they claim to, blocks replays and request floods, and only then forwards them to the target with retries, signed again by the relay itself. No dependencies outside the standard library.

I built this because of a real incident: a webhook that could be forged. This is the right way to close that hole, small enough to read end to end in an interview.

**In 30 seconds:** HMAC-SHA256 over timestamp, nonce and body, compared in constant time. 5 min window and each nonce used once. Per-IP rate limit (per /64 on IPv6) before reading the body. Forward retries only when they help, a deadline per attempt, no redirect following, and the relay's own signature. Hexagonal with the dependency rule under test, TDD in the history, 100% coverage gated in CI, `govulncheck` and a Docker smoke test in CI.

[Versão em português](README.md)

## Running it

```bash
export WEBHOOK_SECRET=$(openssl rand -hex 32) FORWARD_SECRET=$(openssl rand -hex 32)
docker compose up -d --build --wait && go run ./examples/sender
```

Starts the relay on `127.0.0.1:8080` and a demo target that just echoes what it got to its log, then sends a signed webhook with the Go sender. It answers `200 OK {"status":"repassado"}`. Run `NONCE=evt_1 go run ./examples/sender` twice and the second one gets a 409. What reached the target, already signed by the relay: `docker compose logs destino`.

## How it works

```
POST /webhook
  │
  ├─ per-IP rate limit, /64 on IPv6 (token bucket)  → 429 + Retry-After
  ├─ body up to 1 MiB                               → 413
  ├─ X-Webhook-Timestamp / Nonce / Signature?       → 401
  ├─ nonce [A-Za-z0-9_-]{1,128} and HMAC-SHA256     → 401
  ├─ timestamp inside the 5 min window              → 401
  ├─ unseen nonce                                   → 409
  ├─ nonce or bucket store down                     → 503
  └─ forward to TARGET_URL, signed with FORWARD_SECRET, with retries
        ├─ 2xx                        → 200
        └─ failed after retries       → 502 and the nonce is released
```

The sender signs like this:

```
X-Webhook-Timestamp: 1760970000          (unix, seconds)
X-Webhook-Nonce:     evt_8f2c...         (unique per event, only A-Z a-z 0-9 _ -, up to 128)
X-Webhook-Signature: sha256=hex(HMAC_SHA256(secret, timestamp + "." + nonce + "." + body))
```

In Go, with the [`signature`](signature/signature.go) package (it's what [`examples/sender`](examples/sender/main.go) uses):

```go
body := []byte(`{"order":42}`)
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://relay.example/webhook", bytes.NewReader(body))
req.Header.Set("Content-Type", "application/json")
if err := signature.SetHeaders(req.Header, secret, "evt_42", body, time.Now()); err != nil {
	return err // nonce out of format
}
resp, err := http.DefaultClient.Do(req)
```

The forward reaches the target with the same three headers, signed with `FORWARD_SECRET`, and the nonce in `Idempotency-Key`. The target checks it with `signature.Verify(forwardSecret, ts, nonce, body, sig)`.

By hand, with openssl:

```bash
TS=$(date +%s); N=evt_$RANDOM; B='{"order":42,"status":"paid"}'
SIG=$(printf '%s' "$TS.$N.$B" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | awk '{print $2}')
curl -i -X POST localhost:8080/webhook -H 'Content-Type: application/json' \
  -H "X-Webhook-Timestamp: $TS" -H "X-Webhook-Nonce: $N" -H "X-Webhook-Signature: sha256=$SIG" -d "$B"
```

## Decisions and why

- **Timestamp and nonce are part of the HMAC.** If only the body were signed, you could take a valid webhook and resend it with a new nonce and timestamp. Signing all three together means touching any of them breaks the signature.
- **Nonce is only `A-Z a-z 0-9 _ -`, up to 128.** A security review found the hole: with a dot in the nonce, `"ts.nonce.body"` could be split more than one way. You could move the start of the body into the nonce (`evt_1.{"value":10` + `50}`) and the same signature was valid with another nonce, meaning a replay got through. With no dot in the nonce and an integer timestamp, the concatenation splits only one way. The proof scenario became a regression test.
- **`hmac.Equal`, not `==`.** `==` stops at the first different byte, so response time leaks how many bytes of the signature are already right. `hmac.Equal` always takes the same time.
- **Signature before nonce.** Nonces live in memory. If I stored them before checking the signature, anyone could fill memory by sending made-up nonces.
- **Rate limit before reading the body, per /64 on IPv6.** It's the cheapest check, so it goes first. Keyed by exact address, anyone with a /64 (the default for any VPS) switched IP on every request and had no limit at all. The bucket map is capped at 100k; when full, a new client gets 429 until the next sweep. `Retry-After` comes from `RATE`, not a fixed 1.
- **A deadline for everything in the server.** With only `ReadHeaderTimeout`, sending 1 byte every 2 s held the connection forever (slowloris) and an idle keep-alive never closed. Now there's a 10 s `ReadTimeout`, a `WriteTimeout` sized to the read plus the whole `TIMEOUT` (the response only goes out after the forward), `IdleTimeout` and `MaxHeaderBytes`.
- **The relay authenticates to the target.** The forward is signed again with its own `FORWARD_SECRET`, in the same format as the input. Without it, anyone who could reach `TARGET_URL` directly skipped signature, window, nonce and rate limit. `FORWARD_SECRET` must differ from the input secret, otherwise the original webhook would already pass at the target.
- **The forward doesn't follow redirects.** The default `http.Client` does: a 307 took the body to another host and a 302 became a bodyless GET counted as delivered. Now 3xx is a failure.
- **A deadline per attempt.** With only the total deadline, a target that accepts the connection and hangs burned the whole `TIMEOUT` on the first attempt and the retry never happened. Each attempt gets `TIMEOUT/MAX_ATTEMPTS`.
- **Failed forward releases the nonce.** The sender will retry the same event. If the nonce stayed taken, the legitimate retry would get a 409 and the event would be lost. The release happens even if the sender already gave up.
- **The nonce becomes `Idempotency-Key` on the forward.** The relay retries too, so the target may get the same event twice. With the key it drops the copy. Same reasoning as in [outbox-gateway-demo](https://github.com/MiguelSanzBr/outbox-gateway-demo).
- **Retry only when it helps:** network errors, timeouts, 429 and 5xx. Any other 4xx is a bad request and repeating it changes nothing. Exponential backoff with jitter so retries don't all land at once.
- **A store that's down fails closed.** The ports return errors. If the nonce store goes down, the relay can't tell whether the nonce is new, so it answers 503 and forwards nothing. Failing open here would let replays through.
- **Secret rotation.** `WEBHOOK_SECRET` takes more than one, comma separated. During the switch both the old and the new one are valid.
- **Bad config kills startup.** A secret shorter than 32 characters, `RATE=0`, `TIMEOUT=10` (no unit) or `TARGET_URL=ftp://...` stop the process with an error instead of silently falling back to a default.
- **You can see what's going on.** Every request has an `X-Request-Id` (the incoming one if short and clean, otherwise the relay makes a new one) that goes on every log line, one line per request with status and duration, and `/debug/vars` (expvar) counts responses per route and status. Network errors are logged without the target URL, which may carry a token in the query.

## Architecture

Hexagonal, with the domain in the middle. Arrows are imports, always pointing inward.

```
                         cmd/relay
             (config, wiring, HTTP server, -health)
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
 adapters/httpapi      adapters/memory     adapters/httpforward
 (HTTP entry, id,      (nonces, buckets)   (signed forward,
  log, metrics)                             retry)
        │                     │ implements          │ implements
        │                     ▼                     ▼
        └────────────▶ app: RelayWebhook + ports
                       NonceStore, Limiter, Forwarder, Clock
                              │
                              ▼
                       domain: SignedWebhook, Sign/Verify,
                       ReplayPolicy, TokenBucket, RetryPolicy
                              ▲
                              │
   examples/sender ───▶ signature (public, re-exports the domain)
```

**Why hexagonal here.** The relay's most obvious limit is being single instance, because nonces and buckets live in memory. The ports already take a `ctx` and return an error, so a Redis adapter can honor cancellation and say "I'm down", and the use case fails closed with a 503. Moving to Redis means writing that adapter (an atomic `Reserve` with `SET NX` and expiry, the bucket in a Lua script), and the use case, the domain and their tests stay the same. Swapping the synchronous forward for an outbox is another `Forwarder`. And the order of the checks, which is where the security is, lives in one place and is tested without spinning up HTTP.

No ceremony: one use case, four ports, three adapters. The handler uses `RelayWebhook` directly, no interface for something with a single implementation. The `signature` package only re-exports the domain so code outside the module can import it. The dependency rule isn't just in the README: `TestDependencyRule` reads the imports of `internal/domain` and `internal/app` with `go/parser` and fails on `net/http`, an adapter or any package outside the allowlist.

| Guarantee | Where it lives |
|---|---|
| HMAC of `ts.nonce.body`, nonce format and comparison with `hmac.Equal` | `domain/signature.go` |
| 5 min window, invalid timestamp kept apart from stale, how long to remember the nonce | `domain/replay.go` (`ReplayPolicy`) |
| Token bucket math, capped at the burst, time until the next token | `domain/ratelimit.go` (`TokenBucket`) |
| 429 and 5xx retry, other 4xx don't, backoff with jitter, deadline per attempt | `domain/retry.go` (`RetryPolicy`) |
| Order rate limit, body, signature, nonce, forward; store down fails closed; nonce released when the forward fails; total deadline; secret rotation | `app/relay.go` (`RelayWebhook`) |
| Nonces and buckets in memory, with eviction and a bucket cap | `adapters/memory` |
| Signed forward, nonce as `Idempotency-Key`, no redirects, network errors without the URL | `adapters/httpforward` |
| Body up to 1 MiB, required headers, client per /64, errors mapped to 401/409/413/429/502/503, request id, log, metrics | `adapters/httpapi` |
| Validated config, server deadlines, clean shutdown, `-health` | `cmd/relay` |
| Signing and verifying from outside the module | `signature` |

## Tests

Real TDD: every part went in test first, failing, then the code. You can see it in the history, the `Teste: ...` commits come before the code for each part. The move to hexagonal went in small `Refatoração: ...` commits, green before and after. The fixes from the two reviews (security and design) took the same path: each hole first became a failing test.

| Guarantee | Test |
|---|---|
| Changing body, timestamp, nonce or secret invalidates the signature; a nonce shifted across the dot is rejected | `TestVerify`, `TestDotShiftReplayIsRejected` |
| Rate limit before reading the body, nonce stored only after the signature, forward with a deadline and only once, secret rotation | `TestRelayRejects`, `TestRelayForwardsOnce`, `TestRelayAcceptsAnyActiveSecret` |
| Store down fails closed without forwarding; the sender's ctx reaches the forward and `Release` doesn't inherit its cancellation | `TestStoreDownFailsClosed`, `TestStoreDownIs503`, `TestCallerContextReachesForward` |
| Body swapped, missing headers, 6 min old or invalid timestamp, body > 1 MiB and connection dropping mid-read: nothing reaches the target | `TestWebhookChecks` |
| The target gets the body, the `Content-Type`, the `Idempotency-Key` and the relay's signature, not the sender's | `TestForwardedRequest`, `TestForwardSendsBodyAndIdempotencyKey` |
| Target down returns 502, and the sender's retry with the same nonce goes through once the target is back | `TestFailedForwardLetsSenderRetry`, `TestFailedForwardReleasesNonce` |
| Burst and refill, one bucket per IPv4 and per /64, `Retry-After` from `RATE`, bucket cap | `TestTokenBucket`, `TestRateLimitPerClient`, `TestRateLimitPerIP`, `TestBucketsCap` |
| Eviction only removes what expired: a live nonce and a half-empty bucket stay | `TestSweepKeepsLiveNonces`, `TestSweepKeepsPartialBuckets` |
| N goroutines on the same nonce: one wins; on the same bucket: only the burst (this is what gives `-race` something to do) | `TestConcurrentReserveOnlyOneWins`, `TestConcurrentAllowRespectsBurst` |
| Retries on 503/429/500, none on 400, no redirect following, retry on a hung target, stops when the client gives up | `TestForwardRetries`, `TestForwardDoesNotFollowRedirects`, `TestForwardRetriesHungTarget`, `TestForwardStopsWhenContextEnds` |
| Request id in the header and in the log, count per status | `TestRequestIDGoesToLog`, `TestStatusMetrics` |
| Invalid config kills startup, the `main` wiring connects `RATE`/`BURST`/`WINDOW` right, server with every deadline, `-health`, `SIGINT` shuts down cleanly | `TestLoadConfig`, `TestWiringFromEnv`, `TestServerHasDeadlines`, `TestHealthFlag`, `TestMainGracefulShutdown` |
| The example sender against the real relay: 200, then 409 with the same nonce | `TestSenderAgainstRelay`, `ExampleSetHeaders` |
| Domain and application without `net/http`, without adapters, allowlist only | `TestDependencyRule` |

49 tests and 32 subtests, 100% statement coverage gated in CI, `-race` on everything. The clock is injected, so window and rate limit tests don't `sleep`; 15 runs in a row with `-race`, no flakes.

100% coverage doesn't say the tests protect anything, so I break things on purpose in a copy and check that some test fails. In the first version that was three breaks and all three were caught. A later review tried 16 and 10 went through silently (eviction deleting a live nonce, a mutex removed, 3xx counted as success, `RATE` swapped with `BURST`...). I wrote the missing tests and redid it with 30 breaks, including the new defenses: 29 caught. The one that survives is the backoff without the random half of the jitter, and I accept that one: testing randomness without turning it into a flake costs more than it's worth.

```bash
go test -race -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1
```

## Known limits

- **Single instance.** Nonces and rate limit state are in memory. With more than one instance both need to move to Redis or Postgres, otherwise a replay goes through the other instance.
- **Behind a proxy** `RemoteAddr` is the proxy's and the rate limit becomes global. Then you need `X-Forwarded-For`, and only from a trusted proxy. The relay speaks plain HTTP: TLS is terminated at the proxy in front.
- **Synchronous forward.** The sender waits for the retries (up to `TIMEOUT`). If the target can be down for long, the right move is to store, answer 202 and deliver later through an outbox.
- **At-least-once delivery.** If the sender gives up midway, the forward stops and the nonce is released, even if the target already got it; the sender's retry delivers again and `Idempotency-Key` catches the copy. And a retry that arrives while the first one is still in flight gets a 409, same as "already delivered".
- **`Content-Type` is outside the signature.** It's forwarded as it came. With the nonce format only someone with the secret can build an accepted webhook, and with TLS nobody swaps it on the way, but the target shouldn't trust it alone to pick a parser.
- **`/debug/vars` is public on the relay port.** It shows status counts and process memory, no secrets. In production, block it at the proxy.
- **100k bucket cap.** With more distinct clients than that within one refill, a new client gets 429 until the next sweep. There's no global cap on concurrent connections beyond the server deadlines.
- **Shutdown waits up to 25 s** (read + `TIMEOUT` + slack). The orchestrator's grace period has to be longer; in the compose file it's 30 s.

## Configuration

| Variable | Default | |
|---|---|---|
| `WEBHOOK_SECRET` | required | at least 32 characters; several comma separated for rotation |
| `FORWARD_SECRET` | required | at least 32 characters, different from the input one; signs the forward |
| `TARGET_URL` | required | `http://` or `https://` with a host, where to forward |
| `ADDR` | `:8080` | |
| `RATE` / `BURST` | `10` / `20` | requests per second per IP / burst |
| `MAX_ATTEMPTS` | `4` | forward attempts, each with `TIMEOUT/MAX_ATTEMPTS` |
| `TIMEOUT` | `10s` | total forward time, retries included |
| `WINDOW` | `5m` | timestamp window |

Routes: `POST /webhook`, `GET /healthz` (204), `GET /debug/vars` (expvar). `relay -health` hits its own `/healthz` and is the image's healthcheck, since the image is distroless and has no shell or curl.

## Layout

```
cmd/relay              config, wiring, HTTP server with deadlines, shutdown and -health
internal/domain        rules with no HTTP and no I/O
  signature.go         SignedWebhook, nonce format, HMAC-SHA256 and constant-time comparison
  replay.go            timestamp window
  ratelimit.go         token bucket math
  retry.go             when to retry, backoff and deadline per attempt
internal/app
  ports.go             NonceStore, Limiter, Forwarder, Clock
  relay.go             RelayWebhook use case, order of the checks
internal/adapters
  memory               nonces and rate limit buckets in memory, with eviction and a cap
  httpforward          signed HTTP forward with retries and no redirects
  httpapi              handler, body limit, error to status, request id, log, metrics
internal/deps_test.go  dependency rule
signature              signing and verifying from outside the module
examples/sender        Go sender
```
