package relay

import (
	"sync"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// ReplayGuard barra webhook repetido: timestamp tem q estar na janela e o nonce não pode ter aparecido antes.
// Fica tudo em memória, então vale pra uma instância só. Com mais de uma o nonce tem q ir pro Redis ou Postgres.
type ReplayGuard struct {
	policy domain.ReplayPolicy
	now    func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time // nonce -> quando pode sair da memória
	lastSweep time.Time
}

func NewReplayGuard(window time.Duration, now func() time.Time) *ReplayGuard {
	return &ReplayGuard{policy: domain.ReplayPolicy{Window: window}, now: now, seen: map[string]time.Time{}}
}

// Reserve confere a janela e guarda o nonce. Chamar depois de validar a
// assinatura, senão qualquer um enche a memória com nonce inventado.
func (g *ReplayGuard) Reserve(tsHeader, nonce string) error {
	now := g.now()
	keepUntil, err := g.policy.Check(tsHeader, now)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastSweep) >= g.policy.Window {
		for n, exp := range g.seen {
			if now.After(exp) {
				delete(g.seen, n)
			}
		}
		g.lastSweep = now
	}
	if _, dup := g.seen[nonce]; dup {
		return domain.ErrReplay
	}
	g.seen[nonce] = keepUntil
	return nil
}

// Release devolve o nonce quando o repasse falhou, pra o retry do remetente passar.
func (g *ReplayGuard) Release(nonce string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, nonce)
}
