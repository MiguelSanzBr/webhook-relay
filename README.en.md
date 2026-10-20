# webhook-relay

A Go service that receives webhooks, checks they come from who they claim to, blocks replays and request floods, and only then forwards them to the target with retries. No dependencies outside the standard library.

I built this because of a real incident: a webhook that could be forged. This is the right way to close that hole, small enough to read end to end in an interview.

[Versão em português](README.md)

## How it works

```
POST /webhook
  │
  ├─ per-IP rate limit (token bucket)          → 429 + Retry-After
  ├─ body up to 1 MiB                          → 413
  ├─ X-Webhook-Timestamp / Nonce / Signature?  → 401
  ├─ HMAC-SHA256 of "ts.nonce.body"            → 401
  ├─ timestamp inside the 5 min window         → 401
  ├─ unseen nonce                              → 409
  └─ forward to TARGET_URL with retries
        ├─ 2xx                        → 200
        └─ failed after retries       → 502 and the nonce is released
```

The sender signs like this:

```
X-Webhook-Timestamp: 1760970000          (unix, seconds)
X-Webhook-Nonce:     evt_8f2c...         (unique per event)
X-Webhook-Signature: sha256=hex(HMAC_SHA256(secret, timestamp + "." + nonce + "." + body))
```

## Decisions and why

- **Timestamp and nonce are part of the HMAC.** If only the body were signed, you could take a valid webhook and resend it with a new nonce and timestamp. Signing all three together means touching any of them breaks the signature.
- **`hmac.Equal`, not `==`.** `==` stops at the first different byte, so response time leaks how many bytes of the signature are already right. `hmac.Equal` always takes the same time.
- **Signature before nonce.** Nonces live in memory. If I stored them before checking the signature, anyone could fill memory by sending made-up nonces.
- **Rate limit before reading the body.** It's the cheapest check, so it goes first. A noisy client never gets to make me read 1 MiB.
- **Failed forward releases the nonce.** The sender will retry the same event. If the nonce stayed taken, the legitimate retry would get a 409 and the event would be lost.
- **The nonce becomes `Idempotency-Key` on the forward.** The relay retries too, so the target may get the same event twice. With the key it drops the copy. Same reasoning as in [outbox-gateway-demo](https://github.com/MiguelSanzBr/outbox-gateway-demo).
- **Retry only when it helps:** network errors, timeouts, 429 and 5xx. Any other 4xx is a bad request and repeating it changes nothing. Exponential backoff with jitter so retries don't all land at once.
- **Bad config kills startup.** A secret shorter than 32 characters, `RATE=0` or `TIMEOUT=10` (no unit) stop the process with an error instead of silently falling back to a default.

## Tests

Real TDD: every part went in test first, failing, then the code. You can see it in the history, the `Teste: ...` commits come before the code for each part.

| Guarantee | Test |
|---|---|
| Changing body, timestamp, nonce or secret invalidates the signature | `TestVerify` |
| Body swapped after signing, missing headers, 6 min old timestamp, same webhook twice, body > 1 MiB and connection dropping mid-read: none of it reaches the target | `TestWebhookChecks` |
| Target down returns 502, and the sender's retry with the same nonce goes through once the target is back | `TestFailedForwardLetsSenderRetry` |
| Rate limit burst and refill, one bucket per IP, no accumulation past the cap | `TestRateLimiterBurstAndRefill` |
| Old nonces and buckets are evicted from memory | `TestExpiredNoncesAreSwept`, `TestIdleBucketsAreSwept` |
| Retries on 503/429/500, no retry on 400, stops right away when the client gives up | `TestForwardRetries`, `TestForwardStopsWhenContextEnds` |
| Invalid config kills startup; `SIGINT` shuts down cleanly | `TestLoadConfig`, `TestMainGracefulShutdown` |

100% statement coverage, enforced in CI. The clock is injected, so window and rate limit tests don't `sleep`.

To check the tests actually protect something instead of just covering lines, I broke three things on purpose: removed the signature check, removed the nonce release, and removed timestamp and nonce from the HMAC. The tests failed in all three cases.

## Known limits

- **Single instance.** Nonces and rate limit state are in memory. With more than one instance both need to move to Redis or Postgres, otherwise a replay goes through the other instance.
- **Behind a proxy** `RemoteAddr` is the proxy's and the rate limit becomes global. Then you need `X-Forwarded-For`, and only from a trusted proxy.
- **Synchronous forward.** The sender waits for the retries (up to `TIMEOUT`). If the target can be down for long, the right move is to store, answer 202 and deliver later through an outbox.
- **Single secret**, no rotation. Rotation means accepting two secrets during the switch.

## Running it

```bash
docker compose up -d --build
```

Starts the relay on port 8080 and a demo target that just echoes what it got to its log. Sending a signed webhook:

```bash
S=troque-este-segredo-de-demo-com-32-chars
TS=$(date +%s); N=evt-$RANDOM; B='{"order":42,"status":"paid"}'
SIG=$(printf '%s' "$TS.$N.$B" | openssl dgst -sha256 -hmac "$S" -hex | awk '{print $2}')
curl -i -X POST localhost:8080/webhook -H 'Content-Type: application/json' \
  -H "X-Webhook-Timestamp: $TS" -H "X-Webhook-Nonce: $N" -H "X-Webhook-Signature: sha256=$SIG" -d "$B"
docker compose logs destino
```

Running the same `curl` again gives 409. With a wrong signature, 401.

Tests:

```bash
go test -race -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1
```

| Variable | Default | |
|---|---|---|
| `WEBHOOK_SECRET` | required | at least 32 characters |
| `TARGET_URL` | required | where to forward |
| `ADDR` | `:8080` | |
| `RATE` / `BURST` | `10` / `20` | requests per second per IP / burst |
| `MAX_ATTEMPTS` | `4` | forward attempts |
| `TIMEOUT` | `10s` | total forward time, retries included |
| `WINDOW` | `5m` | timestamp window |

## Layout

```
cmd/relay          config, HTTP server and shutdown
internal/relay
  signature.go     HMAC-SHA256 and constant-time comparison
  replay.go        timestamp window and nonce
  ratelimit.go     per-IP token bucket
  forward.go       forward with retries
  handler.go       order of the checks
```
