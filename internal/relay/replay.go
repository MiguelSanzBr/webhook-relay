package relay

import (
	"errors"
	"strconv"
	"sync"
	"time"
)

var (
	ErrStale  = errors.New("timestamp fora da janela")
	ErrReplay = errors.New("nonce já usado")
)

// ReplayGuard barra webhook repetido: timestamp tem q estar na janela e o nonce não pode ter aparecido antes.
// Fica tudo em memória, então vale pra uma instância só. Com mais de uma o nonce tem q ir pro Redis ou Postgres.
type ReplayGuard struct {
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time // nonce -> quando pode sair da memória
	lastSweep time.Time
}

func NewReplayGuard(window time.Duration, now func() time.Time) *ReplayGuard {
	return &ReplayGuard{window: window, now: now, seen: map[string]time.Time{}}
}

// Reserve confere a janela e guarda o nonce. Chamar depois de validar a
// assinatura, senão qualquer um enche a memória com nonce inventado.
func (g *ReplayGuard) Reserve(tsHeader, nonce string) error {
	sec, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return ErrStale
	}
	ts := time.Unix(sec, 0)
	now := g.now()
	if now.Sub(ts).Abs() > g.window {
		return ErrStale
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastSweep) >= g.window {
		for n, exp := range g.seen {
			if now.After(exp) {
				delete(g.seen, n)
			}
		}
		g.lastSweep = now
	}
	if _, dup := g.seen[nonce]; dup {
		return ErrReplay
	}
	// depois de ts+window o próprio timestamp já é recusado, não precisa lembrar do nonce
	g.seen[nonce] = ts.Add(g.window)
	return nil
}

// Release devolve o nonce quando o repasse falhou, pra o retry do remetente passar.
func (g *ReplayGuard) Release(nonce string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, nonce)
}
