# webhook-relay

Serviço em Go que recebe webhook, confere se ele é de quem diz ser, barra replay e excesso de requisição, e só então repassa pro destino com retry. Zero dependência fora da stdlib.

Fiz esse repo por causa de um incidente real: um webhook que podia ser forjado. Aqui tá o jeito certo de fechar isso, pequeno o suficiente pra ler inteiro numa entrevista.

[English version](README.en.md)

## Como funciona

```
POST /webhook
  │
  ├─ rate limit por IP (token bucket)          → 429 + Retry-After
  ├─ corpo até 1 MiB                           → 413
  ├─ X-Webhook-Timestamp / Nonce / Signature?  → 401
  ├─ HMAC-SHA256 de "ts.nonce.corpo"           → 401
  ├─ timestamp na janela de 5 min              → 401
  ├─ nonce inédito                             → 409
  └─ repassa pro TARGET_URL com retry
        ├─ 2xx                        → 200
        └─ falhou depois dos retries  → 502 e libera o nonce
```

Quem manda o webhook assina assim:

```
X-Webhook-Timestamp: 1760970000          (unix, segundos)
X-Webhook-Nonce:     evt_8f2c...         (único por evento)
X-Webhook-Signature: sha256=hex(HMAC_SHA256(segredo, timestamp + "." + nonce + "." + corpo))
```

## Decisões e o porquê

- **Timestamp e nonce entram no HMAC.** Se só o corpo fosse assinado, dava pra pegar um webhook válido e reenviar com outro nonce e outro timestamp. Assinando os três juntos, mexer em qualquer um quebra a assinatura.
- **`hmac.Equal`, não `==`.** Comparar com `==` para no primeiro byte diferente, e o tempo de resposta vaza quantos bytes da assinatura já estão certos. `hmac.Equal` leva o mesmo tempo sempre.
- **Assinatura antes do nonce.** O nonce fica guardado em memória. Se eu guardasse antes de conferir a assinatura, qualquer um enchia a memória mandando nonce inventado.
- **Rate limit antes de ler o corpo.** É a checagem mais barata, então vem primeiro. Cliente barulhento não chega a gastar leitura de 1 MiB.
- **Repasse falhou, nonce liberado.** O remetente vai tentar de novo com o mesmo evento. Se o nonce ficasse preso, o retry legítimo levava 409 e o evento se perdia.
- **Nonce vira `Idempotency-Key` no repasse.** O relay também faz retry, então o destino pode receber o mesmo evento duas vezes. Com a chave ele descarta a cópia. É o mesmo raciocínio do [outbox-gateway-demo](https://github.com/MiguelSanzBr/outbox-gateway-demo).
- **Retry só quando adianta:** rede, timeout, 429 e 5xx. Outro 4xx é erro do pedido, repetir não muda nada. Backoff exponencial com jitter pra os retries não baterem todos juntos.
- **Config ruim derruba a subida.** Segredo com menos de 32 caracteres, `RATE=0` ou `TIMEOUT=10` (sem unidade) param o processo com erro, em vez de cair num padrão em silêncio.

## Arquitetura

Hexagonal, com o domínio no meio. Seta é import, sempre de fora pra dentro.

```
                         cmd/relay
              (config, monta as peças, servidor)
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
 adapters/httpapi      adapters/memory     adapters/httpforward
 (entrada HTTP)        (nonce, baldes)     (repasse HTTP, retry)
        │                     │ implementa          │ implementa
        │                     ▼                     ▼
        └────────────▶ app: RelayWebhook + ports
                       NonceStore, Limiter, Forwarder, Clock
                              │
                              ▼
                       domain: SignedWebhook, Sign/Verify,
                       ReplayPolicy, TokenBucket, RetryPolicy
```

**Por que hexagonal aqui.** O limite mais óbvio do relay é ser uma instância só, pq nonce e balde ficam em memória. Com os ports, ir pro Redis é escrever outro adapter de `NonceStore` e `Limiter`, e caso de uso, domínio e os testes deles não mudam. Trocar o repasse síncrono por uma outbox é outro `Forwarder`. E a ordem das checagens, q é onde mora a segurança, fica num lugar só e é testada sem subir HTTP.

Sem cerimônia: um caso de uso, quatro ports, três adapters. O handler usa o `RelayWebhook` direto, sem interface pra algo q só tem uma implementação. A regra de dependência não fica só no README: `TestDependencyRule` lê os imports de `internal/domain` e `internal/app` com `go/parser` e falha se aparecer `net/http`, adapter ou qualquer pacote fora da lista.

| Garantia | Onde mora |
|---|---|
| HMAC de `ts.nonce.corpo` e comparação com `hmac.Equal` | `domain/signature.go` |
| Janela de 5 min e até quando lembrar do nonce | `domain/replay.go` (`ReplayPolicy`) |
| Conta do token bucket, teto na rajada | `domain/ratelimit.go` (`TokenBucket`) |
| 429 e 5xx repetem, outro 4xx não, backoff com jitter | `domain/retry.go` (`RetryPolicy`) |
| Ordem limite, corpo, assinatura, nonce, repasse; nonce liberado se o repasse falha; prazo total do repasse | `app/relay.go` (`RelayWebhook`) |
| Nonce e balde em memória, com limpeza | `adapters/memory` |
| Nonce como `Idempotency-Key`, erro de rede conta como temporário | `adapters/httpforward` |
| Corpo até 1 MiB, headers obrigatórios, erro vira 401/409/413/429/502 | `adapters/httpapi` |
| Config validada, shutdown limpo | `cmd/relay` |

## Testes

TDD de verdade: cada parte entrou com o teste primeiro, falhando, e o código depois. Dá pra ver no histórico, os commits `Teste: ...` vêm antes do código de cada parte. A ida pra hexagonal foi em commits `Refatoração: ...` pequenos, com tudo verde antes e depois, e o q era unidade nova (caso de uso, regra de dependência) também entrou com o teste primeiro.

| Garantia | Teste |
|---|---|
| Corpo, timestamp, nonce ou segredo alterado invalida a assinatura | `TestVerify` |
| Limite antes de ler o corpo, nonce só guardado depois da assinatura, repasse com prazo e uma vez só | `TestRelayRejects`, `TestRelayForwardsOnce` |
| Corpo trocado depois de assinar, sem header, timestamp de 6 min, mesmo webhook duas vezes, corpo > 1 MiB e conexão caindo no meio da leitura: nada disso chega no destino | `TestWebhookChecks` |
| Destino fora devolve 502 e o retry do remetente com o mesmo nonce passa quando o destino volta | `TestFailedForwardLetsSenderRetry`, `TestFailedForwardReleasesNonce` |
| Rajada e reposição do rate limit, cada IP com seu balde, teto não acumula | `TestTokenBucket`, `TestRateLimiterBurstAndRefill` |
| Nonce e balde velhos saem da memória | `TestExpiredNoncesAreSwept`, `TestIdleBucketsAreSwept` |
| Retry em 503/429/500, nenhum retry em 400, para na hora quando o cliente desiste | `TestRetryPolicy`, `TestForwardRetries`, `TestForwardStopsWhenContextEnds` |
| Domínio e aplicação sem `net/http`, sem adapter, só o permitido | `TestDependencyRule` |
| Config inválida derruba a subida; `SIGINT` desliga limpo | `TestLoadConfig`, `TestMainGracefulShutdown` |

Cobertura de 100% das instruções, com gate no CI. Relógio injetado, então os testes de janela e de rate limit não usam `sleep`.

Pra conferir que os testes protegem de verdade e não só cobrem linha, quebrei de propósito três coisas: tirei a checagem de assinatura, tirei a liberação do nonce e tirei timestamp e nonce do HMAC. Nos três casos os testes falharam.

## Limites conhecidos

- **Uma instância só.** Nonce e rate limit ficam em memória. Com mais de uma instância, os dois têm q ir pro Redis ou Postgres, senão o replay passa pela outra instância.
- **Atrás de proxy** o `RemoteAddr` é o do proxy e o rate limit vira global. Aí precisa ler `X-Forwarded-For`, e só de um proxy confiável.
- **Repasse síncrono.** O remetente fica esperando os retries (até `TIMEOUT`). Se o destino pode ficar fora por muito tempo, o certo é gravar e responder 202, e entregar depois com uma outbox.
- **Um segredo só**, sem rotação. Rotação pede aceitar dois segredos durante a troca.

## Como rodar

```bash
docker compose up -d --build
```

Sobe o relay na porta 8080 e um destino de demo que só ecoa no log o que recebeu. Mandando um webhook assinado:

```bash
S=troque-este-segredo-de-demo-com-32-chars
TS=$(date +%s); N=evt-$RANDOM; B='{"pedido":42,"status":"pago"}'
SIG=$(printf '%s' "$TS.$N.$B" | openssl dgst -sha256 -hmac "$S" -hex | awk '{print $2}')
curl -i -X POST localhost:8080/webhook -H 'Content-Type: application/json' \
  -H "X-Webhook-Timestamp: $TS" -H "X-Webhook-Nonce: $N" -H "X-Webhook-Signature: sha256=$SIG" -d "$B"
docker compose logs destino
```

Rodando o mesmo `curl` de novo dá 409. Com a assinatura errada, 401.

Testes:

```bash
go test -race -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1
```

| Variável | Padrão | |
|---|---|---|
| `WEBHOOK_SECRET` | obrigatório | pelo menos 32 caracteres |
| `TARGET_URL` | obrigatório | pra onde repassar |
| `ADDR` | `:8080` | |
| `RATE` / `BURST` | `10` / `20` | requisições por segundo por IP / rajada |
| `MAX_ATTEMPTS` | `4` | tentativas de repasse |
| `TIMEOUT` | `10s` | tempo total do repasse, retries incluídos |
| `WINDOW` | `5m` | janela do timestamp |

## Estrutura

```
cmd/relay              config, monta as peças, servidor HTTP e shutdown
internal/domain        regras sem HTTP e sem I/O
  signature.go         SignedWebhook, HMAC-SHA256 e comparação em tempo constante
  replay.go            janela do timestamp
  ratelimit.go         conta do token bucket
  retry.go             quando repetir e backoff
internal/app
  ports.go             NonceStore, Limiter, Forwarder, Clock
  relay.go             caso de uso RelayWebhook, ordem das checagens
internal/adapters
  memory               nonce e balde de rate limit em memória, com limpeza
  httpforward          repasse HTTP com retry
  httpapi              handler, limite do corpo, erro pra status, healthz
internal/deps_test.go  regra de dependência
```
