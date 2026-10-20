// Package app tem o caso de uso do relay e os ports q ele precisa. Quem
// implementa os ports são os adapters; aqui não entra HTTP nem memória.
package app

import (
	"context"
	"time"
)

// NonceStore guarda os nonces já vistos. Reserve devolve false se o nonce já
// estava lá; keepUntil diz quando ele pode sair.
type NonceStore interface {
	Reserve(nonce string, keepUntil, now time.Time) bool
	Release(nonce string)
}

// Limiter é o rate limit por cliente.
type Limiter interface {
	Allow(client string, now time.Time) bool
}

// Forwarder entrega o webhook no destino, com os retries q achar certo dentro do prazo do ctx.
type Forwarder interface {
	Forward(ctx context.Context, nonce, contentType string, body []byte) error
}

// Clock é injetado pra os testes de janela e de rate limit não dependerem de sleep.
type Clock func() time.Time
