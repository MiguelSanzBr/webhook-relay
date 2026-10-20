package domain

import (
	"math/rand/v2"
	"time"
)

// RetryPolicy decide se o repasse tenta de novo e quanto espera antes.
type RetryPolicy struct {
	MaxAttempts int
	BackoffBase time.Duration
	// AttemptTimeout é o prazo de cada tentativa. Sem ele, destino q aceita a conexão
	// e trava gasta o prazo total na primeira e o retry nunca acontece.
	AttemptTimeout time.Duration
}

// TemporaryStatus diz se repetir pode dar outra resposta: 429 e 5xx sim.
// Outro 4xx é erro do pedido e repetir não muda nada.
func TemporaryStatus(status int) bool {
	return status == 429 || status >= 500
}

// Again diz se vale outra tentativa depois da tentativa número attempt.
func (p RetryPolicy) Again(attempt int, temporary bool) bool {
	return temporary && attempt < p.MaxAttempts
}

// Backoff exponencial com jitter, metade fixa e metade aleatória, pra os retries não baterem todos juntos.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	d := p.BackoffBase << min(attempt-1, 20)
	return d/2 + rand.N(d/2+1)
}
