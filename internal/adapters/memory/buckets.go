package memory

import (
	"context"
	"sync"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// Buckets é o app.Limiter em memória: um token bucket por cliente.
type Buckets struct {
	policy domain.TokenBucket
	now    func() time.Time
	max    int // teto de baldes; cheio, cliente novo é recusado até a próxima varredura

	mu        sync.Mutex
	buckets   map[string]*domain.Bucket
	lastSweep time.Time
}

func NewBuckets(policy domain.TokenBucket, now func() time.Time, maxClients int) *Buckets {
	return &Buckets{policy: policy, now: now, max: maxClients, buckets: map[string]*domain.Bucket{}}
}

func (l *Buckets) Allow(_ context.Context, client string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// balde q já encheu de novo é igual a um novo, então pode sair
	if now.Sub(l.lastSweep) >= l.policy.Refill() {
		for k, b := range l.buckets {
			if l.policy.Level(*b, now) >= l.policy.Burst {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[client]
	if !ok && len(l.buckets) >= l.max {
		return false, nil
	}
	if !ok {
		nb := l.policy.New(now)
		b = &nb
		l.buckets[client] = b
	}
	return l.policy.Take(b, now), nil
}
