package memory

import (
	"sync"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// Buckets é o app.Limiter em memória: um token bucket por cliente (o IP).
type Buckets struct {
	policy domain.TokenBucket

	mu        sync.Mutex
	buckets   map[string]*domain.Bucket
	lastSweep time.Time
}

func NewBuckets(policy domain.TokenBucket) *Buckets {
	return &Buckets{policy: policy, buckets: map[string]*domain.Bucket{}}
}

func (l *Buckets) Allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

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
	if !ok {
		nb := l.policy.New(now)
		b = &nb
		l.buckets[client] = b
	}
	return l.policy.Take(b, now)
}
