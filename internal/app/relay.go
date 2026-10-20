package app

import (
	"context"
	"errors"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

var (
	ErrRateLimited  = errors.New("muitas requisições")
	ErrBadSignature = errors.New("assinatura inválida")
)

// ForwardError é o repasse q falhou depois dos retries. Quando chega aqui o nonce já foi liberado.
type ForwardError struct{ Err error }

func (e *ForwardError) Error() string { return e.Err.Error() }
func (e *ForwardError) Unwrap() error { return e.Err }

// RelayWebhook junta tudo. A ordem importa: limite antes de ler o corpo
// (barato), assinatura antes do nonce (senão lixo enche a memória).
type RelayWebhook struct {
	Secret    []byte
	Replay    domain.ReplayPolicy
	Nonces    NonceStore
	Limiter   Limiter
	Forwarder Forwarder
	Clock     Clock
	Timeout   time.Duration // tempo total do repasse, retries incluídos
}

// Relay só chama read depois do rate limit. Erro de read volta do jeito q veio.
func (uc *RelayWebhook) Relay(ctx context.Context, client string, read func() (domain.SignedWebhook, error)) error {
	if !uc.Limiter.Allow(client, uc.Clock()) {
		return ErrRateLimited
	}
	w, err := read()
	if err != nil {
		return err
	}
	if !domain.Verify(uc.Secret, w.Timestamp, w.Nonce, w.Body, w.Signature) {
		return ErrBadSignature
	}
	now := uc.Clock()
	keepUntil, err := uc.Replay.Check(w.Timestamp, now)
	if err != nil {
		return err
	}
	if !uc.Nonces.Reserve(w.Nonce, keepUntil, now) {
		return domain.ErrReplay
	}

	ctx, cancel := context.WithTimeout(ctx, uc.Timeout)
	defer cancel()
	if err := uc.Forwarder.Forward(ctx, w.Nonce, w.ContentType, w.Body); err != nil {
		// o remetente vai tentar de novo com o mesmo nonce; preso, o retry legítimo levava 409
		uc.Nonces.Release(w.Nonce)
		return &ForwardError{Err: err}
	}
	return nil
}
