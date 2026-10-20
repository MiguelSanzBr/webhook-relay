# webhook-relay

[![ci](https://github.com/MiguelSanzBr/webhook-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/MiguelSanzBr/webhook-relay/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/MiguelSanzBr/webhook-relay)](go.mod)
[![cobertura](https://img.shields.io/badge/cobertura-100%25-brightgreen)](.github/workflows/ci.yml)
[![licença](https://img.shields.io/github/license/MiguelSanzBr/webhook-relay)](LICENSE)

Serviço em Go que recebe webhook, confere se ele é de quem diz ser, barra replay e excesso de requisição, e só então repassa pro destino com retry, assinado de novo pelo próprio relay. Zero dependência fora da stdlib.

Fiz esse repo por causa de um incidente real: um webhook que podia ser forjado. Aqui está o jeito certo de fechar isso, pequeno o suficiente pra ler inteiro numa entrevista.

**Em 30 segundos:** HMAC-SHA256 sobre timestamp, nonce e corpo, comparado em tempo constante. Janela de 5 min e nonce usado uma vez só. Rate limit por IP (por /64 no IPv6) antes de ler o corpo. Repasse com retry só quando adianta, prazo por tentativa, sem seguir redirect e com a assinatura do relay. Hexagonal com a regra de dependência testada, TDD no histórico, 100% de cobertura com gate no CI, `govulncheck` e smoke test com Docker no CI.

[English version](README.en.md)

## Rodar

```bash
export WEBHOOK_SECRET=$(openssl rand -hex 32) FORWARD_SECRET=$(openssl rand -hex 32)
docker compose up -d --build --wait && go run ./examples/sender
```

Sobe o relay em `127.0.0.1:8080` e um destino de demo que só ecoa no log o que recebeu, e manda um webhook assinado com o remetente em Go. Responde `200 OK {"status":"repassado"}`. Com `NONCE=evt_1 go run ./examples/sender` duas vezes, a segunda dá 409. O que chegou no destino, já com a assinatura do relay: `docker compose logs destino`.

## Como funciona

```
POST /webhook
  │
  ├─ rate limit por IP, /64 no IPv6 (token bucket)  → 429 + Retry-After
  ├─ corpo até 1 MiB                                → 413
  ├─ X-Webhook-Timestamp / Nonce / Signature?       → 401
  ├─ nonce [A-Za-z0-9_-]{1,128} e HMAC-SHA256       → 401
  ├─ timestamp na janela de 5 min                   → 401
  ├─ nonce inédito                                  → 409
  ├─ store de nonce ou de balde fora                → 503
  └─ repassa pro TARGET_URL, assinado com FORWARD_SECRET, com retry
        ├─ 2xx                        → 200
        └─ falhou depois dos retries  → 502 e libera o nonce
```

Quem manda o webhook assina assim:

```
X-Webhook-Timestamp: 1760970000          (unix, segundos)
X-Webhook-Nonce:     evt_8f2c...         (único por evento, só A-Z a-z 0-9 _ -, até 128)
X-Webhook-Signature: sha256=hex(HMAC_SHA256(segredo, timestamp + "." + nonce + "." + corpo))
```

Em Go, com o pacote [`signature`](signature/signature.go) (é o que o [`examples/sender`](examples/sender/main.go) usa):

```go
body := []byte(`{"pedido":42}`)
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://relay.exemplo/webhook", bytes.NewReader(body))
req.Header.Set("Content-Type", "application/json")
if err := signature.SetHeaders(req.Header, secret, "evt_42", body, time.Now()); err != nil {
	return err // nonce fora do formato
}
resp, err := http.DefaultClient.Do(req)
```

O repasse chega no destino com os mesmos três headers, assinados com o `FORWARD_SECRET`, e com o nonce no `Idempotency-Key`. O destino confere com `signature.Verify(forwardSecret, ts, nonce, corpo, sig)`.

Na mão, com openssl:

```bash
TS=$(date +%s); N=evt_$RANDOM; B='{"pedido":42,"status":"pago"}'
SIG=$(printf '%s' "$TS.$N.$B" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | awk '{print $2}')
curl -i -X POST localhost:8080/webhook -H 'Content-Type: application/json' \
  -H "X-Webhook-Timestamp: $TS" -H "X-Webhook-Nonce: $N" -H "X-Webhook-Signature: sha256=$SIG" -d "$B"
```

## Decisões e o porquê

- **Timestamp e nonce entram no HMAC.** Se só o corpo fosse assinado, dava pra pegar um webhook válido e reenviar com outro nonce e outro timestamp. Assinando os três juntos, mexer em qualquer um quebra a assinatura.
- **Nonce só com `A-Z a-z 0-9 _ -`, até 128.** Uma revisão de segurança achou o furo: com ponto no nonce, `"ts.nonce.corpo"` dividia de mais de um jeito. Dava pra mover o começo do corpo pro nonce (`evt_1.{"valor":10` + `50}`) e a mesma assinatura valia com outro nonce, ou seja, replay passando. Sem ponto no nonce e com o timestamp inteiro, a concatenação só se divide de um jeito. O cenário da prova virou teste de regressão.
- **`hmac.Equal`, não `==`.** Comparar com `==` para no primeiro byte diferente, e o tempo de resposta vaza quantos bytes da assinatura já estão certos. `hmac.Equal` leva o mesmo tempo sempre.
- **Assinatura antes do nonce.** O nonce fica guardado em memória. Se eu guardasse antes de conferir a assinatura, qualquer um enchia a memória mandando nonce inventado.
- **Rate limit antes de ler o corpo, por /64 no IPv6.** É a checagem mais barata, então vem primeiro. Por endereço exato, quem tem um /64 (o padrão de qualquer VPS) trocava de IP a cada requisição e não tinha limite nenhum. O mapa de baldes tem teto de 100 mil; cheio, cliente novo leva 429 até a próxima limpeza. O `Retry-After` sai do `RATE`, não é 1 fixo.
- **Prazo pra tudo no servidor.** Só com `ReadHeaderTimeout`, mandar 1 byte a cada 2 s segurava a conexão pra sempre (slowloris) e keep-alive parado nunca fechava. Agora tem `ReadTimeout` de 10 s, `WriteTimeout` do tamanho da leitura mais o `TIMEOUT` inteiro (a resposta só sai depois do repasse), `IdleTimeout` e `MaxHeaderBytes`.
- **O relay se autentica no destino.** O repasse sai assinado de novo com um `FORWARD_SECRET` próprio, no mesmo formato da entrada. Sem isso, quem alcançasse o `TARGET_URL` direto pulava assinatura, janela, nonce e rate limit. O `FORWARD_SECRET` tem que ser diferente do de entrada, senão o webhook original já passava no destino.
- **Repasse não segue redirect.** O `http.Client` padrão segue: um 307 levava o corpo pra outro host e um 302 virava GET sem corpo contado como entregue. Agora 3xx é falha.
- **Prazo por tentativa.** Com só o prazo total, um destino que aceita a conexão e trava gastava o `TIMEOUT` inteiro na primeira tentativa e o retry nunca acontecia. Cada tentativa tem `TIMEOUT/MAX_ATTEMPTS`.
- **Repasse falhou, nonce liberado.** O remetente vai tentar de novo com o mesmo evento. Se o nonce ficasse preso, o retry legítimo levava 409 e o evento se perdia. A liberação acontece mesmo se o remetente já desistiu.
- **Nonce vira `Idempotency-Key` no repasse.** O relay também faz retry, então o destino pode receber o mesmo evento duas vezes. Com a chave ele descarta a cópia. É o mesmo raciocínio do [outbox-gateway-demo](https://github.com/MiguelSanzBr/outbox-gateway-demo).
- **Retry só quando adianta:** rede, timeout, 429 e 5xx. Outro 4xx é erro do pedido, repetir não muda nada. Backoff exponencial com jitter pra os retries não baterem todos juntos.
- **Store fora fecha.** Os ports devolvem erro. Se o store de nonce cair, o relay não sabe se o nonce é novo, então responde 503 e não repassa nada. Fail-open aqui seria replay liberado.
- **Rotação de segredo.** `WEBHOOK_SECRET` aceita mais de um, separado por vírgula. Durante a troca valem o velho e o novo.
- **Config ruim derruba a subida.** Segredo com menos de 32 caracteres, `RATE=0`, `TIMEOUT=10` (sem unidade) ou `TARGET_URL=ftp://...` param o processo com erro, em vez de cair num padrão em silêncio.
- **Dá pra ver o que acontece.** Cada request tem um `X-Request-Id` (o de fora se for curto e limpo, senão o relay cria um) que vai em toda linha de log, uma linha por request com status e duração, e `/debug/vars` (expvar) conta as respostas por rota e status. Erro de rede vai pro log sem a URL do destino, que pode ter token na query.

## Arquitetura

Hexagonal, com o domínio no meio. Seta é import, sempre de fora pra dentro.

```
                         cmd/relay
          (config, monta as peças, servidor, -health)
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
 adapters/httpapi      adapters/memory     adapters/httpforward
 (entrada HTTP, id,    (nonce, baldes)     (repasse assinado,
  log, métricas)                            retry)
        │                     │ implementa          │ implementa
        │                     ▼                     ▼
        └────────────▶ app: RelayWebhook + ports
                       NonceStore, Limiter, Forwarder, Clock
                              │
                              ▼
                       domain: SignedWebhook, Sign/Verify,
                       ReplayPolicy, TokenBucket, RetryPolicy
                              ▲
                              │
   examples/sender ───▶ signature (público, reexporta o domínio)
```

**Por que hexagonal aqui.** O limite mais óbvio do relay é ser uma instância só, porque nonce e balde ficam em memória. Os ports já recebem `ctx` e devolvem erro, então um adapter de Redis consegue respeitar cancelamento e dizer "caí", e o caso de uso fecha com 503. Ir pro Redis é escrever esse adapter (o `Reserve` atômico com `SET NX` e expiração, o balde num script Lua), e caso de uso, domínio e os testes deles não mudam. Trocar o repasse síncrono por uma outbox é outro `Forwarder`. E a ordem das checagens, que é onde mora a segurança, fica num lugar só e é testada sem subir HTTP.

Sem cerimônia: um caso de uso, quatro ports, três adapters. O handler usa o `RelayWebhook` direto, sem interface pra algo que só tem uma implementação. O pacote `signature` só reexporta o domínio pra quem é de fora do módulo conseguir importar. A regra de dependência não fica só no README: `TestDependencyRule` lê os imports de `internal/domain` e `internal/app` com `go/parser` e falha se aparecer `net/http`, adapter ou qualquer pacote fora da lista.

| Garantia | Onde mora |
|---|---|
| HMAC de `ts.nonce.corpo`, formato do nonce e comparação com `hmac.Equal` | `domain/signature.go` |
| Janela de 5 min, timestamp inválido separado de velho, até quando lembrar do nonce | `domain/replay.go` (`ReplayPolicy`) |
| Conta do token bucket, teto na rajada, tempo até a próxima ficha | `domain/ratelimit.go` (`TokenBucket`) |
| 429 e 5xx repetem, outro 4xx não, backoff com jitter, prazo por tentativa | `domain/retry.go` (`RetryPolicy`) |
| Ordem limite, corpo, assinatura, nonce, repasse; store fora fecha; nonce liberado se o repasse falha; prazo total; rotação de segredo | `app/relay.go` (`RelayWebhook`) |
| Nonce e balde em memória, com limpeza e teto de baldes | `adapters/memory` |
| Repasse assinado, nonce como `Idempotency-Key`, sem redirect, erro de rede sem URL | `adapters/httpforward` |
| Corpo até 1 MiB, headers obrigatórios, cliente por /64, erro vira 401/409/413/429/502/503, request id, log, métricas | `adapters/httpapi` |
| Config validada, prazos do servidor, shutdown limpo, `-health` | `cmd/relay` |
| Assinar e conferir de fora do módulo | `signature` |

## Testes

TDD de verdade: cada parte entrou com o teste primeiro, falhando, e o código depois. Dá pra ver no histórico, os commits `Teste: ...` vêm antes do código de cada parte. A ida pra hexagonal foi em commits `Refatoração: ...` pequenos, com tudo verde antes e depois. As correções das duas revisões (segurança e design) seguiram o mesmo caminho: cada furo virou primeiro um teste que falhava.

| Garantia | Teste |
|---|---|
| Corpo, timestamp, nonce ou segredo alterado invalida a assinatura; nonce deslocado pelo ponto é recusado | `TestVerify`, `TestDotShiftReplayIsRejected` |
| Limite antes de ler o corpo, nonce só guardado depois da assinatura, repasse com prazo e uma vez só, rotação de segredo | `TestRelayRejects`, `TestRelayForwardsOnce`, `TestRelayAcceptsAnyActiveSecret` |
| Store fora fecha sem repassar; o ctx do remetente chega no repasse e o `Release` não herda o cancelamento | `TestStoreDownFailsClosed`, `TestStoreDownIs503`, `TestCallerContextReachesForward` |
| Corpo trocado, sem header, timestamp de 6 min ou inválido, corpo > 1 MiB e conexão caindo no meio: nada chega no destino | `TestWebhookChecks` |
| O destino recebe o corpo, o `Content-Type`, o `Idempotency-Key` e a assinatura do relay, não a do remetente | `TestForwardedRequest`, `TestForwardSendsBodyAndIdempotencyKey` |
| Destino fora devolve 502 e o retry do remetente com o mesmo nonce passa quando o destino volta | `TestFailedForwardLetsSenderRetry`, `TestFailedForwardReleasesNonce` |
| Rajada e reposição, um balde por IPv4 e por /64, `Retry-After` pelo `RATE`, teto de baldes | `TestTokenBucket`, `TestRateLimitPerClient`, `TestRateLimitPerIP`, `TestBucketsCap` |
| Limpeza só tira o que venceu: nonce vivo e balde meio vazio continuam | `TestSweepKeepsLiveNonces`, `TestSweepKeepsPartialBuckets` |
| N goroutines no mesmo nonce: um passa; no mesmo balde: só a rajada (é o que dá trabalho pro `-race`) | `TestConcurrentReserveOnlyOneWins`, `TestConcurrentAllowRespectsBurst` |
| Retry em 503/429/500, nenhum em 400, sem seguir redirect, retry com destino travado, para quando o cliente desiste | `TestForwardRetries`, `TestForwardDoesNotFollowRedirects`, `TestForwardRetriesHungTarget`, `TestForwardStopsWhenContextEnds` |
| Request id no header e no log, contagem por status | `TestRequestIDGoesToLog`, `TestStatusMetrics` |
| Config inválida derruba a subida, a montagem do `main` liga `RATE`/`BURST`/`WINDOW` certo, servidor com todos os prazos, `-health`, `SIGINT` desliga limpo | `TestLoadConfig`, `TestWiringFromEnv`, `TestServerHasDeadlines`, `TestHealthFlag`, `TestMainGracefulShutdown` |
| O remetente de exemplo contra o relay de verdade: 200, depois 409 com o mesmo nonce | `TestSenderAgainstRelay`, `ExampleSetHeaders` |
| Domínio e aplicação sem `net/http`, sem adapter, só o permitido | `TestDependencyRule` |

49 testes e 32 subtestes, 100% de cobertura das instruções com gate no CI, `-race` em tudo. Relógio injetado, então os testes de janela e de rate limit não usam `sleep`; 15 rodadas seguidas com `-race` sem nenhum flake.

Cobertura de 100% não diz que os testes protegem, então quebro de propósito numa cópia e vejo se algum teste falha. Na primeira versão foram três quebras e as três pegaram. Uma revisão depois fez 16 e 10 passaram calados (limpeza que apagava nonce vivo, mutex removido, 3xx contado como sucesso, `RATE` trocado com `BURST`...). Escrevi os testes que faltavam e refiz com 30 quebras, incluindo as novas defesas: 29 pegas. A que passa é o backoff sem a parte aleatória do jitter, e essa eu aceito: testar aleatoriedade sem virar flake custa mais do que vale.

```bash
go test -race -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1
```

## Limites conhecidos

- **Uma instância só.** Nonce e rate limit ficam em memória. Com mais de uma instância, os dois têm que ir pro Redis ou Postgres, senão o replay passa pela outra instância.
- **Atrás de proxy** o `RemoteAddr` é o do proxy e o rate limit vira global. Aí precisa ler `X-Forwarded-For`, e só de um proxy confiável. O relay fala HTTP puro: o TLS fica no proxy da frente.
- **Repasse síncrono.** O remetente fica esperando os retries (até `TIMEOUT`). Se o destino pode ficar fora por muito tempo, o certo é gravar e responder 202, e entregar depois com uma outbox.
- **Entrega pelo menos uma vez.** Se o remetente desiste no meio, o repasse para e o nonce é liberado, mesmo que o destino já tenha recebido; o retry dele entrega de novo e o `Idempotency-Key` segura a cópia. E um retry que chega enquanto o primeiro ainda está em voo leva 409, igual a "já entregue".
- **`Content-Type` fora da assinatura.** Ele é repassado como veio. Com o formato do nonce, só quem tem o segredo monta um webhook aceito, e com TLS ninguém troca no caminho, mas o destino não deve confiar só nele pra escolher o parser.
- **`/debug/vars` é público na porta do relay.** Mostra contagem de status e memória do processo, nada de segredo. Em produção, bloquear no proxy.
- **Teto de 100 mil baldes.** Com mais clientes distintos que isso dentro de uma reposição, cliente novo leva 429 até a próxima limpeza. Não tem limite global de conexões simultâneas além dos prazos do servidor.
- **Shutdown espera até 25 s** (leitura + `TIMEOUT` + folga). O grace period do orquestrador tem que ser maior; no compose está em 30 s.

## Configuração

| Variável | Padrão | |
|---|---|---|
| `WEBHOOK_SECRET` | obrigatório | pelo menos 32 caracteres; vários separados por vírgula pra rotação |
| `FORWARD_SECRET` | obrigatório | pelo menos 32 caracteres, diferente do de entrada; assina o repasse |
| `TARGET_URL` | obrigatório | `http://` ou `https://` com host, pra onde repassar |
| `ADDR` | `:8080` | |
| `RATE` / `BURST` | `10` / `20` | requisições por segundo por IP / rajada |
| `MAX_ATTEMPTS` | `4` | tentativas de repasse, cada uma com `TIMEOUT/MAX_ATTEMPTS` |
| `TIMEOUT` | `10s` | tempo total do repasse, retries incluídos |
| `WINDOW` | `5m` | janela do timestamp |

Rotas: `POST /webhook`, `GET /healthz` (204), `GET /debug/vars` (expvar). `relay -health` bate no próprio `/healthz` e é o healthcheck da imagem, que é distroless e não tem shell nem curl.

## Estrutura

```
cmd/relay              config, monta as peças, servidor com prazos, shutdown e -health
internal/domain        regras sem HTTP e sem I/O
  signature.go         SignedWebhook, formato do nonce, HMAC-SHA256 e comparação em tempo constante
  replay.go            janela do timestamp
  ratelimit.go         conta do token bucket
  retry.go             quando repetir, backoff e prazo por tentativa
internal/app
  ports.go             NonceStore, Limiter, Forwarder, Clock
  relay.go             caso de uso RelayWebhook, ordem das checagens
internal/adapters
  memory               nonce e balde de rate limit em memória, com limpeza e teto
  httpforward          repasse HTTP assinado, com retry e sem redirect
  httpapi              handler, limite do corpo, erro pra status, request id, log, métricas
internal/deps_test.go  regra de dependência
signature              assinar e conferir de fora do módulo
examples/sender        remetente em Go
```
