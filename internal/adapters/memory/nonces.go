// Package memory guarda nonces e baldes do rate limit no próprio processo.
// Vale pra uma instância só. Com mais de uma os dois têm q ir pro Redis ou Postgres.
package memory

import (
	"sync"
	"time"
)

// Nonces é o app.NonceStore em memória.
type Nonces struct {
	sweepEvery time.Duration

	mu        sync.Mutex
	seen      map[string]time.Time // nonce -> quando pode sair da memória
	lastSweep time.Time
}

// NewNonces varre os nonces vencidos no máximo uma vez por janela.
func NewNonces(window time.Duration) *Nonces {
	return &Nonces{sweepEvery: window, seen: map[string]time.Time{}}
}

func (s *Nonces) Reserve(nonce string, keepUntil, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastSweep) >= s.sweepEvery {
		for n, exp := range s.seen {
			if now.After(exp) {
				delete(s.seen, n)
			}
		}
		s.lastSweep = now
	}
	if _, dup := s.seen[nonce]; dup {
		return false
	}
	s.seen[nonce] = keepUntil
	return true
}

func (s *Nonces) Release(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, nonce)
}
