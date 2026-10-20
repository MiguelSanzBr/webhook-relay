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
	ErrUnavailable  = errors.New("store fora, tente de novo")
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
	ok, err := uc.Limiter.Allow(ctx, client)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if !ok {
		return ErrRateLimited
	}
	w, err := read()
	if err != nil {
		return err
	}
	if !domain.Verify(uc.Secret, w.Timestamp, w.Nonce, w.Body, w.Signature) {
		return ErrBadSignature
	}
	keepUntil, err := uc.Replay.Check(w.Timestamp, uc.Clock())
	if err != nil {
		return err
	}
	ok, err = uc.Nonces.Reserve(ctx, w.Nonce, keepUntil)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if !ok {
		return domain.ErrReplay
	}

	fwdCtx, cancel := context.WithTimeout(ctx, uc.Timeout)
	defer cancel()
	if err := uc.Forwarder.Forward(fwdCtx, w); err != nil {
		// o remetente vai tentar de novo com o mesmo nonce; preso, o retry legítimo levava 409.
		// Libera mesmo se o remetente já desistiu, por isso sem o cancelamento do ctx dele.
		if rerr := uc.Nonces.Release(context.WithoutCancel(ctx), w.Nonce); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return &ForwardError{Err: err}
	}
	return nil
}
