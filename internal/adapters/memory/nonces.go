// Package memory guarda nonces e baldes do rate limit no próprio processo.
// Vale pra uma instância só. Com mais de uma os dois têm q ir pro Redis ou Postgres.
package memory

import (
	"context"
	"sync"
	"time"
)

// Nonces é o app.NonceStore em memória. Nunca devolve erro, memória não cai.
type Nonces struct {
	sweepEvery time.Duration
	now        func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time // nonce -> quando pode sair da memória
	lastSweep time.Time
}

// NewNonces varre os nonces vencidos no máximo uma vez por janela.
func NewNonces(window time.Duration, now func() time.Time) *Nonces {
	return &Nonces{sweepEvery: window, now: now, seen: map[string]time.Time{}}
}

func (s *Nonces) Reserve(_ context.Context, nonce string, keepUntil time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastSweep) >= s.sweepEvery {
		for n, exp := range s.seen {
			if now.After(exp) {
				delete(s.seen, n)
			}
		}
		s.lastSweep = now
	}
	if _, dup := s.seen[nonce]; dup {
		return false, nil
	}
	s.seen[nonce] = keepUntil
	return true, nil
}

func (s *Nonces) Release(_ context.Context, nonce string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, nonce)
	return nil
}
