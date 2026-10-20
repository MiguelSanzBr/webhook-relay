// Package app tem o caso de uso do relay e os ports q ele precisa. Quem
// implementa os ports são os adapters; aqui não entra HTTP nem memória.
package app

import (
	"context"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// NonceStore guarda os nonces já vistos. Reserve devolve false se o nonce já
// estava lá; keepUntil diz quando ele pode sair. Erro é store fora (Redis caiu,
// por ex.) e o caso de uso fecha: sem saber se o nonce é novo, não repassa.
type NonceStore interface {
	Reserve(ctx context.Context, nonce string, keepUntil time.Time) (bool, error)
	Release(ctx context.Context, nonce string) error
}

// Limiter é o rate limit por cliente. Erro também fecha.
type Limiter interface {
	Allow(ctx context.Context, client string) (bool, error)
}

// Forwarder entrega o webhook no destino, com os retries q achar certo dentro do prazo do ctx.
type Forwarder interface {
	Forward(ctx context.Context, w domain.SignedWebhook) error
}

// Clock é injetado pra os testes de janela e de rate limit não dependerem de sleep.
type Clock func() time.Time
